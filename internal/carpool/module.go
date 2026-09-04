// Package carpool wires the optional user and vehicle sharing module.
package carpool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/accounting"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/httpapi"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const accountingPluginName = "carpool-accounting"

// Module owns all carpool resources for one service process.
type Module struct {
	store      *carpoolsqlite.Store
	control    *carpoolservice.Control
	httpAPI    *httpapi.API
	provider   sdkaccess.Provider
	writer     *accounting.Writer
	retention  *retentionCleaner
	startOnce  sync.Once
	closeOnce  sync.Once
	closeError error
}

// Open validates fixed dependencies, opens SQLite, recovers interrupted requests, and builds the module.
func Open(ctx context.Context, cfg *config.Config, configPath string, authCatalog carpoolservice.AuthCatalog) (*Module, error) {
	if cfg == nil || !cfg.Carpool.Enabled {
		return nil, fmt.Errorf("carpool: module is disabled")
	}
	if errValidate := cfg.ValidateCarpool(); errValidate != nil {
		return nil, errValidate
	}
	carpoolCfg := cfg.Carpool
	if !carpoolCfg.Session.CookieSecure {
		log.Warn("carpool session cookie Secure attribute is disabled; use only for local HTTP development")
	}
	databasePath, errPath := carpoolCfg.DatabasePathForConfig(configPath)
	if errPath != nil {
		return nil, errPath
	}
	absoluteTTL, idleTTL, errTTL := carpoolCfg.SessionDurations()
	if errTTL != nil {
		return nil, errTTL
	}
	reportLocation, errLocation := time.LoadLocation(carpoolCfg.ReportTimezone)
	if errLocation != nil {
		return nil, fmt.Errorf("carpool: load report timezone: %w", errLocation)
	}
	trustedProxies, errProxies := httpapi.ParseTrustedProxyCIDRs(carpoolCfg.TrustedProxyCIDRs)
	if errProxies != nil {
		return nil, errProxies
	}

	store, errStore := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: databasePath})
	if errStore != nil {
		return nil, fmt.Errorf("carpool: open database: %w", errStore)
	}
	closeStore := func(operationErr error) error {
		if errClose := store.Close(); errClose != nil {
			return errors.Join(operationErr, errClose)
		}
		return operationErr
	}
	now := time.Now
	if _, errRecover := store.RecoverInterruptedRequests(ctx, now().UTC()); errRecover != nil {
		return nil, closeStore(fmt.Errorf("carpool: recover interrupted requests: %w", errRecover))
	}
	control, errControl := carpoolservice.NewControl(store, authCatalog, carpoolservice.ControlConfig{
		SessionAbsoluteTTL: absoluteTTL,
		SessionIdleTTL:     idleTTL,
		ReportLocation:     reportLocation,
		UsageRetention:     time.Duration(carpoolCfg.UsageRetentionDays) * 24 * time.Hour,
		HomeEnabled:        cfg.Home.Enabled,
		Now:                now,
	})
	if errControl != nil {
		return nil, closeStore(errControl)
	}
	browserAPI, errAPI := httpapi.New(control, httpapi.Config{
		CookieSecure:    carpoolCfg.Session.CookieSecure,
		SessionTTL:      absoluteTTL,
		TrustedOrigins:  carpoolCfg.TrustedOrigins,
		TrustedProxyNet: trustedProxies,
		Now:             now,
	})
	if errAPI != nil {
		return nil, closeStore(errAPI)
	}
	writer, errWriter := accounting.NewWriter(store, accounting.Config{Now: now})
	if errWriter != nil {
		return nil, closeStore(errWriter)
	}
	return &Module{
		store:    store,
		control:  control,
		httpAPI:  browserAPI,
		provider: carpoolaccess.NewProvider(store, now),
		writer:   writer,
		retention: newRetentionCleaner(store, retentionCleanerConfig{
			UsageRetention: time.Duration(carpoolCfg.UsageRetentionDays) * 24 * time.Hour,
			AuditRetention: time.Duration(carpoolCfg.AuditRetentionDays) * 24 * time.Hour,
			Now:            now,
		}),
	}, nil
}

// Provider returns the module's request authentication provider.
func (m *Module) Provider() sdkaccess.Provider {
	if m == nil {
		return nil
	}
	return m.provider
}

// Control returns the module's business service for CLI-only operations.
func (m *Module) Control() *carpoolservice.Control {
	if m == nil {
		return nil
	}
	return m.control
}

// AuthenticatedRequestHook freezes authorization after a carpool user key is authenticated.
func (m *Module) AuthenticatedRequestHook(ctx context.Context, request *http.Request, result *sdkaccess.Result) *sdkaccess.AuthError {
	if m == nil || m.httpAPI == nil {
		return sdkaccess.NewInternalAuthError("Carpool authorization service unavailable", nil)
	}
	return m.httpAPI.AuthenticatedRequestHook(ctx, request, result)
}

// ServerOptions returns composable HTTP integrations owned by the module.
func (m *Module) ServerOptions() []api.ServerOption {
	if m == nil || m.httpAPI == nil || m.writer == nil {
		return nil
	}
	return []api.ServerOption{
		api.WithMiddleware(m.httpAPI.ProxyCredentialGuard()),
		api.WithMiddleware(m.httpAPI.ScopedModelRequestCompletion(m.writer.HandleRequestCompletion)),
		api.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			m.httpAPI.RegisterRoutes(engine)
		}),
		api.WithRequestCompletionObserver(m.writer.HandleRequestCompletion),
		api.WithNoRouteHandler(m.httpAPI.HandleNoRoute),
	}
}

// Start registers the accounting sink and starts its bounded writer.
func (m *Module) Start() {
	if m == nil || m.writer == nil {
		return
	}
	m.startOnce.Do(func() {
		usage.RegisterNamedPlugin(accountingPluginName, m.writer)
		m.writer.Start()
		if m.retention != nil {
			m.retention.Start()
		}
	})
}

// Close drains accounting, checkpoints WAL, and closes SQLite once.
func (m *Module) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		if m.retention != nil {
			m.closeError = errors.Join(m.closeError, m.retention.Close(ctx))
		}
		if m.writer != nil {
			usage.UnregisterNamedPlugin(accountingPluginName)
			m.closeError = errors.Join(m.closeError, m.writer.Close(ctx))
		}
		if m.store != nil {
			m.closeError = errors.Join(m.closeError, m.store.Checkpoint(ctx))
			m.closeError = errors.Join(m.closeError, m.store.Close())
		}
	})
	return m.closeError
}
