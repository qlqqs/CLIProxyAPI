package claude

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestClaudeModelsResponseUsesConfiguredDisplayName(t *testing.T) {
	const clientID = "claude-display-name-catalog-test"
	const modelID = "claude-display-name-catalog-test"
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID: modelID, Object: "model", OwnedBy: "test", DisplayName: "Configured Claude Name",
	}})
	t.Cleanup(func() {
		registryRef.UnregisterClient(clientID)
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{}).ClaudeModels(ctx)

	var response struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	for _, model := range response.Data {
		if model.ID == modelID {
			if model.DisplayName != "Configured Claude Name" {
				t.Fatalf("display_name = %q, want Configured Claude Name", model.DisplayName)
			}
			return
		}
	}
	t.Fatalf("model %q not found in response", modelID)
}

func TestClaudeModelsResponseUsesCredentialScope(t *testing.T) {
	const (
		allowedClient = "claude-scope-allowed-client"
		outsideClient = "claude-scope-outside-client"
		allowedModel  = "claude-scope-allowed-model"
		outsideModel  = "claude-scope-outside-model"
	)
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(allowedClient, "claude", []*registry.ModelInfo{{ID: allowedModel, Object: "model"}})
	registryRef.RegisterClient(outsideClient, "claude", []*registry.ModelInfo{{ID: outsideModel, Object: "model"}})
	t.Cleanup(func() {
		registryRef.UnregisterClient(allowedClient)
		registryRef.UnregisterClient(outsideClient)
	})

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request = request.WithContext(coreexecutor.WithCredentialScope(context.Background(), coreexecutor.NewCredentialScope(allowedClient)))
	ginContext.Request = request
	NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{}).ClaudeModels(ginContext)

	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	var foundAllowed bool
	for _, model := range response.Data {
		if model.ID == outsideModel {
			t.Fatalf("scoped response contains outside model %q", outsideModel)
		}
		if model.ID == allowedModel {
			foundAllowed = true
		}
	}
	if !foundAllowed {
		t.Fatalf("scoped response does not contain %q: %s", allowedModel, recorder.Body.String())
	}
}

func TestClaudeModelsResponseDisablesModelListCloaking(t *testing.T) {
	const clientID = "claude-disable-model-list-cloaking-test"
	const modelID = "gpt-disable-model-list-cloaking-test"
	registryRef := registry.GetGlobalRegistry()
	registryRef.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID: modelID, Object: "model", OwnedBy: "test",
	}})
	t.Cleanup(func() {
		registryRef.UnregisterClient(clientID)
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	baseHandler := &handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{
		ClaudeCode: sdkconfig.ClaudeCodeConfig{DisableCloakingModelList: true},
	}}
	NewClaudeCodeAPIHandler(baseHandler).ClaudeModels(ctx)

	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(recorder.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	for _, model := range response.Data {
		if model.ID == modelID {
			return
		}
	}
	t.Fatalf("uncloaked model %q not found in response", modelID)
}

func TestRewriteClaudeDDModelInBody(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantModel string
	}{
		{
			name:      "encoded model is decoded",
			body:      `{"model":"claude-fable-5-dd-o4-tpg","messages":[]}`,
			wantModel: "gpt-4o",
		},
		{
			name:      "plain claude model unchanged",
			body:      `{"model":"claude-sonnet-4-6","messages":[]}`,
			wantModel: "claude-sonnet-4-6",
		},
		{
			name:      "encoded model with thinking suffix",
			body:      `{"model":"claude-fable-5-dd-o4-tpg(high)","stream":true}`,
			wantModel: "gpt-4o(high)",
		},
		{
			name:      "missing model field unchanged",
			body:      `{"messages":[]}`,
			wantModel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteClaudeDDModelInBody([]byte(tt.body))
			if model := gjson.GetBytes(got, "model").String(); model != tt.wantModel {
				t.Fatalf("model = %q, want %q; body=%s", model, tt.wantModel, string(got))
			}
		})
	}
}
