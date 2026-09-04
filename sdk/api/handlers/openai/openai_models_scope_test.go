package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestOpenAIModelHandlersUseCredentialScope(t *testing.T) {
	const (
		allowedClient = "openai-scope-allowed-client"
		outsideClient = "openai-scope-outside-client"
		allowedModel  = "openai-scope-allowed-model"
		outsideModel  = "openai-scope-outside-model"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(allowedClient, "openai", []*registry.ModelInfo{{ID: allowedModel, Object: "model"}})
	modelRegistry.RegisterClient(outsideClient, "openai", []*registry.ModelInfo{{ID: outsideModel, Object: "model"}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(allowedClient)
		modelRegistry.UnregisterClient(outsideClient)
	})
	requestContext := coreexecutor.WithCredentialScope(context.Background(), coreexecutor.NewCredentialScope(allowedClient))
	baseHandler := &handlers.BaseAPIHandler{}

	for _, test := range []struct {
		name string
		path string
		run  func(*gin.Context)
		key  string
	}{
		{name: "OpenAI", path: "/v1/models", run: NewOpenAIAPIHandler(baseHandler).OpenAIModels, key: "data"},
		{name: "Responses", path: "/v1/models", run: NewOpenAIResponsesAPIHandler(baseHandler).OpenAIResponsesModels, key: "data"},
		{name: "Codex client", path: "/v1/models?client_version=0.149.1", run: NewOpenAIAPIHandler(baseHandler).OpenAIModels, key: "models"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodGet, test.path, nil).WithContext(requestContext)
			test.run(ginContext)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}

			var response map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
				t.Fatalf("decode response: %v", errUnmarshal)
			}
			var models []map[string]any
			if errUnmarshal := json.Unmarshal(response[test.key], &models); errUnmarshal != nil {
				t.Fatalf("decode %s models: %v; body=%s", test.name, errUnmarshal, recorder.Body.String())
			}
			idField := "id"
			if test.key == "models" {
				idField = "slug"
			}
			if !wireModelsContain(models, idField, allowedModel) {
				t.Fatalf("response does not contain allowed model %q: %s", allowedModel, recorder.Body.String())
			}
			if wireModelsContain(models, idField, outsideModel) {
				t.Fatalf("response contains outside model %q: %s", outsideModel, recorder.Body.String())
			}
		})
	}
}

func wireModelsContain(models []map[string]any, field, modelID string) bool {
	for _, model := range models {
		if value, _ := model[field].(string); value == modelID {
			return true
		}
	}
	return false
}
