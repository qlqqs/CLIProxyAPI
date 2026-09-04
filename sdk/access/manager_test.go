package access

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type managerTestProvider struct {
	id     string
	result *Result
	err    *AuthError
	calls  int
}

func (p *managerTestProvider) Identifier() string {
	return p.id
}

func (p *managerTestProvider) Authenticate(context.Context, *http.Request) (*Result, *AuthError) {
	p.calls++
	return p.result, p.err
}

type managerTestContextKey struct{}

func TestManagerRunsAuthenticatedRequestHookAfterProviderSuccess(t *testing.T) {
	result := &Result{Provider: "primary", Principal: "principal"}
	provider := &managerTestProvider{id: "primary", result: result}
	manager := NewManager()
	manager.SetProviders([]Provider{provider})

	request, errRequest := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
	if errRequest != nil {
		t.Fatalf("NewRequest() error = %v", errRequest)
	}
	ctx := context.WithValue(context.Background(), managerTestContextKey{}, "context-value")
	var hookCalls int
	manager.SetAuthenticatedRequestHook(func(gotCtx context.Context, gotRequest *http.Request, gotResult *Result) *AuthError {
		hookCalls++
		if gotCtx.Value(managerTestContextKey{}) != "context-value" {
			t.Errorf("hook context value = %v", gotCtx.Value(managerTestContextKey{}))
		}
		if gotRequest != request {
			t.Errorf("hook request = %p, want %p", gotRequest, request)
		}
		if gotResult != result {
			t.Errorf("hook result = %p, want %p", gotResult, result)
		}
		return nil
	})

	got, authErr := manager.Authenticate(ctx, request)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if got != result {
		t.Fatalf("Authenticate() result = %p, want %p", got, result)
	}
	if provider.calls != 1 || hookCalls != 1 {
		t.Fatalf("calls = provider %d, hook %d; want 1 each", provider.calls, hookCalls)
	}
}

func TestManagerHookRejectionCannotFallThroughToLaterProvider(t *testing.T) {
	first := &managerTestProvider{id: "first", result: &Result{Provider: "first", Principal: "first"}}
	second := &managerTestProvider{id: "second", result: &Result{Provider: "second", Principal: "second"}}
	manager := NewManager()
	manager.SetProviders([]Provider{first, second})

	cause := errors.New("policy denied")
	var hookCalls int
	manager.SetAuthenticatedRequestHook(func(context.Context, *http.Request, *Result) *AuthError {
		hookCalls++
		return NewForbiddenError("request denied", cause)
	})
	request, errRequest := http.NewRequest(http.MethodPost, "https://proxy.test/v1/chat/completions", nil)
	if errRequest != nil {
		t.Fatalf("NewRequest() error = %v", errRequest)
	}

	got, authErr := manager.Authenticate(context.Background(), request)
	if got != nil {
		t.Fatalf("Authenticate() result = %#v, want nil", got)
	}
	if !IsAuthErrorCode(authErr, AuthErrorCodeForbidden) {
		t.Fatalf("Authenticate() error = %#v, want forbidden", authErr)
	}
	if authErr.HTTPStatusCode() != http.StatusForbidden || !errors.Is(authErr, cause) {
		t.Fatalf("forbidden error = %#v, cause preserved = %t", authErr, errors.Is(authErr, cause))
	}
	if first.calls != 1 || second.calls != 0 || hookCalls != 1 {
		t.Fatalf("calls = first %d, second %d, hook %d; want 1, 0, 1", first.calls, second.calls, hookCalls)
	}
}

func TestManagerWithoutAuthenticatedRequestHookPreservesProviderFallback(t *testing.T) {
	first := &managerTestProvider{id: "first", err: NewNotHandledError()}
	expected := &Result{Provider: "second", Principal: "legacy-principal"}
	second := &managerTestProvider{id: "second", result: expected}
	manager := NewManager()
	manager.SetProviders([]Provider{first, second})
	request, errRequest := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
	if errRequest != nil {
		t.Fatalf("NewRequest() error = %v", errRequest)
	}

	got, authErr := manager.Authenticate(context.Background(), request)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if got != expected {
		t.Fatalf("Authenticate() result = %p, want %p", got, expected)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("provider calls = first %d, second %d; want 1 each", first.calls, second.calls)
	}
}
