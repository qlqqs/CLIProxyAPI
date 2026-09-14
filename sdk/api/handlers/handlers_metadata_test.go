package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/net/context"
)

func TestGetContextWithCancelCapturesClientRequestMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request.RemoteAddr = "192.0.2.10:43123"
	ginCtx.Request.Header.Add("X-Forwarded-For", "203.0.113.5")
	ginCtx.Request.Header.Add("X-Forwarded-For", "198.51.100.8")
	ginCtx.Request.Header.Set("User-Agent", "test-client/1.0")

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	metadata := logging.GetClientRequestMetadata(ctx)
	if metadata.ClientIP != "192.0.2.10" {
		t.Fatalf("ClientIP = %q, want direct peer IP", metadata.ClientIP)
	}
	if metadata.XForwardedFor != "203.0.113.5, 198.51.100.8" {
		t.Fatalf("XForwardedFor = %q", metadata.XForwardedFor)
	}
	if metadata.UserAgent != "test-client/1.0" {
		t.Fatalf("UserAgent = %q", metadata.UserAgent)
	}
}

func TestGetContextWithCancelPreservesRequestExecutionBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	scope := coreexecutor.NewCredentialScope("auth-a", "auth-b")
	requestCtx := WithRequestLifecycleID(request.Context(), "carpool-request")
	requestCtx = coreexecutor.WithCredentialScope(requestCtx, scope)
	ginCtx.Request = request.WithContext(requestCtx)

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	if requestID := requestLifecycleIDFromContext(ctx); requestID != "carpool-request" {
		t.Fatalf("request lifecycle ID = %q, want carpool-request", requestID)
	}
	if gotScope := coreexecutor.CredentialScopeFromContext(ctx); gotScope != scope {
		t.Fatalf("credential scope = %#v, want original scope %#v", gotScope, scope)
	}
}

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestRequestExecutionMetadataIncludesHashedCallerScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Set("userApiKey", "downstream-secret")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	meta := requestExecutionMetadata(ctx)
	got, _ := meta[coreexecutor.CallerScopeMetadataKey].(string)
	want := coresession.CallerScope("downstream-secret")
	if got != want {
		t.Fatalf("CallerScopeMetadataKey = %q, want %q", got, want)
	}
	if got == "downstream-secret" {
		t.Fatal("caller scope contains the raw downstream credential")
	}
}

func TestRequestExecutionMetadataTraceCallbackWebsocketDetection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("skips websocket upgrade", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Connection", "Upgrade")
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		logging.SetGinRequestID(ginCtx, "1234abcd")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)

		if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; exists {
			t.Fatal("unexpected selected auth index callback for websocket upgrade")
		}
	})

	t.Run("keeps callback for incomplete upgrade headers", func(t *testing.T) {
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ginCtx.Request.Header.Set("Upgrade", "websocket")
		logging.SetGinRequestID(ginCtx, "1234abcd")
		ctx := context.WithValue(context.Background(), "gin", ginCtx)

		meta := requestExecutionMetadata(ctx)

		if _, exists := meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey]; !exists {
			t.Fatal("missing selected auth index callback for ordinary HTTP request")
		}
	})
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataExtractsValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "priority")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "auto" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "auto")
	}
}

func TestSetServiceTierMetadataPreservesExplicitDefault(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"default"}`))

	if gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]; gotServiceTier != "default" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "default")
	}
}

func TestSetGenerateMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != true {
		t.Fatalf("GenerateMetadataKey = %v, want true", got)
	}
}

func TestSetGenerateMetadataPreservesTrue(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"generate":true}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != true {
		t.Fatalf("GenerateMetadataKey = %v, want true", got)
	}
}

func TestSetGenerateMetadataHonorsExplicitFalse(t *testing.T) {
	meta := make(map[string]any)

	setGenerateMetadata(meta, []byte(`{"generate":false}`))

	if got := meta[coreexecutor.GenerateMetadataKey]; got != false {
		t.Fatalf("GenerateMetadataKey = %v, want false", got)
	}
}

func TestScopedContextPreservesServerValuesWithoutReplacingCancellation(t *testing.T) {
	type key struct{}
	requestCtx := context.WithValue(context.Background(), key{}, "frozen-snapshot")
	requestCtx = coreexecutor.WithCredentialScope(requestCtx, coreexecutor.NewCredentialScope("allowed"))
	calls := 0
	requestCtx = coreexecutor.WithRequestValidator(requestCtx, func(context.Context, string, coreexecutor.Request) error { calls++; return nil })
	observations := 0
	requestCtx = usage.WithSynchronousObserver(requestCtx, func(observedCtx context.Context, _ usage.Record) {
		observations++
		if observedCtx.Value(key{}) != "frozen-snapshot" {
			t.Error("observer lost frozen snapshot")
		}
	})
	parent, parentCancel := context.WithCancel(context.Background())
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)
	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, parent)
	defer cancel()
	if ctx.Value(key{}) != "frozen-snapshot" {
		t.Fatal("server snapshot lost")
	}
	if err := coreexecutor.ValidateRequest(ctx, "openai", coreexecutor.Request{Model: "model"}); err != nil || calls != 1 {
		t.Fatal("validator lost")
	}
	parentCancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("caller cancellation lost")
	}
	manager := usage.NewManager(1)
	manager.Stop()
	manager.Publish(ctx, usage.Record{Model: "model"})
	if observations != 1 {
		t.Fatal("cancelled request lost synchronous observer")
	}
}
