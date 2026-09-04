package carpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
)

type retentionStoreCall struct {
	cleanup carpoolsqlite.RetentionCleanup
	result  carpoolsqlite.RetentionCleanupResult
	err     error
}

type retentionStoreStub struct {
	mu      sync.Mutex
	results []carpoolsqlite.RetentionCleanupResult
	calls   chan retentionStoreCall
	block   bool
}

func (s *retentionStoreStub) CleanupRetention(ctx context.Context, cleanup carpoolsqlite.RetentionCleanup) (carpoolsqlite.RetentionCleanupResult, error) {
	if s.block {
		<-ctx.Done()
		return carpoolsqlite.RetentionCleanupResult{}, ctx.Err()
	}
	s.mu.Lock()
	result := carpoolsqlite.RetentionCleanupResult{}
	if len(s.results) > 0 {
		result = s.results[0]
		s.results = s.results[1:]
	}
	s.mu.Unlock()
	if s.calls != nil {
		s.calls <- retentionStoreCall{cleanup: cleanup, result: result}
	}
	return result, nil
}

func TestRetentionCleanerUsesConfiguredCutoffsAndDrainsBatches(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))
	store := &retentionStoreStub{results: []carpoolsqlite.RetentionCleanupResult{
		{UsageEventsDeleted: 2},
		{ProxyRequestsDeleted: 1},
	}}
	cleaner := newRetentionCleaner(store, retentionCleanerConfig{
		UsageRetention: 90 * 24 * time.Hour,
		AuditRetention: 180 * 24 * time.Hour,
		BatchSize:      2,
		Now:            func() time.Time { return now },
	})

	cleaner.cleanupPass(context.Background())

	store.mu.Lock()
	remaining := len(store.results)
	store.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("remaining cleanup results = %d, want 0", remaining)
	}
}

func TestRetentionCleanerStartsImmediatelyAndStopsTicker(t *testing.T) {
	store := &retentionStoreStub{calls: make(chan retentionStoreCall, 2)}
	ticks := make(chan time.Time, 1)
	stopped := make(chan struct{})
	cleaner := newRetentionCleaner(store, retentionCleanerConfig{
		UsageRetention: time.Hour,
		AuditRetention: 2 * time.Hour,
		Interval:       time.Minute,
		BatchSize:      10,
		Now: func() time.Time {
			return time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
		},
		NewTicker: func(interval time.Duration) (<-chan time.Time, func()) {
			if interval != time.Minute {
				t.Fatalf("ticker interval = %s, want %s", interval, time.Minute)
			}
			return ticks, func() { close(stopped) }
		},
	})
	cleaner.Start()

	first := receiveRetentionCall(t, store.calls)
	if got, want := first.cleanup.UsageCutoff, time.Date(2026, time.September, 4, 11, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("usage cutoff = %s, want %s", got, want)
	}
	if got, want := first.cleanup.AuditCutoff, time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("audit cutoff = %s, want %s", got, want)
	}
	ticks <- time.Now()
	_ = receiveRetentionCall(t, store.calls)

	if errClose := cleaner.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("ticker was not stopped")
	}
	cleaner.Start()
	select {
	case call := <-store.calls:
		t.Fatalf("closed cleaner restarted with call %#v", call)
	default:
	}
}

func TestRetentionCleanerCloseCancelsActiveCleanup(t *testing.T) {
	store := &retentionStoreStub{block: true}
	cleaner := newRetentionCleaner(store, retentionCleanerConfig{
		UsageRetention: time.Hour,
		AuditRetention: time.Hour,
		NewTicker: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
	})
	cleaner.Start()
	if errClose := cleaner.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if errClose := (&retentionCleaner{cancel: func() {}, done: make(chan struct{})}).Close(canceled); !errors.Is(errClose, context.Canceled) {
		t.Fatalf("Close(canceled) error = %v, want context.Canceled", errClose)
	}
}

func receiveRetentionCall(t *testing.T, calls <-chan retentionStoreCall) retentionStoreCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for retention cleanup")
		return retentionStoreCall{}
	}
}
