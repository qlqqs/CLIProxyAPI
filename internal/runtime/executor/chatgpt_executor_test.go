package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	chatGPTTestToken   = "test-access-token"
	chatGPTTestModel   = "gpt-5.6-sol"
	chatGPTTestRefresh = "test-refresh-token"
	chatGPTTestClient  = "oaiapp_test_client"
)

// chatGPTCompletedSSE is the terminal-event shape observed on the direct endpoint:
// response.output only arrives as response.output_item.done events, and
// response.completed carries an empty output array.
const chatGPTCompletedSSE = `data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol","status":"in_progress"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello from chatgpt"}]}}

data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}

`

func newChatGPTTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "chatgpt-test",
		Provider: chatgpt.ProviderName,
		Metadata: map[string]any{
			"access_token":  chatGPTTestToken,
			"refresh_token": chatGPTTestRefresh,
			"client_id":     chatGPTTestClient,
			"type":          chatgpt.ProviderName,
		},
		Attributes: map[string]string{"base_url": baseURL},
	}
}

func chatGPTTestPayload() []byte {
	return []byte(`{
		"model": "gpt-5.6-sol",
		"input": [{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"stream": false,
		"store": true,
		"temperature": 0.4,
		"top_p": 0.9,
		"max_output_tokens": 128,
		"max_completion_tokens": 128,
		"truncation": "auto",
		"prompt_cache_retention": "24h",
		"prompt_cache_options": {"mode":"explicit"}
	}`)
}

func chatGPTTestRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   chatGPTTestModel,
		Payload: chatGPTTestPayload(),
		Format:  sdktranslator.FormatOpenAIResponse,
	}
}

func chatGPTTestOptions(stream bool) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Stream:         stream,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
}

// capturedUpstream records what the executor sent to the fake endpoint.
type capturedUpstream struct {
	path      string
	headers   http.Header
	body      []byte
	callCount int32
}

func newChatGPTUpstreamServer(t *testing.T, responseBody string, status int, contentType string) (*httptest.Server, *capturedUpstream) {
	t.Helper()
	captured := &capturedUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&captured.callCount, 1)
		captured.path = r.URL.Path
		captured.headers = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		captured.body = body
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)
	return server, captured
}

func TestChatGPTExecutorIdentifierAndRequestToFormat(t *testing.T) {
	executor := NewChatGPTExecutor(nil)
	if got := executor.Identifier(); got != chatgpt.ProviderName {
		t.Fatalf("Identifier = %q, want %q", got, chatgpt.ProviderName)
	}
	if got := executor.RequestToFormat(cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); got != sdktranslator.FormatCodex {
		t.Fatalf("RequestToFormat = %q, want %q", got, sdktranslator.FormatCodex)
	}
}

func TestChatGPTPrepareBodyEnforcesDirectEndpointConstraints(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantInput func(t *testing.T, body []byte)
	}{
		{
			name:    "rejected parameters are removed and store is forced false",
			payload: string(chatGPTTestPayload()),
		},
		{
			name:    "string input is wrapped into a user message",
			payload: `{"model":"m","input":"plain text","store":true}`,
			wantInput: func(t *testing.T, body []byte) {
				t.Helper()
				if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "plain text" {
					t.Fatalf("wrapped input text = %q", got)
				}
				if got := gjson.GetBytes(body, "input.0.role").String(); got != "user" {
					t.Fatalf("wrapped input role = %q", got)
				}
			},
		},
		{
			name:    "missing input becomes an empty array",
			payload: `{"model":"m"}`,
			wantInput: func(t *testing.T, body []byte) {
				t.Helper()
				input := gjson.GetBytes(body, "input")
				if !input.IsArray() || len(input.Array()) != 0 {
					t.Fatalf("input = %s", input.Raw)
				}
			},
		},
		{
			name:    "object input becomes an empty array",
			payload: `{"model":"m","input":{"role":"user"}}`,
			wantInput: func(t *testing.T, body []byte) {
				t.Helper()
				input := gjson.GetBytes(body, "input")
				if !input.IsArray() || len(input.Array()) != 0 {
					t.Fatalf("input = %s", input.Raw)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := prepareChatGPTBody([]byte(test.payload), chatGPTTestModel, true)

			if got := gjson.GetBytes(body, "model").String(); got != chatGPTTestModel {
				t.Fatalf("model = %q", got)
			}
			if got := gjson.GetBytes(body, "store"); got.Type != gjson.False {
				t.Fatalf("store = %s (type %v), want false", got.Raw, got.Type)
			}
			if got := gjson.GetBytes(body, "stream"); got.Type != gjson.True {
				t.Fatalf("stream = %s, want true", got.Raw)
			}
			if input := gjson.GetBytes(body, "input"); !input.IsArray() {
				t.Fatalf("input is not an array: %s", input.Raw)
			}
			for _, field := range chatGPTRejectedParams {
				if gjson.GetBytes(body, field).Exists() {
					t.Fatalf("rejected field %q survived: %s", field, body)
				}
			}
			if test.wantInput != nil {
				test.wantInput(t, body)
			}
		})
	}
}

func TestChatGPTExecutorSendsPlainDirectRequest(t *testing.T) {
	server, captured := newChatGPTUpstreamServer(t, chatGPTCompletedSSE, http.StatusOK, "text/event-stream")

	executor := NewChatGPTExecutor(nil)
	resp, err := executor.Execute(context.Background(), newChatGPTTestAuth(server.URL), chatGPTTestRequest(), chatGPTTestOptions(false))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if captured.path != "/responses" {
		t.Fatalf("path = %q, want /responses", captured.path)
	}
	if got := captured.headers.Get("Authorization"); got != "Bearer "+chatGPTTestToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := captured.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	// No Codex cloaking: this endpoint accepts the credential as itself.
	for _, header := range []string{"originator", "Chatgpt-Account-Id", "Version", "X-Codex-Turn-Metadata", "Session-Id"} {
		if got := captured.headers.Get(header); got != "" {
			t.Fatalf("unexpected Codex header %s = %q", header, got)
		}
	}
	if ua := captured.headers.Get("User-Agent"); strings.Contains(ua, "codex_cli_rs") {
		t.Fatalf("User-Agent impersonates the Codex client: %q", ua)
	}

	body := captured.body
	if got := gjson.GetBytes(body, "model").String(); got != chatGPTTestModel {
		t.Fatalf("upstream model = %q", got)
	}
	if got := gjson.GetBytes(body, "store"); got.Type != gjson.False {
		t.Fatalf("upstream store = %s, want false", got.Raw)
	}
	if got := gjson.GetBytes(body, "stream"); got.Type != gjson.True {
		t.Fatalf("upstream stream = %s, want true", got.Raw)
	}
	if input := gjson.GetBytes(body, "input"); !input.IsArray() || len(input.Array()) == 0 {
		t.Fatalf("upstream input = %s", input.Raw)
	}
	for _, field := range chatGPTRejectedParams {
		if gjson.GetBytes(body, field).Exists() {
			t.Fatalf("rejected field %q reached upstream: %s", field, body)
		}
	}
	if len(resp.Payload) == 0 {
		t.Fatal("empty response payload")
	}
}

func TestChatGPTExecutorAggregatesEmptyCompletedOutput(t *testing.T) {
	server, _ := newChatGPTUpstreamServer(t, chatGPTCompletedSSE, http.StatusOK, "text/event-stream")

	executor := NewChatGPTExecutor(nil)
	resp, err := executor.Execute(context.Background(), newChatGPTTestAuth(server.URL), chatGPTTestRequest(), chatGPTTestOptions(false))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := gjson.GetBytes(resp.Payload, "output")
	if !output.IsArray() || len(output.Array()) == 0 {
		t.Fatalf("expected output rebuilt from response.output_item.done, got %s", resp.Payload)
	}
	if !strings.Contains(string(resp.Payload), "hello from chatgpt") {
		t.Fatalf("aggregated payload lost the assistant text: %s", resp.Payload)
	}
}

func TestChatGPTExecutorStreamsTranslatedChunks(t *testing.T) {
	server, _ := newChatGPTUpstreamServer(t, chatGPTCompletedSSE, http.StatusOK, "text/event-stream")

	executor := NewChatGPTExecutor(nil)
	result, err := executor.ExecuteStream(context.Background(), newChatGPTTestAuth(server.URL), chatGPTTestRequest(), chatGPTTestOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if result == nil || result.Chunks == nil {
		t.Fatal("nil stream result")
	}

	var payloads []string
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	if streamErr != nil {
		t.Fatalf("stream error: %v", streamErr)
	}
	if len(payloads) == 0 {
		t.Fatal("no chunks produced")
	}
	joined := strings.Join(payloads, "\n")
	if !strings.Contains(joined, "hello from chatgpt") {
		t.Fatalf("streamed payloads lost the assistant text: %s", joined)
	}
	if !strings.Contains(joined, "response.completed") {
		t.Fatalf("streamed payloads missing the terminal event: %s", joined)
	}
}

func TestChatGPTExecutorStreamReportsTerminalFailure(t *testing.T) {
	const failedSSE = `data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol"}}

data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"upstream exploded"}}}

`
	server, _ := newChatGPTUpstreamServer(t, failedSSE, http.StatusOK, "text/event-stream")

	executor := NewChatGPTExecutor(nil)
	result, err := executor.ExecuteStream(context.Background(), newChatGPTTestAuth(server.URL), chatGPTTestRequest(), chatGPTTestOptions(true))
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected a terminal stream error")
	}
	status, ok := streamErr.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", streamErr)
	}
	if status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status.StatusCode())
	}
	if !strings.Contains(status.Error(), "upstream exploded") {
		t.Fatalf("error message = %q", status.Error())
	}
}

func TestChatGPTExecutorMapsUpstreamErrorStatus(t *testing.T) {
	const errorBody = `{"error":{"message":"Unsupported parameter: top_p","type":"invalid_request_error"}}`

	t.Run("non-streaming", func(t *testing.T) {
		server, _ := newChatGPTUpstreamServer(t, errorBody, http.StatusBadRequest, "application/json")
		executor := NewChatGPTExecutor(nil)
		_, err := executor.Execute(context.Background(), newChatGPTTestAuth(server.URL), chatGPTTestRequest(), chatGPTTestOptions(false))
		assertChatGPTStatusErr(t, err, http.StatusBadRequest, "Unsupported parameter: top_p")
	})

	t.Run("streaming", func(t *testing.T) {
		server, _ := newChatGPTUpstreamServer(t, errorBody, http.StatusBadRequest, "application/json")
		executor := NewChatGPTExecutor(nil)
		_, err := executor.ExecuteStream(context.Background(), newChatGPTTestAuth(server.URL), chatGPTTestRequest(), chatGPTTestOptions(true))
		assertChatGPTStatusErr(t, err, http.StatusBadRequest, "Unsupported parameter: top_p")
	})
}

func assertChatGPTStatusErr(t *testing.T, err error, wantStatus int, wantMessage string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr (%v)", err, err)
	}
	if status.StatusCode() != wantStatus {
		t.Fatalf("status = %d, want %d", status.StatusCode(), wantStatus)
	}
	if !strings.Contains(status.Error(), wantMessage) {
		t.Fatalf("error message = %q, want substring %q", status.Error(), wantMessage)
	}
}

// stubChatGPTAuthService records refresh calls without touching the network.
type stubChatGPTAuthService struct {
	credential *chatgpt.Credential
	err        error
	calls      int
	gotRefresh string
	gotClient  string
}

func (s *stubChatGPTAuthService) RefreshAccessToken(_ context.Context, refreshToken, clientID string) (*chatgpt.Credential, error) {
	s.calls++
	s.gotRefresh = refreshToken
	s.gotClient = clientID
	if s.err != nil {
		return nil, s.err
	}
	return s.credential, nil
}

func newStubChatGPTExecutor(stub *stubChatGPTAuthService) *ChatGPTExecutor {
	executor := NewChatGPTExecutor(nil)
	executor.newAuthService = func(*config.Config, string) chatGPTAuthService { return stub }
	return executor
}

func TestChatGPTRefreshRotatesTokens(t *testing.T) {
	stub := &stubChatGPTAuthService{credential: &chatgpt.Credential{
		AccessToken:  "rotated-access",
		RefreshToken: "rotated-refresh",
		ClientID:     chatGPTTestClient,
		Scopes:       []string{"openid", chatgpt.DirectTokenScope},
		ExpiresIn:    3600,
	}}
	executor := newStubChatGPTExecutor(stub)
	auth := newChatGPTTestAuth("https://api.openai.com/v1")
	auth.ProxyURL = "http://proxy.test:8080"

	updated, err := executor.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", stub.calls)
	}
	if stub.gotRefresh != chatGPTTestRefresh || stub.gotClient != chatGPTTestClient {
		t.Fatalf("refresh args = %q/%q", stub.gotRefresh, stub.gotClient)
	}
	if updated.Metadata["access_token"] != "rotated-access" {
		t.Fatalf("access_token = %v", updated.Metadata["access_token"])
	}
	if updated.Metadata["refresh_token"] != "rotated-refresh" {
		t.Fatalf("refresh_token = %v", updated.Metadata["refresh_token"])
	}
	if updated.Metadata["client_id"] != chatGPTTestClient {
		t.Fatalf("client_id = %v", updated.Metadata["client_id"])
	}
	if updated.Metadata["type"] != chatgpt.ProviderName {
		t.Fatalf("type = %v", updated.Metadata["type"])
	}
	if expired, _ := updated.Metadata["expired"].(string); expired == "" {
		t.Fatal("expired was not set")
	}
	if lastRefresh, _ := updated.Metadata["last_refresh"].(string); lastRefresh == "" {
		t.Fatal("last_refresh was not set")
	}
}

func TestChatGPTRefreshSkipsWithoutClientIDOrRefreshToken(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{name: "missing client id", metadata: map[string]any{"access_token": "a", "refresh_token": "r"}},
		{name: "missing refresh token", metadata: map[string]any{"access_token": "a", "client_id": chatGPTTestClient}},
		{name: "empty metadata", metadata: map[string]any{}},
		{name: "nil metadata", metadata: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &stubChatGPTAuthService{credential: &chatgpt.Credential{AccessToken: "should-not-be-used"}}
			executor := newStubChatGPTExecutor(stub)
			auth := &cliproxyauth.Auth{ID: "chatgpt-test", Provider: chatgpt.ProviderName, Metadata: test.metadata}

			updated, err := executor.Refresh(context.Background(), auth)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if updated != auth {
				t.Fatal("auth should be returned unchanged")
			}
			if stub.calls != 0 {
				t.Fatalf("refresh service was called %d times, want 0", stub.calls)
			}
			if test.metadata != nil && test.metadata["access_token"] != "a" && test.metadata["access_token"] != nil {
				t.Fatalf("metadata was mutated: %v", test.metadata)
			}
		})
	}
}

func TestChatGPTRefreshPropagatesServiceFailure(t *testing.T) {
	stub := &stubChatGPTAuthService{err: fmt.Errorf("token endpoint rejected the refresh")}
	executor := newStubChatGPTExecutor(stub)
	auth := newChatGPTTestAuth("https://api.openai.com/v1")

	if _, err := executor.Refresh(context.Background(), auth); err == nil {
		t.Fatal("expected the refresh failure to propagate")
	}
}

func TestChatGPTRefreshRequiresAuth(t *testing.T) {
	executor := newStubChatGPTExecutor(&stubChatGPTAuthService{})
	if _, err := executor.Refresh(context.Background(), nil); err == nil {
		t.Fatal("expected an error for a nil auth")
	}
}

func TestChatGPTCredsPrecedenceAndDefaultBaseURL(t *testing.T) {
	if token, baseURL := chatGPTCreds(nil); token != "" || baseURL != "" {
		t.Fatalf("nil auth = %q/%q", token, baseURL)
	}
	// Metadata token with no attributes still resolves.
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": " meta-token "}}
	if token, _ := chatGPTCreds(auth); token != "meta-token" {
		t.Fatalf("token = %q", token)
	}
	if got := chatGPTUpstreamURL(auth); got != chatgpt.DefaultBaseURL+"/responses" {
		t.Fatalf("url = %q", got)
	}
	// api_key attribute wins when both are present.
	auth.Attributes = map[string]string{"api_key": "attr-token", "base_url": "https://example.test/v1/"}
	if token, _ := chatGPTCreds(auth); token != "attr-token" {
		t.Fatalf("token = %q", token)
	}
	if got := chatGPTUpstreamURL(auth); got != "https://example.test/v1/responses" {
		t.Fatalf("url = %q", got)
	}
}

func TestChatGPTExecutorCountTokens(t *testing.T) {
	executor := NewChatGPTExecutor(nil)
	resp, err := executor.CountTokens(context.Background(), newChatGPTTestAuth("https://api.openai.com/v1"), chatGPTTestRequest(), chatGPTTestOptions(false))
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("empty token count payload")
	}
	if !gjson.GetBytes(resp.Payload, "usage.input_tokens").Exists() && !strings.Contains(string(resp.Payload), "input_tokens") {
		t.Fatalf("token count payload missing usage: %s", resp.Payload)
	}
}

func TestChatGPTExecutorPrepareRequest(t *testing.T) {
	executor := NewChatGPTExecutor(nil)
	auth := newChatGPTTestAuth("https://api.openai.com/v1")
	auth.Attributes["header:X-Test"] = "custom-value"

	req, err := http.NewRequest(http.MethodPost, chatGPTUpstreamURL(auth), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if errPrepare := executor.PrepareRequest(req, auth); errPrepare != nil {
		t.Fatalf("PrepareRequest: %v", errPrepare)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+chatGPTTestToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("X-Test"); got != "custom-value" {
		t.Fatalf("custom header = %q", got)
	}

	// Without a token the Authorization header is dropped rather than left stale.
	req.Header.Set("Authorization", "Bearer stale")
	if errPrepare := executor.PrepareRequest(req, nil); errPrepare != nil {
		t.Fatalf("PrepareRequest(nil auth): %v", errPrepare)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestChatGPTExecutorHttpRequestRequiresRequest(t *testing.T) {
	executor := NewChatGPTExecutor(nil)
	if _, err := executor.HttpRequest(context.Background(), nil, nil); err == nil {
		t.Fatal("expected an error for a nil request")
	}
}
