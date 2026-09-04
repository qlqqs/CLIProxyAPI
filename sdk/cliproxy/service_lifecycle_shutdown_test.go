package cliproxy

import (
	"context"
	"errors"
	"testing"
)

func TestShutdownOnRunExitCreatesFreshContext(t *testing.T) {
	wantErr := errors.New("shutdown sentinel")
	called := false
	errShutdown := shutdownOnRunExit(func(ctx context.Context) error {
		called = true
		if ctx == nil {
			t.Fatal("shutdown context is nil")
		}
		if errContext := ctx.Err(); errContext != nil {
			t.Fatalf("shutdown context is already expired: %v", errContext)
		}
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			t.Fatal("shutdown context has no deadline")
		}
		return wantErr
	})
	if !called {
		t.Fatal("shutdown callback was not called")
	}
	if !errors.Is(errShutdown, wantErr) {
		t.Fatalf("shutdown error = %v, want %v", errShutdown, wantErr)
	}
}
