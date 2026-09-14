package carpool

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestModuleRequestCatalogFrozenAcrossRefreshAndAsyncFallback(t *testing.T) {
	f := newServerRouteFixture(t, false)
	a := []byte(`{"gpt-5.4":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`)
	b := []byte(`{"gpt-5.4":{"input_cost_per_token":0.000010,"output_cost_per_token":0.000020},"new-model":{"input_cost_per_token":0,"output_cost_per_token":0}}`)
	if err := f.setPrices(a); err != nil {
		t.Fatal(err)
	}
	old := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(old.Context())
	if err := f.setPrices(b); err != nil {
		t.Fatal(err)
	}
	next := accountingRequest(t, f)
	nextSnapshot, _ := carpoolruntime.AuthorizationFromContext(next.Context())
	if snapshot.Prices().Hash() == nextSnapshot.Prices().Hash() {
		t.Fatal("next request did not capture new catalog")
	}
	for _, s := range []*carpoolruntime.AuthorizationSnapshot{snapshot, nextSnapshot} {
		row, err := f.module.store.GetProxyRequest(context.Background(), s.RequestID())
		if err != nil {
			t.Fatal(err)
		}
		if row.PricingCatalogHash != s.Prices().Hash() {
			t.Fatal("persisted hash differs from execution snapshot")
		}
	}
	if err := ex.ValidateRequest(old.Context(), "openai", ex.Request{Model: "new-model"}); !ex.IsRequestValidationError(err) {
		t.Fatalf("old request accepted newly priced model: %v", err)
	}
	if err := ex.ValidateRequest(next.Context(), "openai", ex.Request{Model: "new-model"}); err != nil {
		t.Fatal(err)
	}
	manager := usage.NewManager(1)
	manager.Register(scopedAccountingPlugin{writer: f.module.writer})
	record := accountingRecord()
	manager.Publish(old.Context(), record)
	manager.Publish(old.Context(), record)
	// Delayed fallback retains only the original request authorization, without a synchronous observer.
	fallback := carpoolruntime.WithAuthorization(context.Background(), snapshot)
	record.EventID = "old-delayed"
	manager.Publish(fallback, record)
	record.EventID = "new"
	manager.Publish(next.Context(), record)
	manager.Stop()
	oldRow, _ := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	newRow, _ := f.module.store.GetProxyRequest(context.Background(), nextSnapshot.RequestID())
	if oldRow.BilledNanoUSD == nil || *oldRow.BilledNanoUSD != 280000 {
		t.Fatalf("old cost=%v", oldRow.BilledNanoUSD)
	}
	if newRow.BilledNanoUSD == nil || *newRow.BilledNanoUSD != 1400000 {
		t.Fatalf("new cost=%v", newRow.BilledNanoUSD)
	}
}

func TestModuleHTTPMissingPriceBlocksBeforeExecutor(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{name: "OpenAI chat", path: "/v1/chat/completions", body: `{"model":"` + serverRouteOpenAIModel + `","messages":[]}`},
		{name: "OpenAI chat stream", path: "/v1/chat/completions", body: `{"model":"` + serverRouteOpenAIModel + `","messages":[],"stream":true}`},
		{name: "OpenAI completions", path: "/v1/completions", body: `{"model":"` + serverRouteOpenAIModel + `","prompt":"test"}`},
		{name: "OpenAI completions stream", path: "/v1/completions", body: `{"model":"` + serverRouteOpenAIModel + `","prompt":"test","stream":true}`},
		{name: "OpenAI responses", path: "/v1/responses", body: `{"model":"` + serverRouteOpenAIModel + `","input":"test"}`},
		{name: "OpenAI responses stream", path: "/v1/responses", body: `{"model":"` + serverRouteOpenAIModel + `","input":"test","stream":true}`},
		{name: "Claude messages", path: "/v1/messages", body: `{"model":"` + serverRouteClaudeModel + `","messages":[],"max_tokens":16}`},
		{name: "Claude messages stream", path: "/v1/messages", body: `{"model":"` + serverRouteClaudeModel + `","messages":[],"max_tokens":16,"stream":true}`},
		{name: "Codex responses", path: "/backend-api/codex/responses", body: `{"model":"` + serverRouteCodexModel + `","input":"test"}`},
		{name: "Gemini generate", path: "/v1beta/models/" + serverRouteGeminiModel + ":generateContent", body: `{"contents":[]}`},
		{name: "Gemini stream", path: "/v1beta/models/" + serverRouteGeminiModel + ":streamGenerateContent?alt=sse", body: `{"contents":[]}`},
		{name: "Gemini interactions", path: "/v1beta/interactions", body: `{"model":"` + serverRouteGeminiModel + `","input":"test"}`},
		{name: "Gemini interactions stream", path: "/v1beta/interactions", body: `{"model":"` + serverRouteGeminiModel + `","input":"test","stream":true}`},
		{name: "Codex responses stream", path: "/backend-api/codex/responses", body: `{"model":"` + serverRouteCodexModel + `","input":"test","stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerRouteFixture(t, false)
			if err := f.setPrices([]byte(`{"other-model":{"input_cost_per_token":0,"output_cost_per_token":0}}`)); err != nil {
				t.Fatal(err)
			}
			response := f.request(t, http.MethodPost, tc.path, tc.body, false)
			if response.Code != 422 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				}
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "model_price_not_configured" {
				t.Fatalf("unsafe/missing code: %s", response.Body.String())
			}
			selected, _ := f.recorder.snapshot()
			if len(selected) != 0 {
				t.Fatalf("upstream called: %v", selected)
			}
			if strings.Contains(response.Body.String(), "carpool-route-") {
				t.Fatal("error leaked model details")
			}
			models := f.request(t, http.MethodGet, "/v1/models", "", false)
			if models.Code != 200 {
				t.Fatalf("model reads blocked: %d", models.Code)
			}
		})
	}
}
