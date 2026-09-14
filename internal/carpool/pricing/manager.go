package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	DefaultCatalogURL       = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	DefaultRefreshInterval  = 24 * time.Hour
	maximumCatalogBodyBytes = 8 << 20
)

// CatalogRecord is the durable representation of one validated catalog.
type CatalogRecord struct {
	Hash     string
	Source   string
	Contents []byte
	LoadedAt time.Time
}

// CatalogPersistence stores the last fully validated catalog.
type CatalogPersistence interface {
	LoadPricingCatalog(context.Context) (CatalogRecord, error)
	SavePricingCatalog(context.Context, CatalogRecord) error
}

// CatalogStatus is safe to expose in the administrator API.
type CatalogStatus struct {
	Available     bool
	Hash          string
	Source        string
	CatalogURL    string
	LoadedAt      time.Time
	LastCheckedAt time.Time
	LastSuccessAt time.Time
	FailureReason string
}

// ManagerConfig controls catalog loading and periodic refresh.
type ManagerConfig struct {
	Initial     *Catalog
	URL         string
	Persistence CatalogPersistence
	Client      *http.Client
	Now         func() time.Time
	Interval    time.Duration
}

// Manager keeps an immutable current catalog and atomically swaps validated updates.
type Manager struct {
	mu          sync.RWMutex
	current     *Catalog
	status      CatalogStatus
	url         string
	persistence CatalogPersistence
	client      *http.Client
	now         func() time.Time
	interval    time.Duration
	cancel      context.CancelFunc
	done        chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

// NewManager loads a durable snapshot when available and otherwise uses Initial.
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if cfg.Initial == nil {
		return nil, errors.New("pricing manager: initial catalog is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultRefreshInterval
	}
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	catalogURL := strings.TrimSpace(cfg.URL)
	if catalogURL == "" {
		catalogURL = DefaultCatalogURL
	}
	if errValidate := validateCatalogURL(catalogURL); errValidate != nil {
		return nil, errValidate
	}
	initial := cloneCatalog(cfg.Initial)
	loadedAt := parseCatalogTime(initial.LoadedAt)
	if loadedAt.IsZero() {
		loadedAt = cfg.Now().UTC()
		initial.LoadedAt = loadedAt.Format(time.RFC3339)
	}
	manager := &Manager{
		current:     initial,
		url:         catalogURL,
		persistence: cfg.Persistence,
		client:      cfg.Client,
		now:         cfg.Now,
		interval:    cfg.Interval,
		status:      CatalogStatus{Available: true, Hash: initial.Hash, Source: initial.Source, CatalogURL: publicCatalogURL(catalogURL), LoadedAt: loadedAt, LastSuccessAt: loadedAt},
	}
	if cfg.Persistence == nil {
		return manager, nil
	}
	record, errLoad := cfg.Persistence.LoadPricingCatalog(ctx)
	if errLoad != nil {
		return manager, nil
	}
	cached, errParse := ParseCatalog(record.Contents, record.Source)
	if errParse != nil {
		return manager, nil
	}
	if record.Hash != "" && cached.Hash != record.Hash {
		return manager, nil
	}
	if !record.LoadedAt.IsZero() {
		cached.LoadedAt = record.LoadedAt.UTC().Format(time.RFC3339)
	}
	manager.current = cached
	manager.status.Hash = cached.Hash
	manager.status.Source = cached.Source
	manager.status.LoadedAt = parseCatalogTime(cached.LoadedAt)
	manager.status.LastSuccessAt = manager.status.LoadedAt
	return manager, nil
}

// Current returns the current immutable catalog snapshot.
func (m *Manager) Current() *Catalog {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Status returns a copy of the current refresh state.
func (m *Manager) Status() CatalogStatus {
	if m == nil {
		return CatalogStatus{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Refresh downloads, validates, persists, and atomically activates one catalog.
func (m *Manager) Refresh(ctx context.Context) error {
	if m == nil {
		return errors.New("pricing manager: nil manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkedAt := m.now().UTC()
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if errRequest != nil {
		return m.refreshFailure(checkedAt, errRequest)
	}
	response, errDo := m.client.Do(request)
	if errDo != nil {
		return m.refreshFailure(checkedAt, errDo)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return m.refreshFailure(checkedAt, fmt.Errorf("pricing catalog: remote status %s", response.Status))
	}
	raw, errRead := io.ReadAll(io.LimitReader(response.Body, maximumCatalogBodyBytes+1))
	if errRead != nil {
		return m.refreshFailure(checkedAt, errRead)
	}
	if len(raw) > maximumCatalogBodyBytes {
		return m.refreshFailure(checkedAt, errors.New("pricing catalog: response is too large"))
	}
	if !json.Valid(raw) {
		return m.refreshFailure(checkedAt, errors.New("pricing catalog: response is not valid JSON"))
	}
	catalog, errParse := ParseCatalog(raw, m.url)
	if errParse != nil {
		return m.refreshFailure(checkedAt, errParse)
	}
	catalog.LoadedAt = checkedAt.Format(time.RFC3339)
	if m.persistence != nil {
		if errSave := m.persistence.SavePricingCatalog(ctx, CatalogRecord{Hash: catalog.Hash, Source: catalog.Source, Contents: raw, LoadedAt: checkedAt}); errSave != nil {
			return m.refreshFailure(checkedAt, fmt.Errorf("pricing catalog: persist validated snapshot: %w", errSave))
		}
	}
	m.mu.Lock()
	m.current = catalog
	m.status.Available = true
	m.status.Hash = catalog.Hash
	m.status.Source = catalog.Source
	m.status.LoadedAt = checkedAt
	m.status.LastCheckedAt = checkedAt
	m.status.LastSuccessAt = checkedAt
	m.status.FailureReason = ""
	m.mu.Unlock()
	return nil
}

func (m *Manager) refreshFailure(checkedAt time.Time, cause error) error {
	m.mu.Lock()
	m.status.LastCheckedAt = checkedAt
	m.status.FailureReason = cause.Error()
	m.mu.Unlock()
	return cause
}

// Start begins the daily refresh loop. It intentionally waits for the first interval.
func (m *Manager) Start(parent context.Context) {
	if m == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	m.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		m.mu.Lock()
		m.cancel = cancel
		m.done = make(chan struct{})
		done := m.done
		m.mu.Unlock()
		go func() {
			defer close(done)
			ticker := time.NewTicker(m.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if errRefresh := m.Refresh(ctx); errRefresh != nil {
						continue
					}
				}
			}
		}()
	})
}

// Close stops a started refresh loop.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.RLock()
		cancel, done := m.cancel, m.done
		m.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			go func() { <-done }()
		}
	})
	m.mu.RLock()
	done := m.done
	m.mu.RUnlock()
	if done == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateCatalogURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("pricing manager: catalog URL must be an http(s) URL without userinfo")
	}
	return nil
}

// publicCatalogURL removes credentials and query material before a URL is
// returned through the administrator API.
func publicCatalogURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func parseCatalogTime(raw string) time.Time {
	value, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return value.UTC()
}

func cloneCatalog(catalog *Catalog) *Catalog {
	if catalog == nil {
		return nil
	}
	copyCatalog := *catalog
	copyCatalog.Raw = append([]byte(nil), catalog.Raw...)
	copyCatalog.Models = make(map[string]ModelPrice, len(catalog.Models))
	for name, model := range catalog.Models {
		copyCatalog.Models[name] = model.clone()
	}
	return &copyCatalog
}
