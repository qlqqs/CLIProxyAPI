package carpool

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	log "github.com/sirupsen/logrus"
)

const (
	defaultRetentionCleanupInterval = 24 * time.Hour
	defaultRetentionCleanupBatch    = 1000
	maxRetentionBatchesPerPass      = 64
)

type retentionStore interface {
	CleanupRetention(context.Context, carpoolsqlite.RetentionCleanup) (carpoolsqlite.RetentionCleanupResult, error)
}

type retentionSettingsStore interface {
	GetRetentionSettings(context.Context, int64) (domain.RetentionSettings, error)
}

type retentionCleanerConfig struct {
	UsageRetention time.Duration
	AuditRetention time.Duration
	Interval       time.Duration
	BatchSize      int
	Now            func() time.Time
	NewTicker      func(time.Duration) (<-chan time.Time, func())
}

type retentionCleaner struct {
	store          retentionStore
	usageRetention time.Duration
	auditRetention time.Duration
	interval       time.Duration
	batchSize      int
	now            func() time.Time
	newTicker      func(time.Duration) (<-chan time.Time, func())
	mu             sync.Mutex
	started        bool
	closed         bool
	cancel         context.CancelFunc
	done           chan struct{}
}

func newRetentionCleaner(store retentionStore, cfg retentionCleanerConfig) *retentionCleaner {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultRetentionCleanupInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultRetentionCleanupBatch
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewTicker == nil {
		cfg.NewTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	return &retentionCleaner{
		store:          store,
		usageRetention: cfg.UsageRetention,
		auditRetention: cfg.AuditRetention,
		interval:       cfg.Interval,
		batchSize:      cfg.BatchSize,
		now:            cfg.Now,
		newTicker:      cfg.NewTicker,
	}
}

func (c *retentionCleaner) Start() {
	if c == nil || c.store == nil {
		return
	}
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ticks, stopTicker := c.newTicker(c.interval)
	c.started = true
	c.cancel = cancel
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()

	go func() {
		defer close(done)
		defer stopTicker()
		c.run(ctx, ticks)
	}()
}

func (c *retentionCleaner) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.closed = true
	cancel := c.cancel
	done := c.done
	c.mu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *retentionCleaner) run(ctx context.Context, ticks <-chan time.Time) {
	c.cleanupPass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.cleanupPass(ctx)
		}
	}
}

func (c *retentionCleaner) cleanupPass(ctx context.Context) {
	now := c.now().UTC()
	usageRetention := c.usageRetention
	if configured, ok := c.store.(retentionSettingsStore); ok {
		if settings, errSettings := configured.GetRetentionSettings(ctx, int64(c.usageRetention/(24*time.Hour))); errSettings == nil {
			if settings.EffectiveDays == 0 {
				usageRetention = now.Sub(time.Unix(0, 1))
			} else {
				usageRetention = time.Duration(settings.EffectiveDays) * 24 * time.Hour
			}
		}
	}
	cleanup := carpoolsqlite.RetentionCleanup{
		UsageCutoff: now.Add(-usageRetention),
		AuditCutoff: now.Add(-c.auditRetention),
		BatchSize:   c.batchSize,
	}
	for batch := 0; batch < maxRetentionBatchesPerPass; batch++ {
		result, errCleanup := c.store.CleanupRetention(ctx, cleanup)
		if errCleanup != nil {
			if ctx.Err() == nil {
				log.WithError(errCleanup).Warn("carpool retention cleanup failed")
			}
			return
		}
		deleted := result.UsageEventsDeleted + result.ProxyRequestsDeleted + result.AuditEventsDeleted
		if deleted > 0 {
			log.WithFields(log.Fields{
				"usage_events_deleted":   result.UsageEventsDeleted,
				"proxy_requests_deleted": result.ProxyRequestsDeleted,
				"audit_events_deleted":   result.AuditEventsDeleted,
			}).Debug("carpool retention cleanup batch completed")
		}
		if result.UsageEventsDeleted < int64(c.batchSize) &&
			result.ProxyRequestsDeleted < int64(c.batchSize) &&
			result.AuditEventsDeleted < int64(c.batchSize) {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	log.WithField("batch_limit", maxRetentionBatchesPerPass).Warn("carpool retention cleanup reached batch limit")
}
