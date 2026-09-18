package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountManagementSharesServerHandlerAndReload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "management-test-secret")
	var shared *management.Handler
	server := newTestServerWithOptions(t, WithManagementHandler(func(handler *management.Handler) { shared = handler }))
	if shared == nil || shared != server.mgmt {
		t.Fatal("account facade must share the server management handler")
	}
	oldDir := server.cfg.AuthDir
	cfg := *server.cfg
	cfg.AuthDir = t.TempDir()
	server.UpdateClients(&cfg)
	// The previously injected pointer must write through the new configuration.
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("POST", "/auth-files?name=reloaded.json", strings.NewReader(`{"type":"codex","access_token":"test-token"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	shared.UploadAuthFile(ctx)
	if rec.Code != 200 {
		t.Fatalf("upload after reload: %d %s", rec.Code, rec.Body)
	}
	if _, errStat := os.Stat(filepath.Join(cfg.AuthDir, "reloaded.json")); errStat != nil {
		t.Fatal(errStat)
	}
	if _, errStat := os.Stat(filepath.Join(oldDir, "reloaded.json")); !os.IsNotExist(errStat) {
		t.Fatal("upload used stale auth directory")
	}
	// Sharing methods is not permission to bypass the original management routes.
	denied := httptest.NewRecorder()
	server.engine.ServeHTTP(denied, httptest.NewRequest("GET", "/v0/management/auth-files", nil))
	if denied.Code != 401 {
		t.Fatalf("original management authentication changed: %d", denied.Code)
	}
}

func TestSharedAccountManagementConfigStatusPreservesReloadHook(t *testing.T) {
	var shared *management.Handler
	reloaded := false
	var server *Server
	server = newTestServerWithOptions(t, WithManagementHandler(func(handler *management.Handler) { shared = handler }), WithConfigReloadHook(func(ctx context.Context, cfg *config.Config) {
		reloaded = len(cfg.CodexKey) == 1 && len(cfg.CodexKey[0].ExcludedModels) == 1 && cfg.CodexKey[0].ExcludedModels[0] == "*"
		for _, candidate := range server.handlers.AuthManager.List() {
			if _, errUpdate := server.handlers.AuthManager.Update(coreauth.WithSkipPersist(ctx), candidate); errUpdate != nil {
				t.Errorf("strict persistence leaked into config reload: %v", errUpdate)
			}
		}
	}))
	cfg := *server.cfg
	cfg.CodexKey = []config.CodexKey{{APIKey: "config-test-secret", BaseURL: "https://example.test/v1"}}
	server.UpdateClients(&cfg)
	if errWrite := os.WriteFile(server.configFilePath, []byte("codex-api-key: []\n"), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	generator := synthesizer.NewStableIDGenerator()
	id, _ := generator.Next("codex:apikey", "config-test-secret", "https://example.test/v1", "", "", "")
	_, errRegister := server.handlers.AuthManager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Attributes: map[string]string{"api_key": "config-test-secret", "base_url": "https://example.test/v1", "source": "config:codex[test]"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if status := shared.ValidateCarpoolAccountTarget(id, "", false); status != 200 {
		t.Fatalf("config target rejected: %d", status)
	}
	body, _ := json.Marshal(map[string]any{"name": id, "disabled": true})
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("PATCH", "/auth-files/status", strings.NewReader(string(body)))
	ctx.Request = ctx.Request.WithContext(management.WithPersistentAuthStatus(ctx.Request.Context()))
	ctx.Request.Header.Set("Content-Type", "application/json")
	shared.PatchAuthFileStatus(ctx)
	if rec.Code != 200 || !reloaded {
		t.Fatalf("config toggle lost persistence/reload hook: %d %s reloaded=%v", rec.Code, rec.Body, reloaded)
	}
	data, errRead := os.ReadFile(server.configFilePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !strings.Contains(string(data), "excluded-models:") {
		t.Fatal("config disable not persisted")
	}
}
