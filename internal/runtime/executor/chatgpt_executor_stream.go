package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ExecuteStream streams a response from the public Responses endpoint, translating
// upstream SSE events into the format the client requested.
//
// The upstream request always asks for a stream: the endpoint reports the real
// completion payload on the terminal event, and streaming keeps the non-streaming
// path identical to this one.
func (e *ChatGPTExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated, body := translateCodexRequestPair(from, to, baseModel, originalPayload, req.Payload, true, helps.APIKeyModelIsCompat(req))

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = prepareChatGPTBody(body, baseModel, true)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, url, err := e.newChatGPTUpstreamRequest(ctx, auth, body, opts.Headers)
	if err != nil {
		return nil, err
	}
	e.recordChatGPTRequest(ctx, auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClientRoundTripOnly(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("chatgpt executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return nil, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("chatgpt executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte

		send := func(chunks [][]byte) bool {
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if !bytes.HasPrefix(line, dataTag) {
				if !send(helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, body, line, &param, claudeInputTokens)) {
					return
				}
				continue
			}

			data := bytes.TrimSpace(line[len(dataTag):])
			eventType := gjson.GetBytes(data, "type").String()

			if streamErr, ok := chatGPTTerminalFailure(data); ok {
				helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
				reporter.PublishFailure(ctx, streamErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return
			}

			terminalSuccess := false
			switch eventType {
			case "response.output_item.done":
				// The endpoint leaves response.completed.response.output empty, so
				// terminal items are collected here and patched in below.
				collectCodexOutputItemDone(data, outputItemsByIndex, &outputItemsFallback)
			case "response.completed", "response.incomplete", "response.done":
				if detail, ok := helps.ParseCodexUsage(data); ok {
					reporter.Publish(ctx, detail)
				} else {
					reporter.EnsurePublished(ctx)
				}
				data = patchCodexCompletedOutput(data, outputItemsByIndex, outputItemsFallback)
				terminalSuccess = true
			}

			if !send(helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, body, append([]byte("data: "), data...), &param, claudeInputTokens)) {
				return
			}
			if terminalSuccess {
				return
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			if ctx.Err() != nil {
				// A cancelled client request must not penalise the credential.
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		// The stream ended without a terminal event; reuse the shared request-scoped
		// incomplete-stream error so the conductor retries instead of treating the
		// truncated response as success.
		streamErr := newCodexIncompleteStreamError()
		helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
		reporter.PublishFailure(ctx, streamErr)
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
		case <-ctx.Done():
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}
