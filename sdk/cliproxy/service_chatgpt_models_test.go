package cliproxy

import (
	"context"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestRegisterModelsForAuthChatGPT guards the routing prerequisite for credentials
// created by the Sign in with ChatGPT flow: the scheduler only offers an auth for a
// model that the model registry has registered for it, so an auth that publishes no
// models is never selected and the provider would be unusable.
func TestRegisterModelsForAuthChatGPT(t *testing.T) {
	authID := "chatgpt-models-oauth"
	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "chatgpt",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "chatgpt", "access_token": "test-token"},
	}
	service.registerModelsForAuth(context.Background(), auth)

	models := modelRegistry.GetModelsForClient(authID)
	if len(models) == 0 {
		t.Fatal("chatgpt auth published no models; it would never be selected")
	}

	// The registry must report the chatgpt provider for a model this account serves,
	// otherwise the conductor never routes the model to ChatGPTExecutor.
	providers := modelRegistry.GetModelProviders("gpt-5.6-sol")
	found := false
	for _, provider := range providers {
		if provider == "chatgpt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("gpt-5.6-sol providers = %v, want chatgpt to be included", providers)
	}
}

func TestRegisterModelsForAuthChatGPTAppliesExclusions(t *testing.T) {
	authID := "chatgpt-models-excluded"
	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	defaultModels := internalregistry.GetCodexProModels()
	if len(defaultModels) < 2 {
		t.Fatalf("expected at least two default models, got %d", len(defaultModels))
	}
	excludedID := defaultModels[0].ID

	service := &Service{cfg: &config.Config{OAuthExcludedModels: map[string][]string{"chatgpt": {excludedID}}}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "chatgpt",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "chatgpt"},
	}
	service.registerModelsForAuth(context.Background(), auth)

	for _, model := range modelRegistry.GetModelsForClient(authID) {
		if model != nil && model.ID == excludedID {
			t.Fatalf("excluded model %q is still registered", excludedID)
		}
	}
	if len(modelRegistry.GetModelsForClient(authID)) == 0 {
		t.Fatal("expected remaining models to stay registered")
	}
}
