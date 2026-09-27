package management

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}

func TestManagementMiddlewareIgnoresForwardedIPForAuthorizationAndBans(t *testing.T) {
	hash, errHash := bcrypt.GenerateFromPassword([]byte("synthetic-management-key"), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	for _, allowRemote := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_remote=%t", allowRemote), func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RemoteManagement.SecretKey = string(hash)
			cfg.RemoteManagement.AllowRemote = allowRemote
			h := &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo)}
			engine := gin.New()
			engine.GET("/config", h.Middleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
			request := func(peer, forwarded, key string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/config", nil)
				req.RemoteAddr = peer
				if forwarded != "" {
					req.Header.Set("X-Forwarded-For", forwarded)
					req.Header.Set("X-Real-IP", forwarded)
				}
				req.Header.Set("X-Management-Key", key)
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, req)
				return rec
			}
			if !allowRemote {
				for _, spoof := range []string{"127.0.0.1", "::1", "198.51.100.2, 127.0.0.1"} {
					if got := request("203.0.113.25:49152", spoof, "synthetic-management-key"); got.Code != http.StatusForbidden {
						t.Fatalf("spoof %q status = %d", spoof, got.Code)
					}
				}
			}
			for i := 0; i < 5; i++ {
				spoof := "127.0.0.1"
				want := http.StatusForbidden
				if allowRemote {
					spoof = fmt.Sprintf("192.0.2.%d", i+1)
					want = http.StatusUnauthorized
				}
				if got := request("203.0.113.25:49152", spoof, "wrong"); got.Code != want {
					t.Fatalf("attempt %d status = %d, want %d", i, got.Code, want)
				}
			}
			if got := request("203.0.113.25:49152", "127.0.0.1", "synthetic-management-key"); got.Code != http.StatusForbidden {
				t.Fatalf("remote address bypassed rejection: %d", got.Code)
			}
			for _, peer := range []string{"127.0.0.1:50000", "[::1]:50000"} {
				if got := request(peer, "", "synthetic-management-key"); got.Code != http.StatusOK {
					t.Fatalf("real loopback %q was rejected/banned: %d", peer, got.Code)
				}
			}
		})
	}
}

func TestAuthenticateManagementKeyLocalPasswordWithoutGlobalSecret(t *testing.T) {
	for _, tc := range []struct {
		name, key          string
		local, allowRemote bool
		want               int
	}{
		{name: "local correct", key: "synthetic-local", local: true, want: 0},
		{name: "local incorrect", key: "wrong", local: true, want: http.StatusUnauthorized},
		{name: "local missing", local: true, want: http.StatusUnauthorized},
		{name: "remote disabled", key: "synthetic-local", want: http.StatusForbidden},
		{name: "remote enabled", key: "synthetic-local", allowRemote: true, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RemoteManagement.AllowRemote = tc.allowRemote
			h := &Handler{cfg: cfg, localPassword: "synthetic-local", failedAttempts: make(map[string]*attemptInfo)}
			allowed, status, _ := h.AuthenticateManagementKey("127.0.0.1", tc.local, tc.key)
			if status != tc.want || allowed != (tc.want == 0) {
				t.Fatalf("allowed=%t status=%d, want %d", allowed, status, tc.want)
			}
		})
	}
}

func TestManagementLoopbackProxyCannotInheritLocalPrivilegesOrPoisonDirectBan(t *testing.T) {
	hash, errHash := bcrypt.GenerateFromPassword([]byte("synthetic-global"), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	headers := []struct{ name, value string }{
		{"X-Forwarded-For", "203.0.113.8"},
		{"X-Forwarded-For", "127.0.0.1"},
		{"X-Forwarded-For", ""},
		{"X-Real-IP", "::1"},
		{"Forwarded", `for="[::1]";proto=https`},
		{"X-Forwarded-Proto", "https"},
		{"X-Original-Forwarded-For", "127.0.0.1"},
		{"CF-Connecting-IP", "127.0.0.1"},
		{"True-Client-IP", "127.0.0.1"},
	}
	for _, allowRemote := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_remote=%t", allowRemote), func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RemoteManagement.AllowRemote = allowRemote
			cfg.RemoteManagement.SecretKey = string(hash)
			h := &Handler{cfg: cfg, localPassword: "synthetic-local", failedAttempts: make(map[string]*attemptInfo)}
			engine := gin.New()
			engine.GET("/config", h.Middleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
			request := func(peer, header, value, key string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/config", nil)
				req.RemoteAddr = peer
				if header != "" {
					req.Header.Set(header, value)
				}
				req.Header.Set("X-Management-Key", key)
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, req)
				return rec
			}
			for _, peer := range []string{"127.0.0.1:49152", "[::1]:49152"} {
				for _, header := range headers {
					wantGlobal, wantLocal := http.StatusForbidden, http.StatusForbidden
					if allowRemote {
						wantGlobal, wantLocal = http.StatusOK, http.StatusUnauthorized
					}
					response := request(peer, header.name, header.value, "synthetic-global")
					if response.Code != wantGlobal {
						t.Fatalf("forwarded global key %s=%q: status %d, want %d", header.name, header.value, response.Code, wantGlobal)
					}
					response = request(peer, header.name, header.value, "synthetic-local")
					if response.Code != wantLocal {
						t.Fatalf("forwarded local key %s=%q: status %d, want %d", header.name, header.value, response.Code, wantLocal)
					}
				}
				if allowRemote {
					if response := request(peer, "Forwarded", "for=203.0.113.8", "synthetic-global"); response.Code != http.StatusOK {
						t.Fatalf("valid forwarded global key rejected: %d", response.Code)
					}
					for i := 0; i < 5; i++ {
						response := request(peer, "X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i+1), "wrong")
						if response.Code != http.StatusUnauthorized {
							t.Fatalf("forwarded failure %d: status %d", i, response.Code)
						}
					}
					// Changing both header families and their values cannot escape the peer's forwarded ban.
					response := request(peer, "Forwarded", "for=127.0.0.1", "synthetic-global")
					if response.Code != http.StatusForbidden {
						t.Fatalf("forwarded requests escaped ban: %d", response.Code)
					}
				}
				response := request(peer, "", "", "synthetic-local")
				if response.Code != http.StatusOK {
					t.Fatalf("forwarded failures poisoned direct-local ban: %d", response.Code)
				}
				response = request(peer, "", "", "synthetic-global")
				if response.Code != http.StatusOK {
					t.Fatalf("direct-local global password rejected: %d", response.Code)
				}
			}
		})
	}
}
