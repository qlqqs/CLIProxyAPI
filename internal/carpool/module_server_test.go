package carpool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const (
	serverRouteOpenAIModel = "carpool-route-openai-model"
	serverRouteClaudeModel = "carpool-route-claude-model"
	serverRouteGeminiModel = "carpool-route-gemini-model"
	serverRouteCodexModel  = "carpool-route-codex-model"
)

type serverRouteRecorder struct {
	mu             sync.Mutex
	selectedAuths  []string
	candidateSets  [][]string
	failAuthID     string
	failureEmitted bool
}

func (r *serverRouteRecorder) recordCandidates(auths []*coreauth.Auth) *coreauth.Auth {
	ids := make([]string, 0, len(auths))
	byID := make(map[string]*coreauth.Auth, len(auths))
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		ids = append(ids, candidate.ID)
		byID[candidate.ID] = candidate
	}
	sort.Strings(ids)
	r.mu.Lock()
	r.candidateSets = append(r.candidateSets, append([]string(nil), ids...))
	r.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	return byID[ids[0]]
}

func (r *serverRouteRecorder) recordExecution(authID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selectedAuths = append(r.selectedAuths, authID)
	if authID == r.failAuthID && !r.failureEmitted {
		r.failureEmitted = true
		return true
	}
	return false
}

func (r *serverRouteRecorder) failOnce(authID string) {
	r.mu.Lock()
	r.failAuthID = authID
	r.failureEmitted = false
	r.mu.Unlock()
}

func (r *serverRouteRecorder) snapshot() (selected []string, candidates [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	selected = append([]string(nil), r.selectedAuths...)
	candidates = make([][]string, len(r.candidateSets))
	for index := range r.candidateSets {
		candidates[index] = append([]string(nil), r.candidateSets[index]...)
	}
	return selected, candidates
}

type serverRouteSelector struct {
	recorder *serverRouteRecorder
}

func (s serverRouteSelector) Pick(_ context.Context, _, _ string, _ coreexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return s.recorder.recordCandidates(auths), nil
}

type serverRouteExecutor struct {
	provider string
	recorder *serverRouteRecorder
}

func (e *serverRouteExecutor) Identifier() string { return e.provider }

func (e *serverRouteExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	if e.record(auth) {
		return coreexecutor.Response{}, &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "test retry"}
	}
	return coreexecutor.Response{Payload: serverRouteResponsePayload()}, nil
}

func (e *serverRouteExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	if e.record(auth) {
		return nil, &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "test retry"}
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: serverRouteStreamPayload(coreexecutor.ResponseFormatOrSource(opts))}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *serverRouteExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *serverRouteExecutor) CountTokens(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(auth)
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":1}`)}, nil
}

func (e *serverRouteExecutor) HttpRequest(_ context.Context, auth *coreauth.Auth, _ *http.Request) (*http.Response, error) {
	e.record(auth)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
}

func (e *serverRouteExecutor) record(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return e.recorder.recordExecution(auth.ID)
}

func serverRouteResponsePayload() []byte {
	return []byte(`{"id":"route-response","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"content":[],"candidates":[]}`)
}

func serverRouteStreamPayload(format sdktranslator.Format) []byte {
	switch format {
	case sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex:
		return []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"route-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	case sdktranslator.FormatClaude:
		return []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	case sdktranslator.FormatGemini:
		return []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}]}`)
	case sdktranslator.FormatInteractions:
		return []byte(`{"id":"route-interaction","status":"completed"}`)
	default:
		return []byte(`{"id":"route-chunk","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	}
}

type serverRouteFixture struct {
	module      *Module
	server      *api.Server
	engine      *gin.Engine
	recorder    *serverRouteRecorder
	accessToken string
	passenger   domain.User
	apiKeyID    string
}

func newServerRouteFixture(t *testing.T, homeEnabled bool) *serverRouteFixture {
	t.Helper()
	ctx := context.Background()
	root, databasePath, configPath := moduleTestPaths(t)
	cfg := moduleTestConfig(databasePath, true)
	cfg.Debug = true
	cfg.AuthDir = filepath.Join(root, "auth")
	cfg.RequestRetry = 2
	cfg.MaxRetryCredentials = 4
	cfg.WebsocketAuth = true
	cfg.Home.Enabled = homeEnabled

	recorder := &serverRouteRecorder{}
	manager := coreauth.NewManager(nil, serverRouteSelector{recorder: recorder}, nil)
	for _, provider := range []string{"openai", "claude", "gemini", "codex"} {
		manager.RegisterExecutor(&serverRouteExecutor{provider: provider, recorder: recorder})
	}

	module, errOpen := Open(ctx, cfg, configPath, manager)
	if errOpen != nil {
		t.Fatalf("Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := module.Close(context.Background()); errClose != nil {
			t.Errorf("Module.Close() error = %v", errClose)
		}
		if metrics := module.writer.Snapshot(); metrics.WriteFailures != 0 {
			t.Errorf("accounting write failures = %d, want 0", metrics.WriteFailures)
		}
	})

	admin, errAdmin := module.store.BootstrapAdmin(ctx, domain.User{
		Username:           "server-route-admin",
		DefaultDisplayName: "Server Route Admin",
		Role:               domain.UserRoleAdmin,
		Status:             domain.UserStatusActive,
		PasswordHash:       "$argon2id$test",
	}, nil)
	if errAdmin != nil {
		t.Fatalf("BootstrapAdmin() error = %v", errAdmin)
	}
	passenger, errPassenger := module.store.CreateUser(ctx, domain.User{
		Username:           "server-route-passenger",
		DefaultDisplayName: "Server Route Passenger",
		Role:               domain.UserRolePassenger,
		Status:             domain.UserStatusActive,
		PasswordHash:       "$argon2id$test",
	})
	if errPassenger != nil {
		t.Fatalf("CreateUser() error = %v", errPassenger)
	}
	secret, errSecret := carpoolservice.NewUserAPIKeySecret(nil)
	if errSecret != nil {
		t.Fatalf("NewUserAPIKeySecret() error = %v", errSecret)
	}
	apiKey, errKey := module.store.CreateAPIKey(ctx, domain.APIKey{
		KeyID: secret.KeyID, UserID: passenger.ID, Name: "server routes", SecretDigest: secret.Digest,
	})
	if errKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errKey)
	}
	carA, errCarA := module.store.CreateCar(ctx, domain.Car{Name: "Server Route A", Status: domain.CarStatusActive})
	if errCarA != nil {
		t.Fatalf("CreateCar(A) error = %v", errCarA)
	}
	carB, errCarB := module.store.CreateCar(ctx, domain.Car{Name: "Server Route B", Status: domain.CarStatusActive})
	if errCarB != nil {
		t.Fatalf("CreateCar(B) error = %v", errCarB)
	}
	if _, errMember := module.store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: carA.ID, DisplayName: "Route Passenger", DisplayNameKey: "route passenger", CreatedByUserID: admin.ID,
	}}); errMember != nil {
		t.Fatalf("MoveMembership() error = %v", errMember)
	}

	credentials := []struct {
		id       string
		provider string
		model    string
		carID    string
	}{
		{id: "route-a-openai-1", provider: "openai", model: serverRouteOpenAIModel, carID: carA.ID},
		{id: "route-a-openai-2", provider: "openai", model: serverRouteOpenAIModel, carID: carA.ID},
		{id: "route-a-claude", provider: "claude", model: serverRouteClaudeModel, carID: carA.ID},
		{id: "route-a-gemini", provider: "gemini", model: serverRouteGeminiModel, carID: carA.ID},
		{id: "route-a-codex", provider: "codex", model: serverRouteCodexModel, carID: carA.ID},
		{id: "route-b-openai", provider: "openai", model: serverRouteOpenAIModel, carID: carB.ID},
		{id: "route-b-claude", provider: "claude", model: serverRouteClaudeModel, carID: carB.ID},
		{id: "route-b-gemini", provider: "gemini", model: serverRouteGeminiModel, carID: carB.ID},
		{id: "route-b-codex", provider: "codex", model: serverRouteCodexModel, carID: carB.ID},
	}
	for index, credential := range credentials {
		if _, errRegister := manager.Register(ctx, &coreauth.Auth{
			ID: credential.id, Provider: credential.provider, Status: coreauth.StatusActive,
			Metadata: map[string]any{"disable_cooling": true},
		}); errRegister != nil {
			t.Fatalf("register auth %q: %v", credential.id, errRegister)
		}
		models := []*registry.ModelInfo{{ID: credential.model}}
		if strings.HasPrefix(credential.id, "route-b-") {
			models = append(models, &registry.ModelInfo{ID: "carpool-route-outside-" + credential.provider})
		}
		registry.GetGlobalRegistry().RegisterClient(credential.id, credential.provider, models)
		credentialID := credential.id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credentialID) })
		if _, errAssignment := module.store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
			CarID: credential.carID, AuthID: credential.id, SafeLabel: "Route account " + string(rune('A'+index)),
			ProviderSnapshot: credential.provider, CreatedByUserID: admin.ID,
		}}); errAssignment != nil {
			t.Fatalf("MoveAuthAssignment(%q) error = %v", credential.id, errAssignment)
		}
	}

	accessManager := sdkaccess.NewManager()
	var engine *gin.Engine
	options := append(module.ServerOptions(),
		api.WithEngineConfigurator(func(configured *gin.Engine) { engine = configured }),
		api.WithRequestLoggerFactory(nil),
	)
	server := api.NewServer(cfg, manager, accessManager, configPath, options...)
	if engine == nil {
		t.Fatal("WithEngineConfigurator did not capture the Gin engine")
	}
	accessManager.SetProviders([]sdkaccess.Provider{module.Provider()})
	accessManager.SetAuthenticatedRequestHook(module.AuthenticatedRequestHook)
	return &serverRouteFixture{
		module: module, server: server, engine: engine, recorder: recorder,
		accessToken: secret.Token, passenger: passenger, apiKeyID: apiKey.KeyID,
	}
}

func (f *serverRouteFixture) request(t *testing.T, method, path, body string, upgrade bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+f.accessToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if upgrade {
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
	}
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	return recorder
}

func TestCarpoolServerRoutesAllowScopedHTTPProtocols(t *testing.T) {
	fixture := newServerRouteFixture(t, false)
	tests := []struct {
		name   string
		path   string
		body   string
		stream bool
	}{
		{name: "OpenAI chat", path: "/v1/chat/completions", body: `{"model":"` + serverRouteOpenAIModel + `","messages":[]}`},
		{name: "OpenAI chat stream", path: "/v1/chat/completions", body: `{"model":"` + serverRouteOpenAIModel + `","messages":[],"stream":true}`, stream: true},
		{name: "OpenAI completions", path: "/v1/completions", body: `{"model":"` + serverRouteOpenAIModel + `","prompt":"test"}`},
		{name: "OpenAI completions stream", path: "/v1/completions", body: `{"model":"` + serverRouteOpenAIModel + `","prompt":"test","stream":true}`, stream: true},
		{name: "OpenAI responses", path: "/v1/responses", body: `{"model":"` + serverRouteOpenAIModel + `","input":"test"}`},
		{name: "OpenAI responses stream", path: "/v1/responses", body: `{"model":"` + serverRouteOpenAIModel + `","input":"test","stream":true}`, stream: true},
		{name: "Claude messages", path: "/v1/messages", body: `{"model":"` + serverRouteClaudeModel + `","messages":[],"max_tokens":16}`},
		{name: "Claude messages stream", path: "/v1/messages", body: `{"model":"` + serverRouteClaudeModel + `","messages":[],"max_tokens":16,"stream":true}`, stream: true},
		{name: "Codex responses", path: "/backend-api/codex/responses", body: `{"model":"` + serverRouteCodexModel + `","input":"test"}`},
		{name: "Gemini generate", path: "/v1beta/models/" + serverRouteGeminiModel + ":generateContent", body: `{"contents":[]}`},
		{name: "Gemini stream", path: "/v1beta/models/" + serverRouteGeminiModel + ":streamGenerateContent?alt=sse", body: `{"contents":[]}`, stream: true},
		{name: "Gemini interactions", path: "/v1beta/interactions", body: `{"model":"` + serverRouteGeminiModel + `","input":"test"}`},
		{name: "Gemini interactions stream", path: "/v1beta/interactions", body: `{"model":"` + serverRouteGeminiModel + `","input":"test","stream":true}`, stream: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.request(t, http.MethodPost, test.path, test.body, false)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
		})
	}

	selected, candidates := fixture.recorder.snapshot()
	if len(selected) != len(tests) {
		t.Fatalf("upstream execution count = %d, want %d; selected=%v", len(selected), len(tests), selected)
	}
	assertOnlyCarAAuths(t, selected, candidates)
}

func TestCarpoolServerRoutesScopeModelQueries(t *testing.T) {
	fixture := newServerRouteFixture(t, false)
	tests := []struct {
		name       string
		path       string
		headers    http.Header
		wantModel  string
		outside    string
		wantStatus int
	}{
		{name: "OpenAI list", path: "/v1/models", wantModel: serverRouteOpenAIModel, outside: "carpool-route-outside-openai", wantStatus: http.StatusOK},
		{name: "Claude list", path: "/v1/models", headers: http.Header{"Anthropic-Version": []string{"2023-06-01"}}, wantModel: serverRouteClaudeModel, outside: "carpool-route-outside-claude", wantStatus: http.StatusOK},
		{name: "Gemini list", path: "/v1beta/models", wantModel: serverRouteGeminiModel, outside: "carpool-route-outside-gemini", wantStatus: http.StatusOK},
		{name: "Gemini read", path: "/v1beta/models/" + serverRouteGeminiModel, wantModel: serverRouteGeminiModel, outside: "carpool-route-outside-gemini", wantStatus: http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+fixture.accessToken)
			for name, values := range test.headers {
				request.Header[name] = append([]string(nil), values...)
			}
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.wantModel) {
				t.Fatalf("response does not contain allowed model %q: %s", test.wantModel, response.Body.String())
			}
			if strings.Contains(response.Body.String(), test.outside) {
				t.Fatalf("response exposes out-of-scope model %q: %s", test.outside, response.Body.String())
			}
		})
	}
}

func TestCarpoolServerRoutesRejectUnsupportedSurfacesBeforeExecution(t *testing.T) {
	fixture := newServerRouteFixture(t, false)
	fixture.server.AttachWebsocketRoute("/v1/ws", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		upgrade bool
	}{
		{name: "Responses websocket", method: http.MethodGet, path: "/v1/responses", upgrade: true},
		{name: "Realtime websocket", method: http.MethodGet, path: "/v1/realtime", upgrade: true},
		{name: "Realtime HTTP", method: http.MethodPost, path: "/v1/realtime", body: `{}`},
		{name: "Realtime calls", method: http.MethodPost, path: "/v1/realtime/calls", body: `{}`},
		{name: "Live", method: http.MethodPost, path: "/v1/live", body: `{}`},
		{name: "wsrelay", method: http.MethodGet, path: "/v1/ws", upgrade: true},
		{name: "Claude count", method: http.MethodPost, path: "/v1/messages/count_tokens", body: `{"model":"` + serverRouteClaudeModel + `"}`},
		{name: "Responses compact", method: http.MethodPost, path: "/v1/responses/compact", body: `{"model":"` + serverRouteOpenAIModel + `"}`},
		{name: "Codex compact", method: http.MethodPost, path: "/backend-api/codex/responses/compact", body: `{"model":"` + serverRouteCodexModel + `"}`},
		{name: "Codex search", method: http.MethodPost, path: "/v1/alpha/search", body: `{"model":"` + serverRouteCodexModel + `"}`},
		{name: "Codex direct search", method: http.MethodPost, path: "/backend-api/codex/alpha/search", body: `{"model":"` + serverRouteCodexModel + `"}`},
		{name: "Gemini count", method: http.MethodPost, path: "/v1beta/models/" + serverRouteGeminiModel + ":countTokens", body: `{}`},
		{name: "OpenAI image", method: http.MethodPost, path: "/v1/images/generations", body: `{"model":"` + serverRouteOpenAIModel + `"}`},
		{name: "OpenAI video", method: http.MethodPost, path: "/v1/videos", body: `{"model":"` + serverRouteOpenAIModel + `"}`},
		{name: "OpenAI compatibility video", method: http.MethodPost, path: "/openai/v1/videos", body: `{"model":"` + serverRouteOpenAIModel + `"}`},
	}
	selectedBefore, _ := fixture.recorder.snapshot()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.request(t, test.method, test.path, test.body, test.upgrade)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusForbidden, response.Body.String())
			}
			if strings.Contains(response.Body.String(), fixture.accessToken) {
				t.Fatalf("response exposes user API key: %s", response.Body.String())
			}
		})
	}
	selectedAfter, _ := fixture.recorder.snapshot()
	if len(selectedAfter) != len(selectedBefore) {
		t.Fatalf("unsupported routes reached upstream: before=%v after=%v", selectedBefore, selectedAfter)
	}
}

func TestCarpoolServerRetryRemainsInsideFrozenCarScope(t *testing.T) {
	fixture := newServerRouteFixture(t, false)
	fixture.recorder.failOnce("route-a-openai-1")
	response := fixture.request(t, http.MethodPost, "/v1/chat/completions", `{"model":"`+serverRouteOpenAIModel+`","messages":[]}`, false)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	selected, candidates := fixture.recorder.snapshot()
	if len(selected) < 2 {
		t.Fatalf("retry did not execute a second attempt: %v", selected)
	}
	assertOnlyCarAAuths(t, selected, candidates)
}

func TestCarpoolHomeAuthorizationIsRejectedBeforeExecution(t *testing.T) {
	fixture := newServerRouteFixture(t, true)
	_, errAuthorize := fixture.module.Control().AuthorizeProxy(
		context.Background(), fixture.passenger.ID, fixture.apiKeyID, "home-test", http.MethodPost, "/v1/chat/completions", false,
	)
	if !errors.Is(errAuthorize, domain.ErrAuthorizationRejected) {
		t.Fatalf("AuthorizeProxy() error = %v, want ErrAuthorizationRejected", errAuthorize)
	}
	selected, candidates := fixture.recorder.snapshot()
	if len(selected) != 0 || len(candidates) != 0 {
		t.Fatalf("Home rejection reached selection or execution: selected=%v candidates=%v", selected, candidates)
	}
}

func assertOnlyCarAAuths(t *testing.T, selected []string, candidates [][]string) {
	t.Helper()
	for _, authID := range selected {
		if !strings.HasPrefix(authID, "route-a-") {
			t.Fatalf("selected out-of-scope auth %q from %v", authID, selected)
		}
	}
	for _, candidateSet := range candidates {
		for _, authID := range candidateSet {
			if !strings.HasPrefix(authID, "route-a-") {
				t.Fatalf("selector received out-of-scope auth %q in %v", authID, candidateSet)
			}
		}
	}
}

var _ coreauth.Selector = serverRouteSelector{}
var _ coreauth.ProviderExecutor = (*serverRouteExecutor)(nil)
var _ sdkaccess.Provider = (*carpoolaccess.Provider)(nil)
