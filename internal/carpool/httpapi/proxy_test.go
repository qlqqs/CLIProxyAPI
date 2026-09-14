package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type proxyCaptureExecutor struct {
	provider string
	options  coreexecutor.Options
}

func (e *proxyCaptureExecutor) Identifier() string {
	return e.provider
}

func (e *proxyCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
	e.options = options
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *proxyCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, options coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.options = options
	chunks := make(chan coreexecutor.StreamChunk)
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *proxyCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *proxyCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
	e.options = options
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":0}`)}, nil
}

func (*proxyCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type proxyAuthorizationFixture struct {
	api           *API
	store         *carpoolsqlite.Store
	accessManager *sdkaccess.Manager
	upstream      *coreauth.Manager
	executor      *proxyCaptureExecutor
	token         string
	authID        string
	model         string
}

func newProxyAuthorizationFixture(t *testing.T) proxyAuthorizationFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }
	store, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{
		Path: filepath.Join(t.TempDir(), "carpool.db"),
		Now:  nowFunc,
	})
	if errOpen != nil {
		t.Fatalf("sqlite.Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Store.Close() error = %v", errClose)
		}
	})

	admin, errAdmin := store.BootstrapAdmin(ctx, domain.User{
		Username:           "proxy-admin",
		DefaultDisplayName: "Proxy Admin",
		Role:               domain.UserRoleAdmin,
		Status:             domain.UserStatusActive,
		PasswordHash:       "$argon2id$test",
	}, nil)
	if errAdmin != nil {
		t.Fatalf("BootstrapAdmin() error = %v", errAdmin)
	}
	passenger, errPassenger := store.CreateUser(ctx, domain.User{
		Username:           "proxy-passenger",
		DefaultDisplayName: "Proxy Passenger",
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
	apiKey, errKey := store.CreateAPIKey(ctx, domain.APIKey{
		KeyID:        secret.KeyID,
		UserID:       passenger.ID,
		Name:         "proxy test",
		SecretDigest: secret.Digest,
	})
	if errKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errKey)
	}
	car, errCar := store.CreateCar(ctx, domain.Car{Name: "proxy car", Status: domain.CarStatusActive})
	if errCar != nil {
		t.Fatalf("CreateCar() error = %v", errCar)
	}
	limitNanoUSD := int64(25_000_000_000)
	if _, errMember := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID:              passenger.ID,
		CarID:               car.ID,
		DisplayName:         "Passenger",
		DisplayNameKey:      "passenger",
		CreatedByUserID:     admin.ID,
		MonthlyLimitNanoUSD: &limitNanoUSD,
	}}); errMember != nil {
		t.Fatalf("MoveMembership() error = %v", errMember)
	}

	const (
		provider = "carpool-proxy-test-provider"
		authID   = "carpool-proxy-test-auth"
		model    = "carpool-proxy-test-model"
	)
	executor := &proxyCaptureExecutor{provider: provider}
	upstream := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	upstream.RegisterExecutor(executor)
	if _, errRegister := upstream.Register(ctx, &coreauth.Auth{
		ID:       authID,
		Provider: provider,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"disable_cooling": true},
	}); errRegister != nil {
		t.Fatalf("upstream.Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	if _, errAssignment := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID:            car.ID,
		AuthID:           authID,
		SafeLabel:        "Primary",
		SafeLabelKey:     "primary",
		ProviderSnapshot: provider,
		CreatedByUserID:  admin.ID,
	}}); errAssignment != nil {
		t.Fatalf("MoveAuthAssignment() error = %v", errAssignment)
	}

	hasher := carpoolservice.PasswordHasher{
		Params: carpoolservice.PasswordParams{
			Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
		},
		Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 16)),
	}
	control, errControl := carpoolservice.NewControl(store, upstream, carpoolservice.ControlConfig{
		SessionAbsoluteTTL: 24 * time.Hour,
		SessionIdleTTL:     2 * time.Hour,
		ReportLocation:     time.UTC,
		UsageRetention:     90 * 24 * time.Hour,
		Now:                nowFunc,
		PasswordHasher:     hasher,
	})
	if errControl != nil {
		t.Fatalf("NewControl() error = %v", errControl)
	}
	api, errAPI := New(control, Config{SessionTTL: 24 * time.Hour})
	if errAPI != nil {
		t.Fatalf("New() error = %v", errAPI)
	}
	accessManager := sdkaccess.NewManager()
	accessManager.SetProviders([]sdkaccess.Provider{carpoolaccess.NewProvider(store, nowFunc)})
	accessManager.SetAuthenticatedRequestHook(api.AuthenticatedRequestHook)

	if apiKey.KeyID != secret.KeyID {
		t.Fatalf("stored API key ID = %q, want %q", apiKey.KeyID, secret.KeyID)
	}
	return proxyAuthorizationFixture{
		api: api, store: store, accessManager: accessManager, upstream: upstream,
		executor: executor, token: secret.Token, authID: authID, model: model,
	}
}

func TestAuthenticatedRequestHookFreezesSnapshotScopeAndLifecycleID(t *testing.T) {
	fixture := newProxyAuthorizationFixture(t)
	request := httptest.NewRequest(http.MethodPost, "https://proxy.test/v1/chat/completions", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.token)

	result, authErr := fixture.accessManager.Authenticate(request.Context(), request)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if result == nil || result.Provider != carpoolaccess.ProviderName {
		t.Fatalf("Authenticate() result = %#v", result)
	}

	snapshot, ok := carpoolruntime.AuthorizationFromContext(request.Context())
	if !ok || snapshot == nil {
		t.Fatal("request context is missing carpool authorization snapshot")
	}
	if snapshot.RequestID() == "" || snapshot.CallerScope() != result.Principal {
		t.Fatalf("authorization snapshot request ID = %q, caller scope = %q", snapshot.RequestID(), snapshot.CallerScope())
	}
	if got := snapshot.AuthIDs(); !reflect.DeepEqual(got, []string{fixture.authID}) {
		t.Fatalf("snapshot AuthIDs() = %v, want [%s]", got, fixture.authID)
	}
	scope := coreexecutor.CredentialScopeFromContext(request.Context())
	if scope == nil || !reflect.DeepEqual(scope.IDs(), []string{fixture.authID}) {
		t.Fatalf("request credential scope = %#v, IDs = %v", scope, scope.IDs())
	}

	persisted, errRequest := fixture.store.GetProxyRequest(request.Context(), snapshot.RequestID())
	if errRequest != nil {
		t.Fatalf("GetProxyRequest() error = %v", errRequest)
	}
	if persisted.Outcome != domain.RequestOutcomeInProgress || persisted.RequestID != snapshot.RequestID() || persisted.ScopeSize != 1 {
		t.Fatalf("persisted proxy request = %#v", persisted)
	}
	persistedScopes, errScopes := fixture.store.ListRequestScopes(request.Context(), snapshot.RequestID())
	if errScopes != nil {
		t.Fatalf("ListRequestScopes() error = %v", errScopes)
	}
	if len(persistedScopes) != 1 || persistedScopes[0].AuthID != fixture.authID {
		t.Fatalf("persisted scopes = %#v", persistedScopes)
	}

	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, fixture.upstream)
	body := []byte(`{"model":"` + fixture.model + `"}`)
	if _, _, errMessage := handler.ExecuteWithAuthManager(request.Context(), "openai", fixture.model, body, ""); errMessage != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMessage)
	}
	if fixture.executor.options.RequestID != snapshot.RequestID() {
		t.Fatalf("executor request ID = %q, want %q", fixture.executor.options.RequestID, snapshot.RequestID())
	}
	if fixture.executor.options.CredentialScope != scope {
		t.Fatalf("executor credential scope = %#v, want context scope %#v", fixture.executor.options.CredentialScope, scope)
	}
}

func TestAuthenticatedRequestHookLeavesLegacyGlobalResultUnscoped(t *testing.T) {
	fixture := newProxyAuthorizationFixture(t)
	request := httptest.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
	result := &sdkaccess.Result{Provider: "config-inline", Principal: "legacy-global-key"}

	if authErr := fixture.api.AuthenticatedRequestHook(request.Context(), request, result); authErr != nil {
		t.Fatalf("AuthenticatedRequestHook() error = %v", authErr)
	}
	if snapshot, ok := carpoolruntime.AuthorizationFromContext(request.Context()); ok || snapshot != nil {
		t.Fatalf("legacy request authorization snapshot = %#v, present = %t", snapshot, ok)
	}
	if scope := coreexecutor.CredentialScopeFromContext(request.Context()); scope != nil {
		t.Fatalf("legacy request credential scope = %#v, want nil", scope)
	}
}

type proxyGuardTestProvider struct {
	calls int
}

func (*proxyGuardTestProvider) Identifier() string { return "guard-test" }

func (p *proxyGuardTestProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	p.calls++
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: "principal"}, nil
}

func TestProxyCredentialGuardRejectsConflictsBeforeAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixture := newProxyAuthorizationFixture(t)
	provider := &proxyGuardTestProvider{}
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{provider})
	engine := gin.New()
	engine.Use(fixture.api.ProxyCredentialGuard())
	engine.GET("/v1/models", func(c *gin.Context) {
		_, _ = manager.Authenticate(c.Request.Context(), c.Request)
		c.Status(http.StatusNoContent)
	})

	conflicting := httptest.NewRequest(http.MethodGet, "/v1/models?key=query-key", nil)
	conflicting.Header.Set("Authorization", "Bearer header-key")
	conflicting.Header.Set("X-Api-Key", "other-key")
	conflictResponse := httptest.NewRecorder()
	engine.ServeHTTP(conflictResponse, conflicting)
	if conflictResponse.Code != http.StatusUnauthorized {
		t.Fatalf("conflicting status = %d, want %d", conflictResponse.Code, http.StatusUnauthorized)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls after conflict = %d, want 0", provider.calls)
	}
	for _, secret := range []string{"query-key", "header-key", "other-key"} {
		if strings.Contains(conflictResponse.Body.String(), secret) {
			t.Fatalf("conflict response exposes credential %q: %s", secret, conflictResponse.Body.String())
		}
	}
	audits, errAudits := fixture.store.ListAuditEvents(t.Context(), time.Time{}, "", 10)
	if errAudits != nil {
		t.Fatalf("ListAuditEvents() error = %v", errAudits)
	}
	if len(audits) != 1 || audits[0].Action != "authentication_reject" || audits[0].ReasonCode != "conflicting_credentials" {
		t.Fatalf("credential conflict audits = %#v", audits)
	}
	for _, secret := range []string{"query-key", "header-key", "other-key"} {
		if strings.Contains(audits[0].ActorRef, secret) || strings.Contains(string(audits[0].MetadataJSON), secret) {
			t.Fatalf("credential conflict audit exposes credential %q: %#v", secret, audits[0])
		}
	}

	deduplicated := httptest.NewRequest(http.MethodGet, "/v1/models?key=shared-key&auth_token=shared-key", nil)
	deduplicated.Header.Set("Authorization", "Bearer shared-key")
	deduplicated.Header.Set("X-Api-Key", "shared-key")
	deduplicated.Header.Set("X-Goog-Api-Key", "shared-key")
	deduplicatedResponse := httptest.NewRecorder()
	engine.ServeHTTP(deduplicatedResponse, deduplicated)
	if deduplicatedResponse.Code != http.StatusNoContent {
		t.Fatalf("deduplicated status = %d, want %d", deduplicatedResponse.Code, http.StatusNoContent)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls after deduplicated request = %d, want 1", provider.calls)
	}
	audits, errAudits = fixture.store.ListAuditEvents(t.Context(), time.Time{}, "", 10)
	if errAudits != nil || len(audits) != 1 {
		t.Fatalf("audits after deduplicated credentials = (%#v, %v), want one conflict event", audits, errAudits)
	}
}

func TestScopedModelRequestCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixedNow := time.Date(2026, time.September, 4, 12, 30, 0, 0, time.FixedZone("test", 8*60*60))
	tests := []struct {
		name          string
		method        string
		path          string
		status        int
		withSnapshot  bool
		cancelContext bool
		wantCalls     int
		wantOutcome   pluginapi.RequestCompletionOutcome
		wantFormat    string
	}{
		{name: "OpenAI success", method: http.MethodGet, path: "/v1/models", status: http.StatusOK, withSnapshot: true, wantCalls: 1, wantOutcome: pluginapi.RequestCompletionSucceeded, wantFormat: "openai"},
		{name: "Gemini model not found", method: http.MethodGet, path: "/v1beta/models/missing", status: http.StatusNotFound, withSnapshot: true, wantCalls: 1, wantOutcome: pluginapi.RequestCompletionFailed, wantFormat: "gemini"},
		{name: "canceled", method: http.MethodGet, path: "/v1beta/models", status: http.StatusOK, withSnapshot: true, cancelContext: true, wantCalls: 1, wantOutcome: pluginapi.RequestCompletionCanceled, wantFormat: "gemini"},
		{name: "legacy global request", method: http.MethodGet, path: "/v1/models", status: http.StatusOK},
		{name: "execution route", method: http.MethodPost, path: "/v1/chat/completions", status: http.StatusOK, withSnapshot: true},
		{name: "unsupported GET", method: http.MethodGet, path: "/v1/responses", status: http.StatusForbidden, withSnapshot: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var completions []pluginapi.RequestCompletion
			engine := gin.New()
			engine.Use((&API{now: func() time.Time { return fixedNow }}).ScopedModelRequestCompletion(func(_ context.Context, completion pluginapi.RequestCompletion) {
				completions = append(completions, completion)
			}))
			engine.Handle(test.method, test.path, func(c *gin.Context) { c.Status(test.status) })

			request := httptest.NewRequest(test.method, test.path, nil)
			requestCtx := request.Context()
			if test.withSnapshot {
				requestCtx = carpoolruntime.WithAuthorization(requestCtx, carpoolruntime.NewAuthorizationSnapshot(
					"model-request-id", "user", "key", "car", "membership", "caller", nil,
				))
			}
			if test.cancelContext {
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithCancel(requestCtx)
				cancel()
			}
			request = request.WithContext(requestCtx)
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if len(completions) != test.wantCalls {
				t.Fatalf("completion calls = %d, want %d; values=%#v", len(completions), test.wantCalls, completions)
			}
			if test.wantCalls == 0 {
				return
			}
			completion := completions[0]
			if completion.RequestID != "model-request-id" || completion.Outcome != test.wantOutcome || completion.SourceFormat != test.wantFormat || !completion.CompletedAt.Equal(fixedNow.UTC()) {
				t.Fatalf("completion = %#v", completion)
			}
			wantStatus := test.status
			if test.cancelContext {
				wantStatus = 0
			}
			if completion.StatusCode != wantStatus {
				t.Fatalf("completion status = %d, want %d", completion.StatusCode, wantStatus)
			}
		})
	}
}
