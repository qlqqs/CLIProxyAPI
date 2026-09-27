package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type streamCredentialRateLimitError struct {
	customStatusError
	credentialScoped bool
}

func (e streamCredentialRateLimitError) IsCredentialScoped() bool { return e.credentialScoped }

func TestExecuteStreamTerminalPreservesCooldownMetadata(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, credentialScoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("credential_scope_%t", credentialScoped), func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			ids := registerOverloadAuths(t, manager, 1)
			cooldown := 300 * time.Second
			streamErr := fmt.Errorf("upstream: %w", streamCredentialRateLimitError{
				customStatusError: customStatusError{
					code:       http.StatusTooManyRequests,
					msg:        `{"error":{"type":"usage_limit_reached","resets_in_seconds":300}}`,
					retryAfter: &cooldown,
				},
				credentialScoped: credentialScoped,
			})
			manager.RegisterExecutor(&customStreamMockExecutor{
				identifier: "codex",
				streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					out := make(chan cliproxyexecutor.StreamChunk, 2)
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created"}`)}
					out <- cliproxyexecutor.StreamChunk{Err: streamErr}
					close(out)
					return &cliproxyexecutor.StreamResult{Chunks: out}, nil
				},
			})
			started := time.Now()
			result, errStream := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			if errStream != nil {
				t.Fatal(errStream)
			}
			failures := 0
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					failures++
					if chunk.Err != streamErr {
						t.Errorf("stream replaced the original error: %v", chunk.Err)
					}
				}
			}
			if failures != 1 {
				t.Fatalf("stream failures = %d, want 1", failures)
			}
			auth, ok := manager.GetByID(ids[0])
			if !ok {
				t.Fatal("auth missing")
			}
			state := auth.ModelStates["gpt-5.6-terra"]
			if state == nil {
				t.Fatal("model cooldown state missing")
			}
			if state.NextRetryAfter.Before(started.Add(cooldown)) || state.NextRetryAfter.After(time.Now().Add(cooldown)) {
				t.Fatalf("cooldown did not preserve RetryAfter: %v", state.NextRetryAfter.Sub(started))
			}
			if credentialScoped {
				if auth.Quota.Reason != "credential_quota" || !auth.NextRetryAfter.Equal(state.NextRetryAfter) {
					t.Fatalf("credential cooldown not preserved: %+v", auth.Quota)
				}
			} else if auth.Quota.Reason == "credential_quota" {
				t.Fatal("model-only error unexpectedly cooled the whole credential")
			}
		})
	}
}
