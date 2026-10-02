package executor

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// CountTokens estimates the input token count locally; the public endpoint offers no
// token counting route for this provider, so the same tokenizer the Codex executor
// uses is applied to the prepared Responses payload.
func (e *ChatGPTExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("codex")
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, false, helps.APIKeyModelIsCompat(req))

	body, err := helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	body = prepareChatGPTBody(body, baseModel, false)

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("chatgpt executor: tokenizer init failed: %w", err)
	}
	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("chatgpt executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: translated}, nil
}
