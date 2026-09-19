package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	liveTestPrompt        = "Reply with exactly LIVE_OK."
	liveTestRequestHeader = "X-Live-Test-Request-ID"
	maxLiveResponseBytes  = 4 << 20
	maxLiveSSELineBytes   = 1 << 20
)

type liveProtocol string

const (
	liveProtocolChat      liveProtocol = "openai-chat"
	liveProtocolResponses liveProtocol = "openai-responses"
	liveProtocolAnthropic liveProtocol = "anthropic"
)

type liveConfig struct {
	BaseURL           *url.URL
	APIKey            string
	Protocol          liveProtocol
	Path              string
	Model             string
	Stream            bool
	Concurrency       int
	Rounds            int
	RunID             string
	Scenario          string
	EvidencePath      string
	RequestTimeout    time.Duration
	CancelAfter       time.Duration
	CancelAfterEvents int
	ExpectedStatuses  statusMatcher
	ExpectedOutcome   string
	Prompt            string
	Client            *http.Client
}

type statusRange struct {
	Min int
	Max int
}

type statusMatcher []statusRange

func (m statusMatcher) Matches(status int) bool {
	for _, statusRange := range m {
		if status >= statusRange.Min && status <= statusRange.Max {
			return true
		}
	}
	return false
}

type liveEvidence struct {
	Timestamp       time.Time `json:"timestamp"`
	Scenario        string    `json:"scenario"`
	RunID           string    `json:"run_id"`
	Round           int       `json:"round"`
	Worker          int       `json:"worker"`
	RequestID       string    `json:"request_id"`
	ServerRequestID string    `json:"server_request_id,omitempty"`
	Protocol        string    `json:"protocol"`
	Stream          bool      `json:"stream"`
	StatusCode      int       `json:"status_code,omitempty"`
	ContentType     string    `json:"content_type,omitempty"`
	FirstEventMS    int64     `json:"first_event_ms,omitempty"`
	TotalMS         int64     `json:"total_ms"`
	EventCount      int       `json:"event_count,omitempty"`
	Terminal        string    `json:"terminal,omitempty"`
	Outcome         string    `json:"outcome"`
	ExpectedOutcome string    `json:"expected_outcome"`
	Matched         bool      `json:"matched"`
	ErrorCategory   string    `json:"error_category,omitempty"`
	ResponseSummary string    `json:"response_summary,omitempty"`
}

type liveResult struct {
	Evidence liveEvidence
	Err      error
}

type evidenceWriter struct {
	mu        sync.Mutex
	file      *os.File
	forbidden []string
}

func newEvidenceWriter(path string, forbidden ...string) (*evidenceWriter, error) {
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return nil, fmt.Errorf("create evidence directory: %w", errMkdir)
	}
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return nil, fmt.Errorf("open evidence file: %w", errOpen)
	}
	if errChmod := file.Chmod(0o600); errChmod != nil {
		_ = file.Close()
		return nil, fmt.Errorf("restrict evidence permissions: %w", errChmod)
	}
	return &evidenceWriter{file: file, forbidden: forbidden}, nil
}

func (w *evidenceWriter) Write(record liveEvidence) error {
	encoded, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return fmt.Errorf("marshal evidence: %w", errMarshal)
	}
	for _, value := range w.forbidden {
		if value != "" && bytes.Contains(encoded, []byte(value)) {
			return errors.New("evidence contains a forbidden sensitive value")
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, errWrite := w.file.Write(append(encoded, '\n')); errWrite != nil {
		return fmt.Errorf("write evidence: %w", errWrite)
	}
	return nil
}

func (w *evidenceWriter) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	if errSync := w.file.Sync(); errSync != nil {
		_ = w.file.Close()
		return fmt.Errorf("sync evidence: %w", errSync)
	}
	if errClose := w.file.Close(); errClose != nil {
		return fmt.Errorf("close evidence: %w", errClose)
	}
	return nil
}

func TestLiveFunctional(t *testing.T) {
	if os.Getenv("LIVE_TEST_ENABLE") != "1" {
		t.Skip("set LIVE_TEST_ENABLE=1 and the required LIVE_TEST_* variables to run the live driver")
	}

	cfg, errConfig := loadLiveConfig(os.Getenv)
	if errConfig != nil {
		t.Fatalf("load live test configuration: %v", errConfig)
	}
	results, errRun := runLiveDriver(context.Background(), cfg)
	if errRun != nil {
		t.Fatalf("run live test driver: %v", errRun)
	}

	var mismatches []string
	for _, result := range results {
		if result.Evidence.Matched {
			continue
		}
		mismatches = append(mismatches, fmt.Sprintf(
			"%s outcome=%s status=%d category=%s",
			result.Evidence.RequestID,
			result.Evidence.Outcome,
			result.Evidence.StatusCode,
			result.Evidence.ErrorCategory,
		))
	}
	if len(mismatches) > 0 {
		t.Fatalf("%d live requests did not match expectations: %s", len(mismatches), strings.Join(mismatches, "; "))
	}
}

func loadLiveConfig(getenv func(string) string) (liveConfig, error) {
	baseURL, errURL := parseLoopbackBaseURL(getenv("LIVE_TEST_BASE_URL"))
	if errURL != nil {
		return liveConfig{}, errURL
	}
	apiKey := getenv("LIVE_TEST_API_KEY")
	if apiKey == "" {
		return liveConfig{}, errors.New("LIVE_TEST_API_KEY is required")
	}
	protocol := liveProtocol(getenvDefault(getenv, "LIVE_TEST_PROTOCOL", string(liveProtocolChat)))
	defaultPath, ok := defaultLivePath(protocol)
	if !ok {
		return liveConfig{}, fmt.Errorf("unsupported LIVE_TEST_PROTOCOL %q", protocol)
	}
	path := getenvDefault(getenv, "LIVE_TEST_PATH", defaultPath)
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return liveConfig{}, errors.New("LIVE_TEST_PATH must be an absolute HTTP path")
	}
	if parsedPath, errPath := url.ParseRequestURI(path); errPath != nil || parsedPath.IsAbs() || parsedPath.Host != "" || parsedPath.RawQuery != "" || parsedPath.Fragment != "" {
		return liveConfig{}, errors.New("LIVE_TEST_PATH must contain only an absolute path without a scheme, host, query, or fragment")
	}
	model := strings.TrimSpace(getenv("LIVE_TEST_MODEL"))
	if model == "" {
		return liveConfig{}, errors.New("LIVE_TEST_MODEL is required")
	}
	evidencePath := strings.TrimSpace(getenv("LIVE_TEST_EVIDENCE_PATH"))
	if evidencePath == "" {
		return liveConfig{}, errors.New("LIVE_TEST_EVIDENCE_PATH is required")
	}
	stream, errStream := parseBoolEnv(getenvDefault(getenv, "LIVE_TEST_STREAM", "false"))
	if errStream != nil {
		return liveConfig{}, fmt.Errorf("parse LIVE_TEST_STREAM: %w", errStream)
	}
	concurrency, errConcurrency := parsePositiveInt(getenvDefault(getenv, "LIVE_TEST_CONCURRENCY", "1"), 1000)
	if errConcurrency != nil {
		return liveConfig{}, fmt.Errorf("parse LIVE_TEST_CONCURRENCY: %w", errConcurrency)
	}
	rounds, errRounds := parsePositiveInt(getenvDefault(getenv, "LIVE_TEST_ROUNDS", "1"), 1000)
	if errRounds != nil {
		return liveConfig{}, fmt.Errorf("parse LIVE_TEST_ROUNDS: %w", errRounds)
	}
	requestTimeout, errTimeout := time.ParseDuration(getenvDefault(getenv, "LIVE_TEST_REQUEST_TIMEOUT", "30s"))
	if errTimeout != nil || requestTimeout <= 0 {
		return liveConfig{}, errors.New("LIVE_TEST_REQUEST_TIMEOUT must be a positive duration")
	}
	cancelAfter, errCancel := parseOptionalDuration(getenv("LIVE_TEST_CANCEL_AFTER"))
	if errCancel != nil {
		return liveConfig{}, fmt.Errorf("parse LIVE_TEST_CANCEL_AFTER: %w", errCancel)
	}
	cancelAfterEvents, errCancelEvents := parseOptionalNonNegativeInt(getenv("LIVE_TEST_CANCEL_AFTER_EVENTS"), 1_000_000)
	if errCancelEvents != nil {
		return liveConfig{}, fmt.Errorf("parse LIVE_TEST_CANCEL_AFTER_EVENTS: %w", errCancelEvents)
	}
	if cancelAfterEvents > 0 && !stream {
		return liveConfig{}, errors.New("LIVE_TEST_CANCEL_AFTER_EVENTS requires LIVE_TEST_STREAM=true")
	}
	expectedStatuses, errStatuses := parseStatusMatcher(getenvDefault(getenv, "LIVE_TEST_EXPECT_STATUS", "200-299"))
	if errStatuses != nil {
		return liveConfig{}, fmt.Errorf("parse LIVE_TEST_EXPECT_STATUS: %w", errStatuses)
	}
	expectedOutcome := getenvDefault(getenv, "LIVE_TEST_EXPECT_OUTCOME", "success")
	if expectedOutcome != "success" && expectedOutcome != "failure" && expectedOutcome != "canceled" {
		return liveConfig{}, errors.New("LIVE_TEST_EXPECT_OUTCOME must be success, failure, or canceled")
	}
	runID := strings.TrimSpace(getenv("LIVE_TEST_RUN_ID"))
	if runID == "" {
		runID = fmt.Sprintf("live-%d", time.Now().UTC().UnixNano())
	}
	if !isSafeLabel(runID) {
		return liveConfig{}, errors.New("LIVE_TEST_RUN_ID may contain only letters, digits, dot, underscore, and hyphen")
	}
	scenario := strings.TrimSpace(getenvDefault(getenv, "LIVE_TEST_SCENARIO", string(protocol)))
	if !isSafeLabel(scenario) {
		return liveConfig{}, errors.New("LIVE_TEST_SCENARIO may contain only letters, digits, dot, underscore, and hyphen")
	}
	prompt := getenvDefault(getenv, "LIVE_TEST_PROMPT", liveTestPrompt)
	if prompt == "" {
		return liveConfig{}, errors.New("LIVE_TEST_PROMPT must not be empty")
	}

	return liveConfig{
		BaseURL:           baseURL,
		APIKey:            apiKey,
		Protocol:          protocol,
		Path:              path,
		Model:             model,
		Stream:            stream,
		Concurrency:       concurrency,
		Rounds:            rounds,
		RunID:             runID,
		Scenario:          scenario,
		EvidencePath:      evidencePath,
		RequestTimeout:    requestTimeout,
		CancelAfter:       cancelAfter,
		CancelAfterEvents: cancelAfterEvents,
		ExpectedStatuses:  expectedStatuses,
		ExpectedOutcome:   expectedOutcome,
		Prompt:            prompt,
		Client:            newLiveHTTPClient(nil),
	}, nil
}

func runLiveDriver(ctx context.Context, cfg liveConfig) ([]liveResult, error) {
	cfg.Client = newLiveHTTPClient(cfg.Client)
	writer, errWriter := newEvidenceWriter(cfg.EvidencePath, cfg.APIKey, cfg.Prompt, "Authorization: Bearer")
	if errWriter != nil {
		return nil, errWriter
	}
	defer func() {
		_ = writer.Close()
	}()

	results := make([]liveResult, 0, cfg.Concurrency*cfg.Rounds)
	var resultsMu sync.Mutex
	var writerErr error
	var writerErrMu sync.Mutex

	for round := 1; round <= cfg.Rounds; round++ {
		ready := make(chan struct{}, cfg.Concurrency)
		start := make(chan struct{})
		var waitGroup sync.WaitGroup
		waitGroup.Add(cfg.Concurrency)
		for worker := 1; worker <= cfg.Concurrency; worker++ {
			round := round
			worker := worker
			go func() {
				defer waitGroup.Done()
				ready <- struct{}{}
				select {
				case <-start:
				case <-ctx.Done():
					return
				}
				result := executeLiveRequest(ctx, cfg, round, worker)
				if errWrite := writer.Write(result.Evidence); errWrite != nil {
					writerErrMu.Lock()
					if writerErr == nil {
						writerErr = errWrite
					}
					writerErrMu.Unlock()
				}
				resultsMu.Lock()
				results = append(results, result)
				resultsMu.Unlock()
			}()
		}
		for worker := 0; worker < cfg.Concurrency; worker++ {
			select {
			case <-ready:
			case <-ctx.Done():
				close(start)
				waitGroup.Wait()
				return nil, fmt.Errorf("wait for live request barrier: %w", ctx.Err())
			}
		}
		close(start)
		waitGroup.Wait()
	}

	writerErrMu.Lock()
	errEvidence := writerErr
	writerErrMu.Unlock()
	if errEvidence != nil {
		return nil, errEvidence
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, errClose
	}
	writer.file = nil

	sort.Slice(results, func(i, j int) bool {
		if results[i].Evidence.Round == results[j].Evidence.Round {
			return results[i].Evidence.Worker < results[j].Evidence.Worker
		}
		return results[i].Evidence.Round < results[j].Evidence.Round
	})
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if _, exists := seen[result.Evidence.RequestID]; exists {
			return nil, fmt.Errorf("duplicate live request ID %q", result.Evidence.RequestID)
		}
		seen[result.Evidence.RequestID] = struct{}{}
	}
	return results, nil
}

func executeLiveRequest(parent context.Context, cfg liveConfig, round, worker int) liveResult {
	requestID := fmt.Sprintf("%s-r%03d-w%03d", cfg.RunID, round, worker)
	record := liveEvidence{
		Timestamp:       time.Now().UTC(),
		Scenario:        cfg.Scenario,
		RunID:           cfg.RunID,
		Round:           round,
		Worker:          worker,
		RequestID:       requestID,
		Protocol:        string(cfg.Protocol),
		Stream:          cfg.Stream,
		ExpectedOutcome: cfg.ExpectedOutcome,
	}
	startedAt := time.Now()

	ctx, cancel := context.WithTimeout(parent, cfg.RequestTimeout)
	defer cancel()
	if cfg.CancelAfter > 0 {
		cancelTimer := time.AfterFunc(cfg.CancelAfter, cancel)
		defer cancelTimer.Stop()
	}
	payload, errPayload := buildLivePayload(cfg)
	if errPayload != nil {
		return finishLiveResult(record, startedAt, "failure", "invalid_request", "request payload could not be built", cfg, errPayload)
	}
	endpoint := cfg.BaseURL.ResolveReference(&url.URL{Path: cfg.Path})
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if errRequest != nil {
		return finishLiveResult(record, startedAt, "failure", "invalid_request", "request could not be created", cfg, errRequest)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(liveTestRequestHeader, requestID)
	if cfg.Protocol == liveProtocolAnthropic {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if cfg.Stream {
		request.Header.Set("Accept", "text/event-stream")
	}

	response, errDo := cfg.Client.Do(request)
	if errDo != nil {
		outcome, category := classifyRequestError(ctx, errDo)
		return finishLiveResult(record, startedAt, outcome, category, "request did not produce an HTTP response", cfg, errDo)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	record.StatusCode = response.StatusCode
	record.ContentType = normalizedContentType(response.Header.Get("Content-Type"))
	record.ServerRequestID = evidenceToken(firstNonEmptyHeader(response.Header, "X-Request-Id", "OpenAI-Request-Id", "X-CPA-Trace-Id"))

	if !cfg.ExpectedStatuses.Matches(response.StatusCode) {
		summary, _, errRead := readBoundedSummary(response.Body)
		if errRead != nil {
			outcome, category := classifyRequestError(ctx, errRead)
			return finishLiveResult(record, startedAt, outcome, category, summary, cfg, errRead)
		}
		return finishLiveResult(record, startedAt, "failure", "unexpected_status", summary, cfg, nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		summary, validJSON, errRead := readBoundedSummary(response.Body)
		if errRead != nil {
			outcome, category := classifyRequestError(ctx, errRead)
			return finishLiveResult(record, startedAt, outcome, category, summary, cfg, errRead)
		}
		if !validJSON {
			return finishLiveResult(record, startedAt, "failure", "invalid_json", summary, cfg, errors.New("error response was not valid JSON"))
		}
		return finishLiveResult(record, startedAt, "failure", "http_status", summary, cfg, nil)
	}

	if cfg.Stream {
		if record.ContentType != "text/event-stream" {
			summary, _, _ := readBoundedSummary(response.Body)
			return finishLiveResult(record, startedAt, "failure", "invalid_content_type", summary, cfg, nil)
		}
		streamResult, errStream := validateLiveSSE(ctx, cancel, response.Body, cfg.Protocol, cfg.CancelAfterEvents, startedAt)
		record.EventCount = streamResult.EventCount
		record.FirstEventMS = streamResult.FirstEvent.Milliseconds()
		record.Terminal = streamResult.Terminal
		record.ResponseSummary = streamResult.Summary
		if errStream != nil {
			outcome, category := classifyRequestError(ctx, errStream)
			if category == "transport" {
				category = "invalid_sse"
			}
			return finishLiveResult(record, startedAt, outcome, category, streamResult.Summary, cfg, errStream)
		}
		return finishLiveResult(record, startedAt, "success", "", streamResult.Summary, cfg, nil)
	}

	if !isJSONContentType(record.ContentType) {
		summary, _, _ := readBoundedSummary(response.Body)
		return finishLiveResult(record, startedAt, "failure", "invalid_content_type", summary, cfg, nil)
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxLiveResponseBytes+1))
	if errRead != nil {
		outcome, category := classifyRequestError(ctx, errRead)
		return finishLiveResult(record, startedAt, outcome, category, "JSON response read failed", cfg, errRead)
	}
	if len(body) > maxLiveResponseBytes {
		return finishLiveResult(record, startedAt, "failure", "response_too_large", summarizeBytes(body), cfg, errors.New("JSON response exceeded evidence limit"))
	}
	summary, errValidate := validateLiveJSON(cfg.Protocol, body)
	if errValidate != nil {
		return finishLiveResult(record, startedAt, "failure", "invalid_json", summary, cfg, errValidate)
	}
	return finishLiveResult(record, startedAt, "success", "", summary, cfg, nil)
}

func finishLiveResult(record liveEvidence, startedAt time.Time, outcome, category, summary string, cfg liveConfig, err error) liveResult {
	record.TotalMS = time.Since(startedAt).Milliseconds()
	record.Outcome = outcome
	record.ErrorCategory = category
	if summary != "" {
		record.ResponseSummary = summary
	}
	record.Matched = outcome == cfg.ExpectedOutcome
	if record.StatusCode != 0 && !cfg.ExpectedStatuses.Matches(record.StatusCode) {
		record.Matched = false
	}
	return liveResult{Evidence: record, Err: err}
}

func buildLivePayload(cfg liveConfig) ([]byte, error) {
	var payload any
	switch cfg.Protocol {
	case liveProtocolChat:
		chatPayload := map[string]any{
			"model":    cfg.Model,
			"messages": []map[string]string{{"role": "user", "content": cfg.Prompt}},
			"stream":   cfg.Stream,
		}
		if cfg.Stream {
			chatPayload["stream_options"] = map[string]bool{"include_usage": true}
		}
		payload = chatPayload
	case liveProtocolResponses:
		payload = map[string]any{
			"model":  cfg.Model,
			"input":  cfg.Prompt,
			"stream": cfg.Stream,
		}
	case liveProtocolAnthropic:
		payload = map[string]any{
			"model":      cfg.Model,
			"max_tokens": 64,
			"messages":   []map[string]string{{"role": "user", "content": cfg.Prompt}},
			"stream":     cfg.Stream,
		}
	default:
		return nil, fmt.Errorf("unsupported protocol %q", cfg.Protocol)
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal live request payload: %w", errMarshal)
	}
	return encoded, nil
}

type liveStreamResult struct {
	EventCount int
	FirstEvent time.Duration
	Terminal   string
	Summary    string
}

type sseValidationState struct {
	protocol         liveProtocol
	eventCount       int
	terminal         string
	terminalCount    int
	sawCreated       bool
	sawResponseDelta bool
	responseID       string
	sawMessageStart  bool
	sawMessageDelta  bool
	sawFinish        bool
	usageCount       int
	eventTypes       []string
	bytes            int
}

func validateLiveSSE(ctx context.Context, cancel context.CancelFunc, reader io.Reader, protocol liveProtocol, cancelAfterEvents int, startedAt time.Time) (liveStreamResult, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxLiveSSELineBytes)
	state := sseValidationState{protocol: protocol}
	var eventName string
	var dataLines []string
	var firstEvent time.Duration

	dispatch := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		state.eventCount++
		state.bytes += len(data)
		if firstEvent == 0 {
			firstEvent = time.Since(startedAt)
		}
		if errObserve := observeSSEEvent(&state, eventName, data); errObserve != nil {
			return errObserve
		}
		eventName = ""
		if cancelAfterEvents > 0 && state.eventCount >= cancelAfterEvents {
			cancel()
			return context.Canceled
		}
		return nil
	}

	streamBytes := 0
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		streamBytes += len(line) + 1
		if streamBytes > maxLiveResponseBytes {
			return streamResultFromState(state, firstEvent), errors.New("SSE response exceeded validation limit")
		}
		if line == "" {
			if errDispatch := dispatch(); errDispatch != nil {
				return streamResultFromState(state, firstEvent), errDispatch
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return streamResultFromState(state, firstEvent), errScan
	}
	if errDispatch := dispatch(); errDispatch != nil {
		return streamResultFromState(state, firstEvent), errDispatch
	}
	if errContext := ctx.Err(); errContext != nil {
		return streamResultFromState(state, firstEvent), errContext
	}
	if errTerminal := validateSSETerminal(state); errTerminal != nil {
		return streamResultFromState(state, firstEvent), errTerminal
	}
	return streamResultFromState(state, firstEvent), nil
}

func observeSSEEvent(state *sseValidationState, eventName, data string) error {
	if data == "[DONE]" {
		switch state.protocol {
		case liveProtocolChat:
			if state.terminalCount > 0 {
				return errors.New("duplicate chat terminal event")
			}
			state.terminal = "[DONE]"
			state.terminalCount++
			state.eventTypes = appendUniqueBounded(state.eventTypes, "[DONE]")
			return nil
		case liveProtocolResponses:
			return errors.New("Responses stream leaked an OpenAI [DONE] marker")
		case liveProtocolAnthropic:
			return errors.New("Anthropic stream leaked an OpenAI [DONE] marker")
		}
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal([]byte(data), &payload); errUnmarshal != nil {
		return errors.New("SSE data field was not valid JSON")
	}
	typeName, _ := payload["type"].(string)
	if eventName != "" && typeName != "" && eventName != typeName {
		return errors.New("SSE event name did not match the JSON type")
	}
	if eventName == "" {
		eventName = typeName
	}
	if eventName == "" {
		eventName = "data"
	}
	state.eventTypes = appendUniqueBounded(state.eventTypes, evidenceToken(eventName))

	switch state.protocol {
	case liveProtocolChat:
		if state.terminalCount > 0 {
			return errors.New("chat event arrived after terminal marker")
		}
		choices, ok := payload["choices"].([]any)
		if !ok {
			return errors.New("chat stream event did not contain choices")
		}
		for _, choiceValue := range choices {
			choice, _ := choiceValue.(map[string]any)
			if choice != nil && choice["finish_reason"] != nil {
				state.sawFinish = true
			}
		}
		if payload["usage"] != nil {
			state.usageCount++
			if state.usageCount > 1 {
				return errors.New("chat stream repeated usage")
			}
		}
	case liveProtocolResponses:
		if typeName == "" {
			typeName = eventName
		}
		if typeName == "error" {
			return errors.New("Responses stream contained an error event")
		}
		switch typeName {
		case "response.created":
			if state.sawCreated || state.terminalCount > 0 {
				return errors.New("response.created was duplicated or out of order")
			}
			state.sawCreated = true
			state.responseID = nestedString(payload, "response", "id")
		case "response.completed":
			if !state.sawCreated || state.terminalCount > 0 {
				return errors.New("response.completed was duplicated or out of order")
			}
			completedID := nestedString(payload, "response", "id")
			if state.responseID != "" && completedID != "" && state.responseID != completedID {
				return errors.New("response.completed did not match the response.created ID")
			}
			if status := nestedString(payload, "response", "status"); status != "" && status != "completed" {
				return fmt.Errorf("response.completed carried status %q", status)
			}
			state.terminal = "response.completed"
			state.terminalCount++
		case "response.failed", "response.incomplete":
			return fmt.Errorf("responses stream ended with %s", typeName)
		default:
			if !state.sawCreated {
				return errors.New("Responses event arrived before response.created")
			}
			if state.terminalCount > 0 {
				return errors.New("Responses event arrived after response.completed")
			}
			if strings.HasSuffix(typeName, ".delta") {
				state.sawResponseDelta = true
			}
		}
	case liveProtocolAnthropic:
		if typeName == "error" {
			return errors.New("Anthropic stream contained an error event")
		}
		switch typeName {
		case "message_start":
			if state.sawMessageStart || state.terminalCount > 0 {
				return errors.New("message_start was duplicated or out of order")
			}
			state.sawMessageStart = true
		case "message_stop":
			if !state.sawMessageStart || state.terminalCount > 0 {
				return errors.New("message_stop was duplicated or out of order")
			}
			state.terminal = "message_stop"
			state.terminalCount++
		case "ping":
			if state.terminalCount > 0 {
				return errors.New("Anthropic event arrived after message_stop")
			}
		default:
			if !state.sawMessageStart {
				return errors.New("Anthropic event arrived before message_start")
			}
			if state.terminalCount > 0 {
				return errors.New("Anthropic event arrived after message_stop")
			}
			if typeName == "content_block_delta" {
				state.sawMessageDelta = true
			}
		}
	}
	return nil
}

func nestedString(payload map[string]any, objectKey, valueKey string) string {
	object, _ := payload[objectKey].(map[string]any)
	value, _ := object[valueKey].(string)
	return value
}

func validateSSETerminal(state sseValidationState) error {
	if state.eventCount == 0 {
		return errors.New("SSE stream contained no data events")
	}
	if state.terminalCount != 1 {
		return fmt.Errorf("SSE stream terminal count = %d, want 1", state.terminalCount)
	}
	switch state.protocol {
	case liveProtocolChat:
		if !state.sawFinish {
			return errors.New("chat stream ended without a finish_reason")
		}
	case liveProtocolResponses:
		if !state.sawCreated {
			return errors.New("Responses stream ended without response.created")
		}
		if !state.sawResponseDelta {
			return errors.New("Responses stream ended without a delta event")
		}
	case liveProtocolAnthropic:
		if !state.sawMessageStart {
			return errors.New("Anthropic stream ended without message_start")
		}
		if !state.sawMessageDelta {
			return errors.New("Anthropic stream ended without a content_block_delta event")
		}
	}
	return nil
}

func streamResultFromState(state sseValidationState, firstEvent time.Duration) liveStreamResult {
	return liveStreamResult{
		EventCount: state.eventCount,
		FirstEvent: firstEvent,
		Terminal:   state.terminal,
		Summary: fmt.Sprintf(
			"sse events=%d types=%s bytes=%d",
			state.eventCount,
			strings.Join(state.eventTypes, ","),
			state.bytes,
		),
	}
}

func validateLiveJSON(protocol liveProtocol, body []byte) (string, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return summarizeBytes(body), errors.New("response body was not valid JSON")
	}
	if len(payload) == 0 {
		return summarizeJSON(payload, body), errors.New("response JSON object was empty")
	}
	switch protocol {
	case liveProtocolChat:
		choices, ok := payload["choices"].([]any)
		if !ok || len(choices) == 0 {
			return summarizeJSON(payload, body), errors.New("chat response did not contain choices")
		}
		choice, _ := choices[0].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)
		if choice == nil || finishReason == "" {
			return summarizeJSON(payload, body), errors.New("chat response did not contain a finish_reason")
		}
		message, _ := choice["message"].(map[string]any)
		content, _ := message["content"].(string)
		if content == "" {
			return summarizeJSON(payload, body), errors.New("chat response did not contain output text")
		}
		if !hasNumericUsage(payload, "total_tokens") {
			return summarizeJSON(payload, body), errors.New("chat response did not contain parseable usage")
		}
	case liveProtocolResponses:
		id, _ := payload["id"].(string)
		output, ok := payload["output"].([]any)
		if id == "" || !ok {
			return summarizeJSON(payload, body), errors.New("Responses response did not contain id and output")
		}
		if status, _ := payload["status"].(string); status != "completed" {
			return summarizeJSON(payload, body), fmt.Errorf("Responses status was %q, want completed", status)
		}
		if !responsesOutputContainsText(output) {
			return summarizeJSON(payload, body), errors.New("Responses response did not contain output text")
		}
		if !hasNumericUsage(payload, "total_tokens") {
			return summarizeJSON(payload, body), errors.New("Responses response did not contain parseable usage")
		}
	case liveProtocolAnthropic:
		content, ok := payload["content"].([]any)
		stopReason, _ := payload["stop_reason"].(string)
		if !ok || stopReason == "" {
			return summarizeJSON(payload, body), errors.New("Anthropic response did not contain content and stop_reason")
		}
		if !anthropicContentContainsText(content) {
			return summarizeJSON(payload, body), errors.New("Anthropic response did not contain output text")
		}
		if !hasNumericUsage(payload, "input_tokens", "output_tokens") {
			return summarizeJSON(payload, body), errors.New("Anthropic response did not contain parseable usage")
		}
	default:
		return summarizeJSON(payload, body), fmt.Errorf("unsupported protocol %q", protocol)
	}
	return summarizeJSON(payload, body), nil
}

func hasNumericUsage(payload map[string]any, fields ...string) bool {
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		return false
	}
	for _, field := range fields {
		value, ok := usage[field].(float64)
		if !ok || value < 0 || value != float64(int64(value)) {
			return false
		}
	}
	return true
}

func responsesOutputContainsText(output []any) bool {
	for _, itemValue := range output {
		item, _ := itemValue.(map[string]any)
		content, _ := item["content"].([]any)
		for _, contentValue := range content {
			part, _ := contentValue.(map[string]any)
			if text, _ := part["text"].(string); text != "" {
				return true
			}
		}
	}
	return false
}

func anthropicContentContainsText(content []any) bool {
	for _, contentValue := range content {
		part, _ := contentValue.(map[string]any)
		if text, _ := part["text"].(string); text != "" {
			return true
		}
	}
	return false
}

func readBoundedSummary(reader io.Reader) (string, bool, error) {
	body, errRead := io.ReadAll(io.LimitReader(reader, maxLiveResponseBytes+1))
	if errRead != nil {
		return "response read failed", false, errRead
	}
	if len(body) > maxLiveResponseBytes {
		return summarizeBytes(body), false, errors.New("response exceeded evidence limit")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return summarizeBytes(body), false, nil
	}
	return summarizeJSON(payload, body), true, nil
}

func summarizeJSON(payload map[string]any, body []byte) string {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, evidenceToken(key))
	}
	sort.Strings(keys)
	if len(keys) > 12 {
		keys = keys[:12]
	}
	return fmt.Sprintf("json keys=%s bytes=%d sha256=%s", strings.Join(keys, ","), len(body), shortHash(body))
}

func summarizeBytes(body []byte) string {
	return fmt.Sprintf("body bytes=%d sha256=%s", len(body), shortHash(body))
}

func shortHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:8])
}

func classifyRequestError(ctx context.Context, err error) (string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "canceled", "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "failure", "timeout"
	}
	return "failure", "transport"
}

func newLiveHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	client := *base
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func parseLoopbackBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("LIVE_TEST_BASE_URL is required")
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("LIVE_TEST_BASE_URL must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("LIVE_TEST_BASE_URL must not contain userinfo, query, or fragment")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("LIVE_TEST_BASE_URL must use a loopback host")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/"
	return parsed, nil
}

func defaultLivePath(protocol liveProtocol) (string, bool) {
	switch protocol {
	case liveProtocolChat:
		return "/v1/chat/completions", true
	case liveProtocolResponses:
		return "/v1/responses", true
	case liveProtocolAnthropic:
		return "/v1/messages", true
	default:
		return "", false
	}
}

func parseStatusMatcher(raw string) (statusMatcher, error) {
	var matcher statusMatcher
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("empty status expression")
		}
		minRaw, maxRaw, ranged := strings.Cut(part, "-")
		min, errMin := strconv.Atoi(minRaw)
		if errMin != nil || min < 100 || min > 599 {
			return nil, fmt.Errorf("invalid status %q", minRaw)
		}
		max := min
		if ranged {
			var errMax error
			max, errMax = strconv.Atoi(maxRaw)
			if errMax != nil || max < min || max > 599 {
				return nil, fmt.Errorf("invalid status range %q", part)
			}
		}
		matcher = append(matcher, statusRange{Min: min, Max: max})
	}
	return matcher, nil
}

func parsePositiveInt(raw string, max int) (int, error) {
	value, errParse := strconv.Atoi(raw)
	if errParse != nil || value <= 0 || value > max {
		return 0, fmt.Errorf("must be an integer between 1 and %d", max)
	}
	return value, nil
}

func parseOptionalNonNegativeInt(raw string, max int) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, errParse := strconv.Atoi(raw)
	if errParse != nil || value < 0 || value > max {
		return 0, fmt.Errorf("must be an integer between 0 and %d", max)
	}
	return value, nil
}

func parseOptionalDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	value, errParse := time.ParseDuration(raw)
	if errParse != nil || value <= 0 {
		return 0, errors.New("must be a positive duration")
	}
	return value, nil
}

func parseBoolEnv(raw string) (bool, error) {
	value, errParse := strconv.ParseBool(raw)
	if errParse != nil {
		return false, errors.New("must be true or false")
	}
	return value, nil
}

func getenvDefault(getenv func(string) string, key, fallback string) string {
	if value := getenv(key); value != "" {
		return value
	}
	return fallback
}

func normalizedContentType(raw string) string {
	if index := strings.IndexByte(raw, ';'); index >= 0 {
		raw = raw[:index]
	}
	return evidenceToken(strings.ToLower(strings.TrimSpace(raw)))
}

func isJSONContentType(contentType string) bool {
	return contentType == "application/json" || strings.HasSuffix(contentType, "+json")
}

func firstNonEmptyHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			if len(value) > 128 {
				return value[:128]
			}
			return value
		}
	}
	return ""
}

func evidenceToken(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 128 {
		safe := true
		for _, char := range value {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._:/+-", char) {
				continue
			}
			safe = false
			break
		}
		if safe {
			return value
		}
	}
	return "sha256:" + shortHash([]byte(value))
}

func appendUniqueBounded(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	if len(values) >= 16 {
		return values
	}
	return append(values, value)
}

func isSafeLabel(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}
