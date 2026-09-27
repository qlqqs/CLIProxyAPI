package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	service "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCarpoolDeleteRejectsForeignProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	name := "foreign-claude.json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{"type":"claude","access_token":"synthetic"}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{ID: name, FileName: name, Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{"path": path}})
	if err != nil {
		t.Fatal(err)
	}
	a := &API{}
	a.originValidator, _ = NewOriginValidator(nil)
	a.SetAccountManagement(management.NewHandler(&config.Config{AuthDir: dir}, "", manager))
	identity := service.SessionIdentity{User: domain.User{Role: domain.UserRoleAdmin}, Session: domain.Session{ID: "review-owner", CSRFToken: "review-csrf"}}
	engine := gin.New()
	group := engine.Group("/admin", func(c *gin.Context) { c.Set(identityContextKey, identity) }, requireChangedPassword, requireAdmin)
	a.registerAccountManagement(group)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://test/admin"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://test")
		req.Header.Set(csrfHeaderName, "review-csrf")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	list := request(http.MethodGet, "/auth-files", "")
	if list.Code != 200 || strings.Contains(list.Body.String(), name) {
		t.Fatalf("restricted list: %d %s", list.Code, list.Body)
	}
	if got := request(http.MethodPatch, "/auth-files/status", `{"name":"`+name+`","disabled":true}`); got.Code != 403 {
		t.Fatalf("restricted status: %d %s", got.Code, got.Body)
	}
	// The index is only a deterministic hash of provider + path, not an authorization secret.
	response := request(http.MethodDelete, "/auth-files?"+url.Values{"name": {name}, "auth_index": {registered.Index}}.Encode(), "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("delete: %d %s", response.Code, response.Body)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("protected file lost: %v", err)
	}
	if _, ok := manager.GetByID(name); !ok {
		t.Fatal("protected runtime credential lost")
	}
}

func TestCarpoolLoginSourceLimitSurvivesUsernameRotation(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	for i := 0; i < 5; i++ {
		response := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": "nonexistent", "password": "wrong-password"}, nil, "", true)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", i, response.Code, response.Body)
		}
	}
	limited := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": "nonexistent", "password": "wrong-password"}, nil, "", true)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("same-user not limited: %d", limited.Code)
	}
	for i := 0; i < 30; i++ {
		response := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": fmt.Sprintf("nonexistent-%d", i), "password": "wrong-password"}, nil, "", true)
		if response.Code == http.StatusTooManyRequests {
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("source rate limit lacks Retry-After")
			}
			return
		}
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("rotated attempt %d: %d %s", i, response.Code, response.Body)
		}
	}
	t.Fatal("rotating nonexistent usernames bypassed the source budget")
}
