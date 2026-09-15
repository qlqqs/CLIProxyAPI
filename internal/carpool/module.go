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
	carpoolpricing "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/pricing"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
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
	pricing    *carpoolpricing.Manager
	retention  *retentionCleaner
	startOnce  sync.Once
	closeMu    sync.Mutex
	closed     bool
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
	if recovered, errRecover := store.RecoverInterruptedRequests(ctx, now().UTC()); errRecover != nil {
		return nil, closeStore(fmt.Errorf("carpool: recover interrupted requests: %w", errRecover))
	} else if recovered > 0 {
		log.WithField("reason", "carpool_accounting_possibly_incomplete").WithField("requests", recovered).Warn("interrupted requests may have incomplete accounting; new generation remains available")
	}
	catalog, errCatalog := carpoolpricing.DefaultCatalog()
	if errCatalog != nil {
		return nil, closeStore(fmt.Errorf("carpool: load pricing catalog: %w", errCatalog))
	}
	catalog.LoadedAt = now().UTC().Format(time.RFC3339)
	pricingManager, errPricingManager := carpoolpricing.NewManager(ctx, carpoolpricing.ManagerConfig{
		Initial: catalog, URL: carpoolCfg.Pricing.CatalogURL, Persistence: store, Now: now,
	})
	if errPricingManager != nil {
		return nil, closeStore(fmt.Errorf("carpool: initialize pricing catalog: %w", errPricingManager))
	}
	// Historical interruption is a coverage warning, not a startup admission gate.
	writer, errWriter := accounting.NewWriter(store, accounting.Config{Now: now, CatalogProvider: pricingManager})
	if errWriter != nil {
		return nil, closeStore(errWriter)
	}
	control, errControl := carpoolservice.NewControl(store, authCatalog, carpoolservice.ControlConfig{
		SessionAbsoluteTTL:       absoluteTTL,
		ConcurrencyQueueCapacity: carpoolCfg.ConcurrencyQueueCapacity,
		SessionIdleTTL:           idleTTL,
		ReportLocation:           reportLocation,
		UsageRetention:           time.Duration(carpoolCfg.UsageRetentionDays) * 24 * time.Hour,
		HomeEnabled:              cfg.Home.Enabled,
		Now:                      now,
		PricingProvider:          pricingManager,
		AccountingAdmission:      writer.CheckAdmission,
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
		Pricing:         pricingManager,
	})
	if errAPI != nil {
		return nil, closeStore(errAPI)
	}

	return &Module{
		store:    store,
		control:  control,
		httpAPI:  browserAPI,
		provider: carpoolaccess.NewProvider(store, now),
		writer:   writer,
		pricing:  pricingManager,
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
	if errAuthorize := m.httpAPI.AuthenticatedRequestHook(ctx, request, result); errAuthorize != nil {
		return errAuthorize
	}
	if request != nil {
		if snapshot, ok := carpoolruntime.AuthorizationFromContext(request.Context()); ok {
			// Bind identity to the server snapshot, not caller-supplied record IDs.
			requestCtx := usage.WithSynchronousObserver(request.Context(), func(observeCtx context.Context, record usage.Record) {
				record.RequestID = snapshot.RequestID()
				m.writer.ObserveUsage(observeCtx, record)
			})
			*request = *request.WithContext(requestCtx)
		}
	}
	return nil
}

// ServerOptions returns composable HTTP integrations owned by the module.
func (m *Module) ServerOptions() []api.ServerOption {
	if m == nil || m.httpAPI == nil || m.writer == nil {
		return nil
	}
	return []api.ServerOption{
		api.WithMiddleware(m.httpAPI.ProxyCredentialGuard()),
		api.WithMiddleware(m.httpAPI.ScopedFailedRequestCompletion(m.observeHTTPFallbackCompletion)),
		api.WithMiddleware(m.httpAPI.ScopedModelRequestCompletion(m.observeModelRequestCompletion)),
		api.WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
			m.httpAPI.RegisterRoutes(engine)
		}),
		api.WithRequestCompletionObserver(m.observeRequestCompletion),
		api.WithNoRouteHandler(m.httpAPI.HandleNoRoute),
	}
}

// scopedAccountingPlugin keeps global-key events outside the billing retry queue.
type scopedAccountingPlugin struct {
	writer *accounting.Writer
}

func (p scopedAccountingPlugin) HandleUsage(ctx context.Context, record usage.Record) {
	snapshot, ok := carpoolruntime.AuthorizationFromContext(ctx)
	if !ok {
		return
	}
	record.RequestID = snapshot.RequestID()
	p.writer.HandleUsage(ctx, record)
}

func (m *Module) observeRequestCompletion(ctx context.Context, completion pluginapi.RequestCompletion) {
	snapshot, ok := carpoolruntime.AuthorizationFromContext(ctx)
	if !ok {
		return
	}
	snapshot.CompleteOnce(func() {
		completion.RequestID = snapshot.RequestID()
		m.writer.HandleRequestCompletion(ctx, completion)
		if m.control != nil {
			if errSync := m.control.SyncQuotaWindows(context.WithoutCancel(ctx), snapshot.AuthIDs()...); errSync != nil {
				log.WithError(errSync).Warn("carpool quota period observation failed")
			}
		}
	})
}

func (m *Module) observeHTTPFallbackCompletion(ctx context.Context, completion pluginapi.RequestCompletion) {
	snapshot, ok := carpoolruntime.AuthorizationFromContext(ctx)
	if !ok {
		return
	}
	snapshot.CompleteOnce(func() {
		completion.RequestID = snapshot.RequestID()
		m.writer.HandleHTTPFallbackCompletion(ctx, completion, snapshot.ExecutionMayHaveStarted())
	})
}

func (m *Module) observeModelRequestCompletion(ctx context.Context, completion pluginapi.RequestCompletion) {
	snapshot, ok := carpoolruntime.AuthorizationFromContext(ctx)
	if !ok {
		return
	}
	completion.RequestID = snapshot.RequestID()
	m.writer.HandleNonBillableRequestCompletion(ctx, completion)
}

// Start registers the accounting sink and starts its bounded writer.
func (m *Module) Start() {
	if m == nil || m.writer == nil {
		return
	}
	m.startOnce.Do(func() {
		usage.RegisterNamedPlugin(accountingPluginName, scopedAccountingPlugin{writer: m.writer})
		m.writer.Start()
		if m.pricing != nil {
			m.pricing.Start(context.Background())
		}
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
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	if m.closed {
		if m.writer != nil {
			return errors.Join(m.closeError, m.writer.Close(ctx))
		}
		return m.closeError
	}
	if m.control != nil {
		m.control.CloseAdmission()
	}
	var drainError error
	if m.pricing != nil {
		drainError = errors.Join(drainError, m.pricing.Close(ctx))
	}
	if m.retention != nil {
		drainError = errors.Join(drainError, m.retention.Close(ctx))
	}
	if m.writer != nil {
		usage.UnregisterNamedPlugin(accountingPluginName)
		drainError = errors.Join(drainError, m.writer.Close(ctx))
	}
	if drainError != nil {
		// Keep SQLite open for any undrained producer/writer and a later Close.
		return errors.Join(m.closeError, drainError)
	}
	if m.store != nil {
		m.closeError = errors.Join(m.closeError, m.store.Checkpoint(ctx))
		m.closeError = errors.Join(m.closeError, m.store.Close())
	}
	m.closed = true
	return m.closeError
}
