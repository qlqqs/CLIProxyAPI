package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRouterConfiguratorsCompose(t *testing.T) {
	var calls []string
	server := newTestServerWithOptions(t,
		WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			calls = append(calls, "first")
			engine.GET("/option-first", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		}),
		WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			calls = append(calls, "second")
			engine.GET("/option-second", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		}),
	)
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("router configurator calls = %#v", calls)
	}
	for _, path := range []string{"/option-first", "/option-second"} {
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("GET %s status = %d", path, response.Code)
		}
	}
}

func TestRequestCompletionObserversCompose(t *testing.T) {
	var calls []string
	server := newTestServerWithOptions(t,
		WithRequestCompletionObserver(func(_ context.Context, _ pluginapi.RequestCompletion) {
			calls = append(calls, "first")
		}),
		WithRequestCompletionObserver(func(_ context.Context, _ pluginapi.RequestCompletion) {
			calls = append(calls, "second")
		}),
	)
	server.handlers.RequestCompletionObserver(context.Background(), pluginapi.RequestCompletion{RequestID: "request-1"})
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("completion observer calls = %#v", calls)
	}
}

func TestNoRouteHandlersComposeBeforePluginFallback(t *testing.T) {
	var calls []string
	server := newTestServerWithOptions(t,
		WithNoRouteHandler(func(*gin.Context) bool {
			calls = append(calls, "first")
			return false
		}),
		WithNoRouteHandler(func(c *gin.Context) bool {
			calls = append(calls, "second")
			c.JSON(http.StatusNotFound, gin.H{"error": "custom"})
			return true
		}),
	)

	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/custom/missing", nil))
	if response.Code != http.StatusNotFound || response.Body.String() != "{\"error\":\"custom\"}" {
		t.Fatalf("NoRoute response = %d %q", response.Code, response.Body.String())
	}
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("NoRoute calls = %#v", calls)
	}
}
