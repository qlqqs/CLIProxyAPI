package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type credentialScopeSelector struct {
	selected *Auth
	seen     []string
}

func (s *credentialScopeSelector) Pick(_ context.Context, _ string, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	s.seen = s.seen[:0]
	for _, auth := range auths {
		if auth != nil {
			s.seen = append(s.seen, auth.ID)
		}
	}
	if s.selected != nil {
		return s.selected, nil
	}
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[0], nil
}

type credentialScopePluginScheduler struct {
	response pluginapi.SchedulerPickResponse
	requests []pluginapi.SchedulerPickRequest
}

func (s *credentialScopePluginScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.requests = append(s.requests, req)
	return s.response, true, nil
}

type credentialScopeExecutionCall struct {
	authID    string
	requestID string
	scope     *cliproxyexecutor.CredentialScope
}

type credentialScopeRecordingExecutor struct {
	provider string
	mu       sync.Mutex
	calls    map[string][]credentialScopeExecutionCall
}

func (e *credentialScopeRecordingExecutor) Identifier() string {
	return e.provider
}

func (e *credentialScopeRecordingExecutor) record(kind string, auth *Auth, opts cliproxyexecutor.Options) error {
	call := credentialScopeExecutionCall{requestID: opts.RequestID, scope: opts.CredentialScope}
	if auth != nil {
		call.authID = auth.ID
	}
	e.mu.Lock()
	if e.calls == nil {
		e.calls = make(map[string][]credentialScopeExecutionCall)
	}
	e.calls[kind] = append(e.calls[kind], call)
	e.mu.Unlock()
	return &Error{HTTPStatus: http.StatusInternalServerError, Message: "credential scope test failure"}
}

func (e *credentialScopeRecordingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.record("execute", auth, opts)
}

func (e *credentialScopeRecordingExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.record("stream", auth, opts)
}

func (*credentialScopeRecordingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *credentialScopeRecordingExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.record("count", auth, opts)
}

func (*credentialScopeRecordingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *credentialScopeRecordingExecutor) callsFor(kind string) []credentialScopeExecutionCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]credentialScopeExecutionCall(nil), e.calls[kind]...)
}

type credentialScopeHomeDispatcher struct {
	calls atomic.Int32
}

func (*credentialScopeHomeDispatcher) HeartbeatOK() bool {
	return true
}

func (d *credentialScopeHomeDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	d.calls.Add(1)
	return nil, errors.New("unexpected Home dispatch")
}

func (*credentialScopeHomeDispatcher) AbortAmbiguousDispatch() {}

func registerCredentialScopeAuth(t *testing.T, manager *Manager, authID, provider, model string) {
	t.Helper()
	if model != "" {
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(authID) })
	}
	_, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       authID,
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{"disable_cooling": true},
	})
	if errRegister != nil {
		t.Fatalf("Register(%q) error = %v", authID, errRegister)
	}
}

func assertCredentialScopeError(t *testing.T, err error, code string) {
	t.Helper()
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil || authErr.Code != code {
		t.Fatalf("error = %v, want auth error code %q", err, code)
	}
}

func TestManagerCredentialScopeFastSchedulerSingleAndMixed(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-fast-gemini"})
	manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-fast-claude"})
	registerCredentialScopeAuth(t, manager, "scope-fast-gemini-a", "scope-fast-gemini", "")
	registerCredentialScopeAuth(t, manager, "scope-fast-gemini-b", "scope-fast-gemini", "")
	registerCredentialScopeAuth(t, manager, "scope-fast-claude-a", "scope-fast-claude", "")

	singleOpts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope("scope-fast-gemini-b")}
	selected, _, errPick := manager.pickNext(context.Background(), "scope-fast-gemini", "", singleOpts, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if selected == nil || selected.ID != "scope-fast-gemini-b" {
		t.Fatalf("pickNext() auth = %#v, want scope-fast-gemini-b", selected)
	}

	mixedOpts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope("scope-fast-claude-a")}
	selected, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"scope-fast-gemini", "scope-fast-claude"}, "", mixedOpts, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if selected == nil || selected.ID != "scope-fast-claude-a" || provider != "scope-fast-claude" {
		t.Fatalf("pickNextMixed() auth = %#v, provider = %q", selected, provider)
	}

	emptyOpts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope()}
	if selected, _, errPick = manager.pickNext(context.Background(), "scope-fast-gemini", "", emptyOpts, nil); errPick == nil || selected != nil {
		t.Fatalf("pickNext(empty scope) = %#v, %v; want rejection", selected, errPick)
	}
	assertCredentialScopeError(t, errPick, "auth_not_found")
}

func TestManagerCredentialScopeLegacySelectorFiltersAndValidates(t *testing.T) {
	t.Run("candidate input", func(t *testing.T) {
		selector := &credentialScopeSelector{}
		manager := NewManager(nil, selector, nil)
		manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-legacy"})
		registerCredentialScopeAuth(t, manager, "scope-legacy-a", "scope-legacy", "")
		registerCredentialScopeAuth(t, manager, "scope-legacy-b", "scope-legacy", "")

		opts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope("scope-legacy-b")}
		selected, _, errPick := manager.pickNext(context.Background(), "scope-legacy", "", opts, nil)
		if errPick != nil {
			t.Fatalf("pickNext() error = %v", errPick)
		}
		if selected == nil || selected.ID != "scope-legacy-b" {
			t.Fatalf("pickNext() auth = %#v, want scope-legacy-b", selected)
		}
		if got := strings.Join(selector.seen, ","); got != "scope-legacy-b" {
			t.Fatalf("selector candidates = %q, want scope-legacy-b", got)
		}
	})

	t.Run("out of scope result", func(t *testing.T) {
		selector := &credentialScopeSelector{selected: &Auth{ID: "scope-malicious-outside", Provider: "scope-malicious"}}
		manager := NewManager(nil, selector, nil)
		manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-malicious"})
		registerCredentialScopeAuth(t, manager, "scope-malicious-allowed", "scope-malicious", "")
		registerCredentialScopeAuth(t, manager, "scope-malicious-outside", "scope-malicious", "")

		opts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope("scope-malicious-allowed")}
		selected, _, errPick := manager.pickNext(context.Background(), "scope-malicious", "", opts, nil)
		if selected != nil {
			t.Fatalf("pickNext() auth = %#v, want nil", selected)
		}
		assertCredentialScopeError(t, errPick, "credential_scope_violation")
		if strings.Contains(errPick.Error(), "scope-malicious-outside") {
			t.Fatalf("scope error leaked selected auth ID: %v", errPick)
		}

		selected, _, errPick = manager.pickNext(context.Background(), "scope-malicious", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(nil scope) error = %v", errPick)
		}
		if selected == nil || selected.ID != "scope-malicious-outside" {
			t.Fatalf("pickNext(nil scope) auth = %#v, want legacy selector result", selected)
		}
	})
}

func TestManagerCredentialScopePluginCandidatesAndReturnValue(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "single"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-plugin-gemini"})
			manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-plugin-claude"})
			registerCredentialScopeAuth(t, manager, "scope-plugin-allowed", "scope-plugin-gemini", "")
			registerCredentialScopeAuth(t, manager, "scope-plugin-outside", "scope-plugin-claude", "")
			plugin := &credentialScopePluginScheduler{response: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "scope-plugin-outside"}}
			manager.SetPluginScheduler(plugin)
			opts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope("scope-plugin-allowed")}

			var errPick error
			if mixed {
				_, _, _, errPick = manager.pickNextMixed(context.Background(), []string{"scope-plugin-gemini", "scope-plugin-claude"}, "", opts, nil)
			} else {
				_, _, errPick = manager.pickNext(context.Background(), "scope-plugin-gemini", "", opts, nil)
			}
			assertCredentialScopeError(t, errPick, "credential_scope_violation")
			if len(plugin.requests) != 1 || len(plugin.requests[0].Candidates) != 1 || plugin.requests[0].Candidates[0].ID != "scope-plugin-allowed" {
				t.Fatalf("plugin candidates = %#v, want only scope-plugin-allowed", plugin.requests)
			}
			if strings.Contains(errPick.Error(), "scope-plugin-outside") {
				t.Fatalf("scope error leaked plugin auth ID: %v", errPick)
			}
		})
	}

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-plugin-legacy"})
	registerCredentialScopeAuth(t, manager, "scope-plugin-legacy-a", "scope-plugin-legacy", "")
	registerCredentialScopeAuth(t, manager, "scope-plugin-legacy-b", "scope-plugin-legacy", "")
	manager.SetPluginScheduler(&credentialScopePluginScheduler{response: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "scope-plugin-legacy-b"}})
	selected, _, errPick := manager.pickNext(context.Background(), "scope-plugin-legacy", "", cliproxyexecutor.Options{}, nil)
	if errPick != nil || selected == nil || selected.ID != "scope-plugin-legacy-b" {
		t.Fatalf("pickNext(nil scope) = %#v, %v; want legacy plugin selection", selected, errPick)
	}
}

func TestManagerCredentialScopeRejectsOutOfScopePinnedAuth(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-pinned"})
	registerCredentialScopeAuth(t, manager, "scope-pinned-a", "scope-pinned", "")
	registerCredentialScopeAuth(t, manager, "scope-pinned-b", "scope-pinned", "")

	opts := cliproxyexecutor.Options{
		CredentialScope: cliproxyexecutor.NewCredentialScope("scope-pinned-a"),
		Metadata:        map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "scope-pinned-b"},
	}
	selected, _, errPick := manager.pickNext(context.Background(), "scope-pinned", "", opts, nil)
	if errPick == nil || selected != nil {
		t.Fatalf("pickNext() = %#v, %v; want out-of-scope pin rejected", selected, errPick)
	}
	assertCredentialScopeError(t, errPick, "auth_not_found")
}

func TestManagerCredentialScopeTreatsOutOfScopeAffinityAsMiss(t *testing.T) {
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, TTL: time.Hour})
	t.Cleanup(affinity.Stop)
	manager := NewManager(nil, affinity, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "scope-affinity"})
	registerCredentialScopeAuth(t, manager, "scope-affinity-a", "scope-affinity", "")
	registerCredentialScopeAuth(t, manager, "scope-affinity-b", "scope-affinity", "")
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"scope-affinity-session"}}}

	first, _, errPick := manager.pickNext(context.Background(), "scope-affinity", "", opts, nil)
	if errPick != nil || first == nil {
		t.Fatalf("first pickNext() = %#v, %v", first, errPick)
	}
	allowedID := "scope-affinity-a"
	if first.ID == allowedID {
		allowedID = "scope-affinity-b"
	}
	opts.CredentialScope = cliproxyexecutor.NewCredentialScope(allowedID)
	second, _, errPick := manager.pickNext(context.Background(), "scope-affinity", "", opts, nil)
	if errPick != nil {
		t.Fatalf("second pickNext() error = %v", errPick)
	}
	if second == nil || second.ID != allowedID {
		t.Fatalf("second pickNext() auth = %#v, want %q after affinity miss", second, allowedID)
	}
}

func TestManagerCredentialScopeSurvivesExecutionRetries(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		invoke func(*Manager, cliproxyexecutor.Request, cliproxyexecutor.Options) error
	}{
		{
			name: "execute",
			kind: "execute",
			invoke: func(manager *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, errExecute := manager.Execute(context.Background(), []string{"scope-execute"}, req, opts)
				return errExecute
			},
		},
		{
			name: "count",
			kind: "count",
			invoke: func(manager *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				_, errExecute := manager.ExecuteCount(context.Background(), []string{"scope-count"}, req, opts)
				return errExecute
			},
		},
		{
			name: "stream",
			kind: "stream",
			invoke: func(manager *Manager, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
				opts.Stream = true
				_, errExecute := manager.ExecuteStream(context.Background(), []string{"scope-stream"}, req, opts)
				return errExecute
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := "scope-" + test.name
			model := provider + "-model"
			allowedA := provider + "-a"
			allowedB := provider + "-b"
			outside := provider + "-outside"
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.SetRetryConfig(1, 0, 0)
			executor := &credentialScopeRecordingExecutor{provider: provider}
			manager.RegisterExecutor(executor)
			registerCredentialScopeAuth(t, manager, allowedA, provider, model)
			registerCredentialScopeAuth(t, manager, allowedB, provider, model)
			registerCredentialScopeAuth(t, manager, outside, provider, model)
			scope := cliproxyexecutor.NewCredentialScope(allowedA, allowedB)
			opts := cliproxyexecutor.Options{RequestID: "request-" + test.name, CredentialScope: scope}

			if errExecute := test.invoke(manager, cliproxyexecutor.Request{Model: model}, opts); errExecute == nil {
				t.Fatal("execution error = nil, want terminal retry error")
			}
			calls := executor.callsFor(test.kind)
			if len(calls) < 2 {
				t.Fatalf("execution calls = %#v, want scoped failover", calls)
			}
			seen := make(map[string]bool)
			for _, call := range calls {
				if call.authID == outside || !scope.Allows(call.authID) {
					t.Fatalf("execution selected out-of-scope auth %q; calls=%#v", call.authID, calls)
				}
				if call.scope != scope {
					t.Fatalf("execution scope pointer changed across retry: got %p want %p", call.scope, scope)
				}
				if call.requestID != "request-"+test.name {
					t.Fatalf("execution request ID = %q", call.requestID)
				}
				seen[call.authID] = true
			}
			if !seen[allowedA] || !seen[allowedB] {
				t.Fatalf("execution calls = %#v, want both allowed auths", calls)
			}
		})
	}
}

func TestManagerCredentialScopeRejectsHomeBeforeDispatch(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(*Manager, cliproxyexecutor.Options) error
	}{
		{
			name: "execute",
			invoke: func(manager *Manager, opts cliproxyexecutor.Options) error {
				_, errExecute := manager.Execute(context.Background(), []string{"scope-home"}, cliproxyexecutor.Request{Model: "scope-home-model"}, opts)
				return errExecute
			},
		},
		{
			name: "count",
			invoke: func(manager *Manager, opts cliproxyexecutor.Options) error {
				_, errExecute := manager.ExecuteCount(context.Background(), []string{"scope-home"}, cliproxyexecutor.Request{Model: "scope-home-model"}, opts)
				return errExecute
			},
		},
		{
			name: "stream",
			invoke: func(manager *Manager, opts cliproxyexecutor.Options) error {
				opts.Stream = true
				_, errExecute := manager.ExecuteStream(context.Background(), []string{"scope-home"}, cliproxyexecutor.Request{Model: "scope-home-model"}, opts)
				return errExecute
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
			dispatcher := &credentialScopeHomeDispatcher{}
			manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
			opts := cliproxyexecutor.Options{CredentialScope: cliproxyexecutor.NewCredentialScope("scope-home-auth")}

			errExecute := test.invoke(manager, opts)
			assertCredentialScopeError(t, errExecute, "credential_scope_unsupported")
			if calls := dispatcher.calls.Load(); calls != 0 {
				t.Fatalf("Home dispatcher calls = %d, want 0", calls)
			}
		})
	}
}
