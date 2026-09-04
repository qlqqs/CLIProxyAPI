package executor

import (
	"context"
	"reflect"
	"testing"
)

func TestCredentialScopeContextRoundTrip(t *testing.T) {
	scope := NewCredentialScope("auth-b", "auth-a")
	ctx := WithCredentialScope(context.Background(), scope)
	got := CredentialScopeFromContext(ctx)

	if got != scope {
		t.Fatal("CredentialScopeFromContext() did not return the stored immutable scope")
	}
	if ids := got.IDs(); !reflect.DeepEqual(ids, []string{"auth-a", "auth-b"}) {
		t.Fatalf("CredentialScopeFromContext().IDs() = %#v, want auth-a/auth-b", ids)
	}
}

func TestCredentialScopeContextNilSemantics(t *testing.T) {
	if got := CredentialScopeFromContext(nil); got != nil {
		t.Fatalf("CredentialScopeFromContext(nil) = %#v, want nil", got)
	}

	ctx := WithCredentialScope(nil, nil)
	if ctx == nil {
		t.Fatal("WithCredentialScope(nil, nil) returned a nil context")
	}
	if got := CredentialScopeFromContext(ctx); got != nil {
		t.Fatalf("CredentialScopeFromContext(legacy context) = %#v, want nil", got)
	}

	empty := NewCredentialScope()
	ctx = WithCredentialScope(nil, empty)
	if got := CredentialScopeFromContext(ctx); got != empty || !got.Enforced() {
		t.Fatalf("CredentialScopeFromContext(empty scope) = %#v, want enforced empty scope", got)
	}
}

func TestCredentialScopeContextCannotBeExpandedOrCleared(t *testing.T) {
	original := NewCredentialScope("auth-a")
	ctx := WithCredentialScope(context.Background(), original)
	ctx = WithCredentialScope(ctx, NewCredentialScope("auth-a", "auth-b"))
	ctx = WithCredentialScope(ctx, nil)

	got := CredentialScopeFromContext(ctx)
	if got != original {
		t.Fatalf("CredentialScopeFromContext() = %#v, want original scope %#v", got, original)
	}
	if got.Allows("auth-b") {
		t.Fatal("nested WithCredentialScope() expanded the original scope")
	}
}
