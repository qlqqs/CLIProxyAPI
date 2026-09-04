package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type handlerCredentialScopeCaptureExecutor struct {
	provider string

	mu      sync.Mutex
	options map[string]coreexecutor.Options
}

func (e *handlerCredentialScopeCaptureExecutor) Identifier() string {
	return e.provider
}

func (e *handlerCredentialScopeCaptureExecutor) capture(kind string, opts coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.options == nil {
		e.options = make(map[string]coreexecutor.Options)
	}
	e.options[kind] = opts
}

func (e *handlerCredentialScopeCaptureExecutor) option(kind string) coreexecutor.Options {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.options[kind]
}

func (e *handlerCredentialScopeCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture("execute", opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *handlerCredentialScopeCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.capture("stream", opts)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *handlerCredentialScopeCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *handlerCredentialScopeCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.capture("count", opts)
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":0}`)}, nil
}

func (*handlerCredentialScopeCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newHandlerCredentialScopeTestSubject(t *testing.T) (*BaseAPIHandler, *handlerCredentialScopeCaptureExecutor, string) {
	t.Helper()
	const (
		provider = "handler-credential-scope-provider"
		model    = "handler-credential-scope-model"
		authID   = "handler-credential-scope-auth"
	)
	executor := &handlerCredentialScopeCaptureExecutor{provider: provider}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: provider,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"disable_cooling": true},
	}); errRegister != nil {
		t.Fatalf("manager.Register() error = %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager), executor, authID
}

func assertHandlerCredentialScope(t *testing.T, got, want *coreexecutor.CredentialScope) {
	t.Helper()
	if got != want {
		t.Fatalf("executor credential scope = %#v, want stored scope %#v", got, want)
	}
}

func TestHandlerPropagatesCredentialScopeToAuthManagerOptions(t *testing.T) {
	handler, executor, authID := newHandlerCredentialScopeTestSubject(t)
	scope := coreexecutor.NewCredentialScope(authID)
	ctx := coreexecutor.WithCredentialScope(context.Background(), scope)
	body := []byte(`{"model":"handler-credential-scope-model"}`)

	if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "handler-credential-scope-model", body, ""); errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	assertHandlerCredentialScope(t, executor.option("execute").CredentialScope, scope)

	if _, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", "handler-credential-scope-model", body, ""); errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager() error = %+v", errMsg)
	}
	assertHandlerCredentialScope(t, executor.option("count").CredentialScope, scope)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "handler-credential-scope-model", body, "")
	for range dataChan {
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("ExecuteStreamWithAuthManager() error = %+v", errMsg)
		}
	}
	assertHandlerCredentialScope(t, executor.option("stream").CredentialScope, scope)
}

type handlerCredentialScopePluginHost struct {
	mu      sync.Mutex
	options map[string]coreexecutor.Options
	calls   map[string]int
}

func (*handlerCredentialScopePluginHost) HasModelRouters() bool {
	return true
}

func (*handlerCredentialScopePluginHost) RouteModel(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetExecutor,
		Target:     "handler-credential-scope-plugin",
	}, true
}

func (h *handlerCredentialScopePluginHost) capture(kind string, opts coreexecutor.Options) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.options == nil {
		h.options = make(map[string]coreexecutor.Options)
	}
	if h.calls == nil {
		h.calls = make(map[string]int)
	}
	h.options[kind] = opts
	h.calls[kind]++
}

func (h *handlerCredentialScopePluginHost) option(kind string) coreexecutor.Options {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.options[kind]
}

func (h *handlerCredentialScopePluginHost) callCount(kind string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[kind]
}

func (h *handlerCredentialScopePluginHost) ExecutePluginExecutor(_ context.Context, _ string, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	h.capture("execute", opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (h *handlerCredentialScopePluginHost) ExecutePluginExecutorStream(_ context.Context, _ string, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	h.capture("stream", opts)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (h *handlerCredentialScopePluginHost) CountPluginExecutor(_ context.Context, _ string, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	h.capture("count", opts)
	return coreexecutor.Response{Payload: []byte(`{"total_tokens":0}`)}, nil
}

func TestHandlerRejectsCredentialScopeBeforeDirectPluginExecutor(t *testing.T) {
	host := &handlerCredentialScopePluginHost{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetModelRouterHost(host)
	var completions []pluginapi.RequestCompletion
	handler.SetRequestCompletionObserver(func(_ context.Context, completion pluginapi.RequestCompletion) {
		completions = append(completions, completion)
	})
	scope := coreexecutor.NewCredentialScope("plugin-auth")
	ctx := coreexecutor.WithCredentialScope(context.Background(), scope)
	body := []byte(`{"model":"plugin-model"}`)

	if response, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "plugin-model", body, ""); response != nil || headers != nil {
		t.Fatalf("ExecuteWithAuthManager() response = %q, headers = %#v, want nil", response, headers)
	} else {
		assertHandlerCredentialScopedPluginRejection(t, errMsg)
	}

	if response, headers, errMsg := handler.ExecuteCountWithAuthManager(ctx, "openai", "plugin-model", body, ""); response != nil || headers != nil {
		t.Fatalf("ExecuteCountWithAuthManager() response = %q, headers = %#v, want nil", response, headers)
	} else {
		assertHandlerCredentialScopedPluginRejection(t, errMsg)
	}

	dataChan, headers, errChan := handler.ExecuteStreamWithAuthManager(ctx, "openai", "plugin-model", body, "")
	if dataChan != nil || headers != nil {
		t.Fatalf("ExecuteStreamWithAuthManager() data channel = %#v, headers = %#v, want nil", dataChan, headers)
	}
	errMsg, ok := <-errChan
	if !ok {
		t.Fatal("ExecuteStreamWithAuthManager() returned a closed error channel without an error")
	}
	assertHandlerCredentialScopedPluginRejection(t, errMsg)
	if _, ok = <-errChan; ok {
		t.Fatal("ExecuteStreamWithAuthManager() error channel remained open")
	}

	for _, kind := range []string{"execute", "count", "stream"} {
		if calls := host.callCount(kind); calls != 0 {
			t.Fatalf("direct plugin %s calls = %d, want 0", kind, calls)
		}
	}
	if len(completions) != 3 {
		t.Fatalf("request completion count = %d, want 3", len(completions))
	}
	for i, completion := range completions {
		if completion.RequestID == "" || completion.Outcome != pluginapi.RequestCompletionRejected || completion.StatusCode != http.StatusForbidden {
			t.Fatalf("completion[%d] = %#v, want rejected 403 with request ID", i, completion)
		}
	}
}

func TestHandlerAllowsLegacyUnscopedDirectPluginExecutor(t *testing.T) {
	host := &handlerCredentialScopePluginHost{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetModelRouterHost(host)
	body := []byte(`{"model":"plugin-model"}`)

	if _, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "plugin-model", body, ""); errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	assertHandlerCredentialScope(t, host.option("execute").CredentialScope, nil)

	if _, _, errMsg := handler.ExecuteCountWithAuthManager(context.Background(), "openai", "plugin-model", body, ""); errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager() error = %+v", errMsg)
	}
	assertHandlerCredentialScope(t, host.option("count").CredentialScope, nil)

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "plugin-model", body, "")
	for range dataChan {
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("ExecuteStreamWithAuthManager() error = %+v", errMsg)
		}
	}
	assertHandlerCredentialScope(t, host.option("stream").CredentialScope, nil)

	for _, kind := range []string{"execute", "count", "stream"} {
		if calls := host.callCount(kind); calls != 1 {
			t.Fatalf("direct plugin %s calls = %d, want 1", kind, calls)
		}
	}
}

func assertHandlerCredentialScopedPluginRejection(t *testing.T, errMsg *interfaces.ErrorMessage) {
	t.Helper()
	if errMsg == nil || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("credential-scoped plugin error = %#v, want 403", errMsg)
	}
	if errMsg.Error == nil || errMsg.Error.Error() != "credential-scoped requests are unsupported by direct executor plugins" {
		t.Fatalf("credential-scoped plugin error = %#v, want safe unsupported message", errMsg.Error)
	}
}
