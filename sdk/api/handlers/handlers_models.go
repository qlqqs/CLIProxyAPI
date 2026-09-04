package handlers

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// RequestContext returns the inbound request context when available.
func RequestContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}

// AvailableModelsForContext returns the legacy global model list unless the
// request carries an enforced credential scope.
func AvailableModelsForContext(ctx context.Context, handlerType string) []map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	scope := coreexecutor.CredentialScopeFromContext(ctx)
	if scope == nil {
		return modelRegistry.GetAvailableModels(handlerType)
	}
	return modelRegistry.GetAvailableModelsForClients(handlerType, scope.IDs())
}

// AvailableModelInfosForContext returns legacy global metadata unless the
// request carries an enforced credential scope.
func AvailableModelInfosForContext(ctx context.Context) []*registry.ModelInfo {
	modelRegistry := registry.GetGlobalRegistry()
	scope := coreexecutor.CredentialScopeFromContext(ctx)
	if scope == nil {
		return modelRegistry.GetAvailableModelInfos()
	}
	return modelRegistry.GetAvailableModelInfosForClients(scope.IDs())
}
