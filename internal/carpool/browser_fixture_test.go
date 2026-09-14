//go:build carpool_browser

package carpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestCarpoolBrowserFixture runs only as a dedicated opt-in test process. All
// credentials and accounting here are synthetic; browser actions use real APIs.
func TestCarpoolBrowserFixture(t *testing.T) {
	if os.Getenv("CARPOOL_BROWSER") != "1" {
		t.Skip("set CARPOOL_BROWSER=1 to run the loopback browser fixture")
	}
	readyPath := os.Getenv("CARPOOL_BROWSER_READY_FILE")
	if readyPath == "" {
		t.Fatal("CARPOOL_BROWSER_READY_FILE is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root := t.TempDir()
	// Production rejects databases under os.TempDir; keep the isolated test root
	// while redirecting the reported temp directory as in module lifecycle tests.
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, filepath.Join(root, "reported-temp"))
	}
	t.Setenv("MANAGEMENT_PASSWORD", "")
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	t.Cleanup(func() {
		if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			t.Error(errClose)
		}
	})
	origin := "http://" + listener.Addr().String()
	carpoolCfg := config.DefaultCarpoolConfig()
	carpoolCfg.Enabled = true
	carpoolCfg.DatabasePath = filepath.Join(root, "carpool.db")
	carpoolCfg.Session.CookieSecure = false
	carpoolCfg.TrustedOrigins = []string{origin}
	// Catalog refresh stays entirely offline, using the same embedded catalog.
	carpoolCfg.Pricing.CatalogURL = origin + "/__fixture/catalog"
	cfg := &config.Config{Carpool: carpoolCfg, AuthDir: filepath.Join(root, "auth")}
	cfg.RemoteManagement.DisableControlPanel = true
	configPath := filepath.Join(root, "unused-config.yaml")
	manager := coreauth.NewManager(nil, nil, nil)
	executor := &browserFixtureExecutor{}
	manager.RegisterExecutor(executor)
	module, errOpen := Open(ctx, cfg, configPath, manager)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		usage.StopDefault()
		if errClose := module.Close(context.Background()); errClose != nil {
			t.Error(errClose)
		}
	})
	const adminPassword = "Fake-Browser-Admin-Only-2026!"
	admin, errAdmin := module.Control().BootstrapAdmin(ctx, "qa-admin", "QA Administrator", adminPassword)
	if errAdmin != nil {
		t.Fatal(errAdmin)
	}
	type login struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	passengers := make([]login, 0, 2)
	for index, name := range []string{"QA Shared Car", "QA Isolated Car"} {
		car, errCar := module.Control().CreateCar(ctx, admin, name, "Synthetic browser fixture; no real upstream accounts", nil)
		if errCar != nil {
			t.Fatal(errCar)
		}
		passenger, errPassenger := module.Control().CreateUser(ctx, admin, fmt.Sprintf("qa-passenger-%d", index+1), fmt.Sprintf("QA Passenger %d", index+1), domain.UserRolePassenger)
		if errPassenger != nil {
			t.Fatal(errPassenger)
		}
		passengers = append(passengers, login{passenger.User.Username, passenger.TemporaryPassword})
		limit := int64(1_000_000_000)
		if _, errMember := module.Control().MoveMemberWithLimit(ctx, admin, car.CarRef, passenger.User.UserRef, passenger.User.DefaultDisplayName, &limit); errMember != nil {
			t.Fatal(errMember)
		}
		authID := fmt.Sprintf("qa-fake-openai-%d", index+1)
		if _, errRegister := manager.Register(ctx, &coreauth.Auth{ID: authID, Provider: "openai", Status: coreauth.StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
		models := []*registry.ModelInfo{{ID: "gpt-4o", Object: "model", OwnedBy: "openai"}, {ID: "qa-price-missing", Object: "model", OwnedBy: "openai"}}
		if index == 1 {
			models = append(models, &registry.ModelInfo{ID: "qa-isolated-only", Object: "model", OwnedBy: "openai"})
		}
		registry.GetGlobalRegistry().RegisterClient(authID, "openai", models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errAssign := module.Control().MoveAccount(ctx, admin, car.CarRef, authID, fmt.Sprintf("QA Fake Account %d", index+1)); errAssign != nil {
			t.Fatal(errAssign)
		}

		if index == 0 {
			for _, observation := range []struct {
				id, provider, label string
				quota               coreauth.QuotaState
			}{
				{"qa-fake-claude", "claude", "QA Numeric and Status Only", coreauth.QuotaState{ObservedAt: time.Now().UTC(), Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "allowed", "Anthropic-Ratelimit-Unified-5h-Utilization": "0.53", "Anthropic-Ratelimit-Unified-7d-Status": "rejected"}}},
				{"qa-fake-codex-stale", "codex", "QA Stale Observation", coreauth.QuotaState{ObservedAt: time.Now().UTC().Add(-24 * time.Hour), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "81"}}},
				{"qa-fake-codex-missing", "codex", "QA Missing Observation", coreauth.QuotaState{}},
			} {
				if _, errRegister := manager.Register(ctx, &coreauth.Auth{ID: observation.id, Provider: observation.provider, Status: coreauth.StatusActive, Quota: observation.quota}); errRegister != nil {
					t.Fatal(errRegister)
				}
				if _, errAssign := module.Control().MoveAccount(ctx, admin, car.CarRef, observation.id, observation.label); errAssign != nil {
					t.Fatal(errAssign)
				}
			}
		}
	}
	accessManager := sdkaccess.NewManager()
	var engine *gin.Engine
	options := append(module.ServerOptions(), api.WithEngineConfigurator(func(e *gin.Engine) { engine = e }), api.WithRequestLoggerFactory(nil))
	api.NewServer(cfg, manager, accessManager, configPath, options...)
	accessManager.SetProviders([]sdkaccess.Provider{module.Provider()})
	accessManager.SetAuthenticatedRequestHook(module.AuthenticatedRequestHook)
	engine.GET("/__fixture/calls", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"calls": executor.calls.Load()}) })
	catalog := append([]byte(nil), module.pricing.Current().Raw...)
	engine.GET("/__fixture/catalog", func(c *gin.Context) { c.Data(http.StatusOK, "application/json", catalog) })
	module.Start()
	server := &http.Server{Handler: engine}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		if errShutdown := server.Shutdown(context.Background()); errShutdown != nil {
			t.Error(errShutdown)
		}
	})
	ready, errMarshal := json.Marshal(map[string]any{"url": origin, "admin": login{"qa-admin", adminPassword}, "passengers": passengers, "synthetic": true, "model": "gpt-4o", "missing_model": "qa-price-missing", "input_tokens": 10000, "output_tokens": 2000})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	// Exclusive creation prevents following a caller-supplied symlink or replacing
	// an unrelated file. The caller must remove stale readiness files explicitly.
	readyFile, errReady := os.OpenFile(readyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errReady != nil {
		t.Fatal(errReady)
	}
	t.Cleanup(func() {
		if errRemove := os.Remove(readyPath); errRemove != nil && !os.IsNotExist(errRemove) {
			t.Error(errRemove)
		}
	})
	_, errWrite := readyFile.Write(ready)
	errClose := readyFile.Close()
	if errWrite != nil || errClose != nil {
		t.Fatal(errors.Join(errWrite, errClose))
	}
	select {
	case <-ctx.Done():
	case errServe := <-serveErr:
		if !errors.Is(errServe, http.ErrServerClosed) {
			t.Error(errServe)
		}
	}
}

type browserFixtureExecutor struct{ calls atomic.Int64 }

func (*browserFixtureExecutor) Identifier() string { return "openai" }
func (e *browserFixtureExecutor) publish(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, stream bool) {
	sequence := e.calls.Add(1)
	requestID := opts.RequestID
	if requestID == "" {
		requestID = usage.RequestIDFromContext(ctx)
	}
	usage.PublishRecord(ctx, usage.Record{
		EventID: fmt.Sprintf("qa-event-%d", sequence), RequestID: requestID,
		AuthID: auth.ID, AuthIndex: auth.Index, Provider: "openai", ExecutorType: "openai", Model: req.Model,
		UsageKnown: true, Stream: stream, RequestedAt: time.Now().UTC(), ResponseServiceTier: "default",
		Detail: usage.Detail{InputTokens: 10000, OutputTokens: 2000, TotalTokens: 12000, TokenBreakdown: usage.NewSubsetTokenBreakdown(10000, 0, 0, 2000, 0, 12000)},
	})
}
func (e *browserFixtureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.publish(ctx, auth, req, opts, false)
	return coreexecutor.Response{Payload: []byte(`{"id":"qa-completion","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Synthetic browser QA response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`)}, nil
}
func (e *browserFixtureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.publish(ctx, auth, req, opts, true)
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"qa-stream","object":"chat.completion.chunk","created":0,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Synthetic browser QA stream"},"finish_reason":null}]}`)}
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"id":"qa-stream","object":"chat.completion.chunk","created":0,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":2000,"total_tokens":12000}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}
func (*browserFixtureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*browserFixtureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("browser fixture supports chat generation only")
}
func (*browserFixtureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("browser fixture forbids upstream network requests")
}

var _ coreauth.ProviderExecutor = (*browserFixtureExecutor)(nil)
