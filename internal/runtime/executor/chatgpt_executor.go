package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// chatGPTUserAgent identifies this proxy to the public API host. The Codex client
// identity is deliberately not impersonated: these credentials are accepted by
// api.openai.com as themselves, and cloaking headers are what the Codex backend
// expects instead of a plain bearer request.
const chatGPTUserAgent = "cli-proxy-chatgpt"

// chatGPTRejectedParams lists request fields that the public Responses endpoint
// rejects with "Unsupported parameter: <name>". They are removed after translation
// and config application so that neither a client nor a payload rule can reintroduce
// them.
var chatGPTRejectedParams = []string{
	"max_output_tokens",
	// max_completion_tokens is the chat-completions alias for the same rejected
	// limit and is not a Responses API parameter; the codex request translator
	// deletes it for the same upstream (translator/codex/openai/responses).
	"max_completion_tokens",
	"temperature",
	"prompt_cache_retention",
	"prompt_cache_options",
	"top_p",
	"truncation",
}

// chatGPTAuthService is the subset of *chatgpt.ChatGPTAuth the executor needs.
type chatGPTAuthService interface {
	RefreshAccessToken(ctx context.Context, refreshToken, clientID string) (*chatgpt.Credential, error)
}

// ChatGPTExecutor serves OpenAI Responses API traffic for credentials issued by the
// "Sign in with ChatGPT" flow.
//
// Unlike CodexExecutor it targets the public API host (https://api.openai.com/v1)
// instead of the Codex backend (https://chatgpt.com/backend-api/codex). Tokens minted
// by the dynamic-client flow carry the chatgpt.tokens.use.direct scope and are
// rejected by the Codex backend (401 rejected_by_access_enforcement /
// no_matching_rule), so this executor must never fall back to that host.
//
// The upstream wire format is the OpenAI Responses API, which is what
// sdktranslator.FromString("codex") denotes in this repository, so the shared
// translation pipeline is reused as-is minus every Codex-specific header and payload
// tweak.
type ChatGPTExecutor struct {
	cfg *config.Config
	// newAuthService is a seam for tests; production builds chatgpt.ChatGPTAuth.
	newAuthService func(*config.Config, string) chatGPTAuthService
}

// NewChatGPTExecutor creates a "Sign in with ChatGPT" executor.
func NewChatGPTExecutor(cfg *config.Config) *ChatGPTExecutor {
	return &ChatGPTExecutor{
		cfg: cfg,
		newAuthService: func(cfg *config.Config, proxyURL string) chatGPTAuthService {
			return chatgpt.NewChatGPTAuthWithProxyURL(cfg, proxyURL)
		},
	}
}

// Identifier returns the provider key handled by this executor.
func (e *ChatGPTExecutor) Identifier() string { return chatgpt.ProviderName }

// RequestToFormat reports the upstream wire format. The Responses API is addressed in
// this repository with the "codex" format token, which the conductor would otherwise
// not select for a non-codex provider.
func (e *ChatGPTExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatCodex
}

func (e *ChatGPTExecutor) authService(cfg *config.Config, proxyURL string) chatGPTAuthService {
	if e != nil && e.newAuthService != nil {
		return e.newAuthService(cfg, proxyURL)
	}
	return chatgpt.NewChatGPTAuthWithProxyURL(cfg, proxyURL)
}

// chatGPTCreds resolves the bearer token and upstream base URL for an auth record.
// Tokens live in metadata (auth files are re-read into Metadata), while api_key /
// base_url attributes cover records built in memory.
func chatGPTCreds(a *cliproxyauth.Auth) (token, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		token = strings.TrimSpace(a.Attributes["api_key"])
		baseURL = strings.TrimSpace(a.Attributes["base_url"])
	}
	if token == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			token = strings.TrimSpace(v)
		}
	}
	return token, baseURL
}

// chatGPTUpstreamURL returns the Responses endpoint for this credential.
func chatGPTUpstreamURL(auth *cliproxyauth.Auth) string {
	_, baseURL := chatGPTCreds(auth)
	if baseURL == "" {
		baseURL = chatgpt.DefaultBaseURL
	}
	return strings.TrimSuffix(baseURL, "/") + "/responses"
}

// prepareChatGPTBody enforces the constraints of the public Responses endpoint:
// input must be a JSON array, store must be present and false, and the rejected
// parameters must be absent.
func prepareChatGPTBody(body []byte, model string, stream bool) []byte {
	body = helps.SetStringIfDifferent(body, "model", model)
	for _, field := range chatGPTRejectedParams {
		body, _ = sjson.DeleteBytes(body, field)
	}
	body = helps.SetBoolIfDifferent(body, "stream", stream)
	body = helps.SetBoolIfDifferent(body, "store", false)
	return ensureChatGPTInputArray(body)
}

// ensureChatGPTInputArray guarantees the `input` field is a JSON array. A plain
// string is wrapped into a single user message; anything else unusable becomes an
// empty array, which the endpoint accepts.
func ensureChatGPTInputArray(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	switch {
	case !input.Exists() || input.Type == gjson.Null:
		updated, errSet := sjson.SetRawBytes(body, "input", []byte("[]"))
		if errSet == nil {
			return updated
		}
		return body
	case input.IsArray():
		return body
	case input.Type == gjson.String:
		wrapped := []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": input.String()}},
		}}
		updated, errSet := sjson.SetBytes(body, "input", wrapped)
		if errSet == nil {
			return updated
		}
		return body
	default:
		updated, errSet := sjson.SetRawBytes(body, "input", []byte("[]"))
		if errSet == nil {
			return updated
		}
		return body
	}
}

// applyChatGPTHeaders sets the plain bearer headers this endpoint expects. No
// Codex cloaking headers (originator, Chatgpt-Account-Id, Version, X-Codex-*) are
// added, and the Codex client User-Agent is not impersonated.
func applyChatGPTHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, clientHeaders ...http.Header) {
	if r == nil {
		return
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Del("Authorization")
	}
	r.Header.Set("User-Agent", chatGPTUserAgent)
	applyChatGPTCustomHeaders(r, auth, clientHeaders...)
}

// applyChatGPTCustomHeaders applies per-account headers declared in auth metadata.
func applyChatGPTCustomHeaders(r *http.Request, auth *cliproxyauth.Auth, clientHeaders ...http.Header) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		util.ApplyCustomHeadersFromAttrs(r, attrs, clientHeaders[0])
		return
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
}

// newChatGPTUpstreamRequest builds the POST request used by every call path.
func (e *ChatGPTExecutor) newChatGPTUpstreamRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte, clientHeaders ...http.Header) (*http.Request, string, error) {
	url := chatGPTUpstreamURL(auth)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, url, err
	}
	token, _ := chatGPTCreds(auth)
	applyChatGPTHeaders(httpReq, auth, token, clientHeaders...)
	return httpReq, url, nil
}

// recordChatGPTRequest forwards the upstream request into the request log.
func (e *ChatGPTExecutor) recordChatGPTRequest(ctx context.Context, auth *cliproxyauth.Auth, url string, headers http.Header, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   headers,
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

// chatGPTTerminalFailure extracts a terminal failure from an SSE event payload.
// The public endpoint reports failures as `error` / `response.failed` events, which
// can arrive inside an HTTP 200 stream.
func chatGPTTerminalFailure(eventData []byte) (statusErr, bool) {
	switch gjson.GetBytes(eventData, "type").String() {
	case "error", "response.failed", "response.error":
	default:
		return statusErr{}, false
	}
	status := 0
	for _, path := range []string{"error.status_code", "error.status", "response.error.status_code", "response.error.status"} {
		if candidate := int(gjson.GetBytes(eventData, path).Int()); candidate >= 400 && candidate <= 599 {
			status = candidate
			break
		}
	}
	if status == 0 {
		status = http.StatusBadRequest
	}
	message := strings.TrimSpace(gjson.GetBytes(eventData, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(eventData, "response.error.message").String())
	}
	if message == "" {
		message = strings.TrimSpace(string(eventData))
	}
	return statusErr{code: status, msg: message}, true
}
