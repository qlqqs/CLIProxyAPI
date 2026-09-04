package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGeminiModelsResponseUsesConfiguredDisplayName(t *testing.T) {
	const clientID = "gemini-display-name-catalog-test"
	const modelID = "gemini-display-name-catalog-test"
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(clientID, "gemini", []*registry.ModelInfo{{
		ID: modelID, Name: modelID, DisplayName: "Configured Gemini Name",
	}})
	t.Cleanup(func() {
		registryRef.UnregisterClient(clientID)
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	NewGeminiAPIHandler(&handlers.BaseAPIHandler{}).GeminiModels(ctx)

	var response struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	for _, model := range response.Models {
		if model.Name == "models/"+modelID {
			if model.DisplayName != "Configured Gemini Name" {
				t.Fatalf("displayName = %q, want Configured Gemini Name", model.DisplayName)
			}
			return
		}
	}
	t.Fatalf("model %q not found in response", modelID)
}

func TestGeminiModelHandlersUseCredentialScope(t *testing.T) {
	const (
		allowedClient = "gemini-scope-allowed-client"
		outsideClient = "gemini-scope-outside-client"
		allowedModel  = "gemini-scope-allowed-model"
		outsideModel  = "gemini-scope-outside-model"
	)
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(allowedClient, "gemini", []*registry.ModelInfo{{ID: allowedModel, Name: allowedModel}})
	registryRef.RegisterClient(outsideClient, "gemini", []*registry.ModelInfo{{ID: outsideModel, Name: outsideModel}})
	t.Cleanup(func() {
		registryRef.UnregisterClient(allowedClient)
		registryRef.UnregisterClient(outsideClient)
	})
	handler := NewGeminiAPIHandler(&handlers.BaseAPIHandler{})
	requestContext := coreexecutor.WithCredentialScope(context.Background(), coreexecutor.NewCredentialScope(allowedClient))

	listRecorder := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listRecorder)
	listContext.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil).WithContext(requestContext)
	handler.GeminiModels(listContext)
	if !strings.Contains(listRecorder.Body.String(), "models/"+allowedModel) {
		t.Fatalf("scoped list does not contain %q: %s", allowedModel, listRecorder.Body.String())
	}
	if strings.Contains(listRecorder.Body.String(), outsideModel) {
		t.Fatalf("scoped list contains outside model %q: %s", outsideModel, listRecorder.Body.String())
	}

	for _, test := range []struct {
		name   string
		model  string
		status int
	}{
		{name: "allowed", model: allowedModel, status: http.StatusOK},
		{name: "outside", model: outsideModel, status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/"+test.model, nil).WithContext(requestContext)
			ginContext.Params = gin.Params{{Key: "action", Value: "/" + test.model}}
			handler.GeminiGetHandler(ginContext)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.status, recorder.Body.String())
			}
		})
	}
}
