package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func missingTestPrice() error {
	return &ex.RequestValidationError{Code: "model_price_not_configured", Message: "Model price is not configured", HTTPStatus: 422}
}

func TestFinalRequestValidationAliasRestoreAndLegacy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, restore := range []bool{false, true} {
			name := "execute"
			if stream {
				name = "stream"
			}
			if restore {
				name += "-restored"
			}
			t.Run(name, func(t *testing.T) {
				m, e := setupForceMappingManager(t, "antigravity", "real-model", "public-alias")
				opts := ex.Options{Stream: stream}
				req := ex.Request{Model: "public-alias"}
				want := "real-model"
				if restore {
					req.Model = "plugin-rewritten"
					opts.Metadata = map[string]any{ex.AuthSelectionModelMetadataKey: "public-alias"}
					want = req.Model
				}
				checks := 0
				ctx := ex.WithRequestValidator(context.Background(), func(_ context.Context, provider string, got ex.Request) error {
					checks++
					if got.Model != want || provider != "antigravity" {
						t.Errorf("final model/provider = %s/%s, want %s", got.Model, provider, want)
					}
					return missingTestPrice()
				})
				var err error
				if stream {
					_, err = m.ExecuteStream(ctx, []string{"antigravity"}, req, opts)
				} else {
					_, err = m.Execute(ctx, []string{"antigravity"}, req, opts)
				}
				if !ex.IsRequestValidationError(err) || checks != 1 {
					t.Fatalf("error=%v checks=%d", err, checks)
				}
				if len(e.ExecuteModels())+len(e.StreamModels()) != 0 {
					t.Fatal("local rejection called upstream")
				}
				for _, a := range m.List() {
					if a.LastError != nil || a.Unavailable || len(a.ModelStates) != 0 {
						t.Fatalf("local rejection penalized auth: %+v", a)
					}
				}
				// The identical legacy request has no validation hook.
				if stream {
					result, err := m.ExecuteStream(context.Background(), []string{"antigravity"}, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for range result.Chunks {
					}
				} else {
					if _, err := m.Execute(context.Background(), []string{"antigravity"}, req, opts); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestFinalRequestValidationConfiguredAliasAndScope(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetConfig(&internalconfig.Config{ClaudeKey: []internalconfig.ClaudeKey{{APIKey: "test-key", Prefix: "tenant", Models: []internalconfig.ClaudeModel{{Name: "real-model(high)", Alias: "public"}}}}})
			a := configuredCapabilityTestAuth("validation-allowed", "test-key")
			registerCapabilityTestAuth(t, m, a)
			registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "tenant/public"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			outside := &Auth{ID: "validation-outside", Provider: "codex", Status: StatusActive}
			if _, err := m.Register(context.Background(), outside); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(outside.ID, "codex", []*registry.ModelInfo{{ID: "tenant/public"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(outside.ID) })
			e := &forceMappingExecutor{id: "claude"}
			other := &forceMappingExecutor{id: "codex"}
			m.RegisterExecutor(e)
			m.RegisterExecutor(other)
			scope := ex.NewCredentialScope(a.ID)
			ctx := ex.WithCredentialScope(context.Background(), scope)
			checked := 0
			ctx = ex.WithRequestValidator(ctx, func(ctx context.Context, provider string, req ex.Request) error {
				checked++
				if provider != "claude" || req.Model != "real-model(high)" {
					t.Fatalf("incorrect actual model: %s %s", provider, req.Model)
				}
				if _, ok := ResolvedAPIKeyModelInfo(req); !ok {
					t.Error("configured model info absent")
				}
				if !ex.CredentialScopeFromContext(ctx).Allows(a.ID) || ex.CredentialScopeFromContext(ctx).Allows(outside.ID) {
					t.Error("scope expanded")
				}
				return missingTestPrice()
			})
			opts := ex.Options{Stream: stream, CredentialScope: scope, Metadata: map[string]any{ex.PinnedAuthMetadataKey: a.ID}}
			var err error
			if stream {
				_, err = m.ExecuteStream(ctx, []string{"claude", "codex"}, ex.Request{Model: "tenant/public"}, opts)
			} else {
				_, err = m.Execute(ctx, []string{"claude", "codex"}, ex.Request{Model: "tenant/public"}, opts)
			}
			if !ex.IsRequestValidationError(err) || checked != 1 {
				t.Fatalf("error=%v checked=%d", err, checked)
			}
			if len(e.ExecuteModels())+len(e.StreamModels())+len(other.ExecuteModels())+len(other.StreamModels()) != 0 {
				t.Fatal("upstream invoked")
			}
		})
	}
}

func TestFinalRequestValidationRunsAgainAfter401Refresh(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			m, e, _, _, model := newUnauthorizedRefreshFixture(t, false)
			checks := 0
			ctx := ex.WithRequestValidator(context.Background(), func(context.Context, string, ex.Request) error {
				checks++
				if checks == 2 {
					return missingTestPrice()
				}
				return nil
			})
			var err error
			if stream {
				_, err = m.ExecuteStream(ctx, []string{"codex"}, ex.Request{Model: model}, ex.Options{Stream: true})
			} else {
				_, err = m.Execute(ctx, []string{"codex"}, ex.Request{Model: model}, ex.Options{})
			}
			if !ex.IsRequestValidationError(err) || checks != 2 || e.RefreshCalls() != 1 {
				t.Fatalf("err=%v checks=%d refresh=%d", err, checks, e.RefreshCalls())
			}
			if len(e.ExecuteCalls())+len(e.StreamCalls()) != 1 {
				t.Fatal("refresh retry bypassed validation")
			}
		})
	}
}

func TestFinalRequestValidationPoolRetryKeepsPriorResult(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			failure := &Error{HTTPStatus: 503, Message: "actual upstream failure"}
			e := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey, executeErrors: map[string]error{"one": failure, "two": failure}, streamFirstErrors: map[string]error{"one": failure, "two": failure}, streamAttempts: map[string]bool{"one": true, "two": true}}
			m := newOpenAICompatPoolTestManager(t, "pool", []internalconfig.OpenAICompatibilityModel{{Name: "one", Alias: "pool"}, {Name: "two", Alias: "pool"}}, e)
			hook := &resultCaptureHook{}
			m.hook = hook
			checked := []string{}
			ctx := ex.WithRequestValidator(context.Background(), func(_ context.Context, _ string, req ex.Request) error {
				checked = append(checked, req.Model)
				if len(checked) == 2 {
					return missingTestPrice()
				}
				return nil
			})
			var err error
			if stream {
				_, err = m.ExecuteStream(ctx, []string{openAICompatPoolProviderKey}, ex.Request{Model: "pool"}, ex.Options{Stream: true})
			} else {
				_, err = m.Execute(ctx, []string{openAICompatPoolProviderKey}, ex.Request{Model: "pool"}, ex.Options{})
			}
			if !ex.IsRequestValidationError(err) || len(checked) != 2 || checked[0] == checked[1] {
				t.Fatalf("err=%v checked=%v", err, checked)
			}
			if len(e.ExecuteModels())+len(e.StreamModels()) != 1 {
				t.Fatal("missing-price pool member executed")
			}
			results := hook.Results()
			if len(results) != 1 || results[0].Error.HTTPStatus != 503 {
				t.Fatalf("lost real failure or penalized local error: %+v", results)
			}
			// CountTokens must not invoke generation-price validation.
			checksBefore := len(checked)
			if _, err := m.ExecuteCount(ctx, []string{openAICompatPoolProviderKey}, ex.Request{Model: "pool"}, ex.Options{}); err != nil {
				t.Fatal(err)
			}
			if len(checked) != checksBefore {
				t.Fatal("count tokens invoked generation validator")
			}
		})
	}
}

type bootstrapValidationExecutor struct{ *unauthorizedRefreshExecutor }

func (e *bootstrapValidationExecutor) ExecuteStream(ctx context.Context, a *Auth, r ex.Request, o ex.Options) (*ex.StreamResult, error) {
	result, err := e.unauthorizedRefreshExecutor.ExecuteStream(ctx, a, r, o)
	if err == nil {
		return result, nil
	}
	ex.MarkUpstreamAttempt(ctx)
	chunks := make(chan ex.StreamChunk, 1)
	chunks <- ex.StreamChunk{Err: err}
	close(chunks)
	return &ex.StreamResult{Chunks: chunks}, nil
}
func TestFinalRequestValidationBootstrap401Retry(t *testing.T) {
	m, e, _, _, model := newUnauthorizedRefreshFixture(t, false)
	m.RegisterExecutor(&bootstrapValidationExecutor{e})
	hook := &resultCaptureHook{}
	m.hook = hook
	checks := 0
	ctx := ex.WithRequestValidator(context.Background(), func(context.Context, string, ex.Request) error {
		checks++
		if checks == 2 {
			return missingTestPrice()
		}
		return nil
	})
	_, err := m.ExecuteStream(ctx, []string{"codex"}, ex.Request{Model: model}, ex.Options{Stream: true})
	if !ex.IsRequestValidationError(err) || checks != 2 || len(e.StreamCalls()) != 1 || e.RefreshCalls() != 1 {
		t.Fatalf("err=%v checks=%d calls=%v refresh=%d", err, checks, e.StreamCalls(), e.RefreshCalls())
	}
	if len(hook.Results()) != 0 {
		t.Fatal("local bootstrap retry rejection penalized credential")
	}
}
