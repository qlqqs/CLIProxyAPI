package test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLiveDriverConcurrentJSONBarrierAndEvidenceRedaction(t *testing.T) {
	const concurrency = 4
	const apiKey = "unit-secret-api-key"
	const prompt = "unit-secret-full-prompt"

	arrived := make(chan string, concurrency)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequests := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequests)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Error("request did not contain expected live Authorization header")
		}
		requestID := request.Header.Get(liveTestRequestHeader)
		arrived <- requestID
		<-release
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Request-Id", "server-"+requestID)
		_, _ = response.Write([]byte(`{"id":"chatcmpl_test","choices":[{"message":{"content":"LIVE_OK"},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`))
	}))
	defer server.Close()

	baseURL, errURL := url.Parse(server.URL)
	if errURL != nil {
		t.Fatalf("parse test server URL: %v", errURL)
	}
	evidencePath := filepath.Join(t.TempDir(), "evidence", "results.jsonl")
	cfg := liveConfig{
		BaseURL:          baseURL,
		APIKey:           apiKey,
		Protocol:         liveProtocolChat,
		Path:             "/v1/chat/completions",
		Model:            "test-model",
		Concurrency:      concurrency,
		Rounds:           1,
		RunID:            "unit-run",
		Scenario:         "json-barrier",
		EvidencePath:     evidencePath,
		RequestTimeout:   5 * time.Second,
		ExpectedStatuses: statusMatcher{{Min: 200, Max: 299}},
		ExpectedOutcome:  "success",
		Prompt:           prompt,
		Client:           server.Client(),
	}

	type runResponse struct {
		results []liveResult
		err     error
	}
	runDone := make(chan runResponse, 1)
	go func() {
		results, errRun := runLiveDriver(context.Background(), cfg)
		runDone <- runResponse{results: results, err: errRun}
	}()

	requestIDs := make(map[string]struct{}, concurrency)
	for index := 0; index < concurrency; index++ {
		select {
		case requestID := <-arrived:
			if requestID == "" {
				t.Fatal("live request ID was empty")
			}
			requestIDs[requestID] = struct{}{}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent requests did not reach the server barrier")
		}
	}
	releaseRequests()

	var run runResponse
	select {
	case run = <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("live driver did not finish after server release")
	}
	if run.err != nil {
		t.Fatalf("runLiveDriver error: %v", run.err)
	}
	if len(run.results) != concurrency {
		t.Fatalf("result count = %d, want %d", len(run.results), concurrency)
	}
	if len(requestIDs) != concurrency {
		t.Fatalf("unique request ID count = %d, want %d", len(requestIDs), concurrency)
	}
	for _, result := range run.results {
		if !result.Evidence.Matched || result.Evidence.Outcome != "success" {
			t.Fatalf("unexpected live result: %#v", result.Evidence)
		}
	}

	evidence, errRead := os.ReadFile(evidencePath)
	if errRead != nil {
		t.Fatalf("read evidence: %v", errRead)
	}
	for _, forbidden := range []string{apiKey, prompt, "Authorization", "LIVE_OK"} {
		if bytes.Contains(evidence, []byte(forbidden)) {
			t.Fatalf("evidence contains forbidden value %q", forbidden)
		}
	}
	if lines := bytes.Count(evidence, []byte("\n")); lines != concurrency {
		t.Fatalf("evidence line count = %d, want %d", lines, concurrency)
	}
	info, errStat := os.Stat(evidencePath)
	if errStat != nil {
		t.Fatalf("stat evidence: %v", errStat)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("evidence permissions = %o, want 600", permissions)
	}
}

func TestLiveDriverSSEValidation(t *testing.T) {
	tests := []struct {
		name     string
		protocol liveProtocol
		stream   string
		terminal string
	}{
		{
			name:     "chat",
			protocol: liveProtocolChat,
			stream: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
			terminal: "[DONE]",
		},
		{
			name:     "responses",
			protocol: liveProtocolResponses,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n",
			terminal: "response.completed",
		},
		{
			name:     "anthropic",
			protocol: liveProtocolAnthropic,
			stream: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			terminal: "message_stop",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, errValidate := validateLiveSSE(ctx, cancel, strings.NewReader(testCase.stream), testCase.protocol, 0, time.Now())
			if errValidate != nil {
				t.Fatalf("validateLiveSSE error: %v", errValidate)
			}
			if result.Terminal != testCase.terminal {
				t.Fatalf("terminal = %q, want %q", result.Terminal, testCase.terminal)
			}
			if result.EventCount < 3 {
				t.Fatalf("event count = %d, want at least 3", result.EventCount)
			}
		})
	}
}

func TestLiveDriverCancelsSSEAfterDeterministicEventCount(t *testing.T) {
	const apiKey = "cancel-secret-key"
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := response.(http.Flusher)
		if !ok {
			t.Error("test server response did not implement http.Flusher")
			return
		}
		_, _ = response.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n\n"))
		flusher.Flush()
		<-request.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	baseURL, errURL := url.Parse(server.URL)
	if errURL != nil {
		t.Fatalf("parse test server URL: %v", errURL)
	}
	cfg := liveConfig{
		BaseURL:           baseURL,
		APIKey:            apiKey,
		Protocol:          liveProtocolChat,
		Path:              "/v1/chat/completions",
		Model:             "test-model",
		Stream:            true,
		Concurrency:       1,
		Rounds:            1,
		RunID:             "cancel-run",
		Scenario:          "cancel-sse",
		EvidencePath:      filepath.Join(t.TempDir(), "cancel.jsonl"),
		RequestTimeout:    5 * time.Second,
		CancelAfterEvents: 1,
		ExpectedStatuses:  statusMatcher{{Min: 200, Max: 299}},
		ExpectedOutcome:   "canceled",
		Prompt:            liveTestPrompt,
		Client:            server.Client(),
	}

	results, errRun := runLiveDriver(context.Background(), cfg)
	if errRun != nil {
		t.Fatalf("runLiveDriver error: %v", errRun)
	}
	if len(results) != 1 || !results[0].Evidence.Matched || results[0].Evidence.Outcome != "canceled" {
		t.Fatalf("unexpected cancellation result: %#v", results)
	}
	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe request cancellation")
	}
}

func TestLiveDriverRejectsIncompleteAndLeakingSSE(t *testing.T) {
	tests := []struct {
		name     string
		protocol liveProtocol
		stream   string
	}{
		{
			name:     "missing chat terminal",
			protocol: liveProtocolChat,
			stream:   "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n",
		},
		{
			name:     "Anthropic DONE leakage",
			protocol: liveProtocolAnthropic,
			stream:   "event: message_start\ndata: {\"type\":\"message_start\"}\n\ndata: [DONE]\n\n",
		},
		{
			name:     "Responses delta before created",
			protocol: liveProtocolResponses,
			stream: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n",
		},
		{
			name:     "Responses mismatched terminal ID",
			protocol: liveProtocolResponses,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\"}}\n\n",
		},
		{
			name:     "Responses DONE leakage",
			protocol: liveProtocolResponses,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:     "Anthropic delta before start",
			protocol: liveProtocolAnthropic,
			stream: "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
				"event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, errValidate := validateLiveSSE(ctx, cancel, strings.NewReader(testCase.stream), testCase.protocol, 0, time.Now()); errValidate == nil {
				t.Fatal("validateLiveSSE succeeded, want error")
			}
		})
	}
}

func TestLiveDriverRejectsOversizedSSE(t *testing.T) {
	line := ":" + strings.Repeat("x", maxLiveSSELineBytes-2) + "\n"
	stream := strings.Repeat(line, maxLiveResponseBytes/(len(line))+1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, errValidate := validateLiveSSE(ctx, cancel, strings.NewReader(stream), liveProtocolChat, 0, time.Now()); errValidate == nil || !strings.Contains(errValidate.Error(), "validation limit") {
		t.Fatalf("validateLiveSSE error = %v, want validation limit error", errValidate)
	}
}

func TestLiveDriverDoesNotFollowRedirects(t *testing.T) {
	redirectFollowed := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		redirectFollowed <- struct{}{}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	baseURL, errURL := url.Parse(redirect.URL)
	if errURL != nil {
		t.Fatalf("parse redirect server URL: %v", errURL)
	}
	cfg := liveConfig{
		BaseURL:          baseURL,
		APIKey:           "redirect-secret-key",
		Protocol:         liveProtocolChat,
		Path:             "/v1/chat/completions",
		Model:            "test-model",
		Concurrency:      1,
		Rounds:           1,
		RunID:            "redirect-run",
		Scenario:         "redirect",
		EvidencePath:     filepath.Join(t.TempDir(), "redirect.jsonl"),
		RequestTimeout:   5 * time.Second,
		ExpectedStatuses: statusMatcher{{Min: http.StatusTemporaryRedirect, Max: http.StatusTemporaryRedirect}},
		ExpectedOutcome:  "failure",
		Prompt:           liveTestPrompt,
		Client:           redirect.Client(),
	}

	results, errRun := runLiveDriver(context.Background(), cfg)
	if errRun != nil {
		t.Fatalf("runLiveDriver error: %v", errRun)
	}
	if len(results) != 1 || !results[0].Evidence.Matched || results[0].Evidence.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("unexpected redirect result: %#v", results)
	}
	select {
	case <-redirectFollowed:
		t.Fatal("live driver followed an HTTP redirect")
	default:
	}
}

func TestLiveDriverJSONValidationRequiresOutputAndUsage(t *testing.T) {
	tests := []struct {
		name     string
		protocol liveProtocol
		body     string
		wantErr  bool
	}{
		{
			name:     "chat valid",
			protocol: liveProtocolChat,
			body:     `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`,
		},
		{
			name:     "chat missing usage",
			protocol: liveProtocolChat,
			body:     `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`,
			wantErr:  true,
		},
		{
			name:     "Responses valid",
			protocol: liveProtocolResponses,
			body:     `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"total_tokens":3}}`,
		},
		{
			name:     "Responses missing text",
			protocol: liveProtocolResponses,
			body:     `{"id":"resp_1","status":"completed","output":[],"usage":{"total_tokens":3}}`,
			wantErr:  true,
		},
		{
			name:     "Anthropic valid",
			protocol: liveProtocolAnthropic,
			body:     `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		},
		{
			name:     "Anthropic missing usage",
			protocol: liveProtocolAnthropic,
			body:     `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
			wantErr:  true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, errValidate := validateLiveJSON(testCase.protocol, []byte(testCase.body))
			if (errValidate != nil) != testCase.wantErr {
				t.Fatalf("validateLiveJSON error = %v, wantErr %v", errValidate, testCase.wantErr)
			}
		})
	}
}

func TestLiveDriverEvidenceSanitizesUntrustedMetadata(t *testing.T) {
	const secretMetadata = "metadata secret with spaces"
	payload := map[string]any{secretMetadata: true}
	body := []byte(`{"metadata secret with spaces":true}`)
	if summary := summarizeJSON(payload, body); strings.Contains(summary, secretMetadata) {
		t.Fatalf("JSON summary leaked untrusted metadata: %q", summary)
	}
	if token := evidenceToken(secretMetadata); strings.Contains(token, secretMetadata) {
		t.Fatalf("evidence token leaked untrusted metadata: %q", token)
	}
}

func TestLiveDriverConfigurationRequiresLoopbackAndExplicitEnableInputs(t *testing.T) {
	values := map[string]string{
		"LIVE_TEST_BASE_URL":      "https://example.com",
		"LIVE_TEST_API_KEY":       "secret",
		"LIVE_TEST_MODEL":         "model",
		"LIVE_TEST_EVIDENCE_PATH": filepath.Join(t.TempDir(), "evidence.jsonl"),
	}
	getenv := func(key string) string { return values[key] }
	if _, errConfig := loadLiveConfig(getenv); errConfig == nil || !strings.Contains(errConfig.Error(), "loopback") {
		t.Fatalf("loadLiveConfig error = %v, want loopback rejection", errConfig)
	}

	values["LIVE_TEST_BASE_URL"] = "http://127.0.0.1:18317"
	cfg, errConfig := loadLiveConfig(getenv)
	if errConfig != nil {
		t.Fatalf("loadLiveConfig loopback error: %v", errConfig)
	}
	if cfg.Concurrency != 1 || cfg.Rounds != 1 || cfg.Protocol != liveProtocolChat {
		t.Fatal("loadLiveConfig returned unexpected defaults")
	}

	values["LIVE_TEST_PATH"] = "/v1/chat/completions?secret=value"
	if _, errConfig = loadLiveConfig(getenv); errConfig == nil || !strings.Contains(errConfig.Error(), "query") {
		t.Fatalf("loadLiveConfig query path error = %v, want rejection", errConfig)
	}
}
