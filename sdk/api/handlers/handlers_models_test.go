package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAvailableModelsForContextUsesCredentialScope(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	const (
		allowedClient = "handlers-scope-allowed-client"
		outsideClient = "handlers-scope-outside-client"
		allowedModel  = "handlers-scope-allowed-model"
		outsideModel  = "handlers-scope-outside-model"
	)
	modelRegistry.RegisterClient(allowedClient, "openai", []*registry.ModelInfo{{ID: allowedModel}})
	modelRegistry.RegisterClient(outsideClient, "openai", []*registry.ModelInfo{{ID: outsideModel}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(allowedClient)
		modelRegistry.UnregisterClient(outsideClient)
	})

	scopedCtx := coreexecutor.WithCredentialScope(context.Background(), coreexecutor.NewCredentialScope(allowedClient))
	models := AvailableModelsForContext(scopedCtx, "openai")
	if !handlerModelsContain(models, allowedModel) {
		t.Fatalf("scoped models do not contain %q: %#v", allowedModel, models)
	}
	if handlerModelsContain(models, outsideModel) {
		t.Fatalf("scoped models contain outside model %q: %#v", outsideModel, models)
	}

	infos := AvailableModelInfosForContext(scopedCtx)
	if !handlerModelInfosContain(infos, allowedModel) || handlerModelInfosContain(infos, outsideModel) {
		t.Fatalf("scoped model infos = %#v", infos)
	}
	if models := AvailableModelsForContext(coreexecutor.WithCredentialScope(context.Background(), coreexecutor.NewCredentialScope()), "openai"); len(models) != 0 {
		t.Fatalf("empty enforced scope returned %#v", models)
	}
	if models := AvailableModelsForContext(context.Background(), "openai"); !handlerModelsContain(models, allowedModel) || !handlerModelsContain(models, outsideModel) {
		t.Fatalf("legacy model list did not preserve global behavior: %#v", models)
	}
}

func TestRequestContextHandlesMissingRequest(t *testing.T) {
	if RequestContext(nil) == nil {
		t.Fatal("RequestContext(nil) returned nil")
	}
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	if RequestContext(ginContext) == nil {
		t.Fatal("RequestContext() returned nil for a Gin context without a request")
	}
}

func handlerModelsContain(models []map[string]any, modelID string) bool {
	for _, model := range models {
		if id, _ := model["id"].(string); id == modelID {
			return true
		}
	}
	return false
}

func handlerModelInfosContain(models []*registry.ModelInfo, modelID string) bool {
	for _, model := range models {
		if model != nil && model.ID == modelID {
			return true
		}
	}
	return false
}
