package executor

import (
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

// Execute performs a non-streaming request by streaming upstream and aggregating the
// terminal event, mirroring the Codex executor. The upstream request still asks for a
// stream because the endpoint returns the completed payload as SSE events.
func (e *ChatGPTExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
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
	originalTranslated, body := translateCodexRequestPair(from, to, baseModel, originalPayload, req.Payload, false, helps.APIKeyModelIsCompat(req))

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = prepareChatGPTBody(body, baseModel, true)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, url, err := e.newChatGPTUpstreamRequest(ctx, auth, body, opts.Headers)
	if err != nil {
		return resp, err
	}
	e.recordChatGPTRequest(ctx, auth, url, httpReq.Header.Clone(), body)

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("chatgpt executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return resp, err
	}

	upstreamData, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errCtx)
			err = errCtx
			return resp, err
		}
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, upstreamData)

	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	for _, line := range bytes.Split(upstreamData, []byte("\n")) {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		eventData := bytes.TrimSpace(line[len(dataTag):])

		if streamErr, ok := chatGPTTerminalFailure(eventData); ok {
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			err = streamErr
			return resp, err
		}

		switch gjson.GetBytes(eventData, "type").String() {
		case "response.output_item.done":
			collectCodexOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
			continue
		case "response.completed", "response.incomplete", "response.done":
		default:
			continue
		}

		if detail, ok := helps.ParseCodexUsage(eventData); ok {
			reporter.Publish(ctx, detail)
		} else {
			reporter.EnsurePublished(ctx)
		}
		completedData := patchCodexCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)

		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, body, completedData, &param)
		if responseFormat == sdktranslator.FormatOpenAIResponse {
			out = helps.EnsureResponsesUsageDetails(out)
		}
		return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
	}

	err = newCodexIncompleteStreamError()
	return resp, err
}
