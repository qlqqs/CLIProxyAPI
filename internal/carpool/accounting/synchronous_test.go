package accounting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/pricing"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type retryRepository struct {
	writerRepository
	lock      sync.Mutex
	fail      bool
	entered   chan struct{}
	release   chan struct{}
	calls     int
	costs     []int64
	succeeded []string
}

func (r *retryRepository) RecordUsageEventBilled(ctx context.Context, event domain.UsageEvent, _ string, cost *int64, _, _ string) (domain.UsageEvent, error) {
	if r.entered != nil {
		r.entered <- struct{}{}
		<-r.release
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	r.calls++
	if ctx.Err() != nil {
		return domain.UsageEvent{}, ctx.Err()
	}
	if r.fail {
		return domain.UsageEvent{}, errors.New("secret-database-error")
	}
	r.succeeded = append(r.succeeded, event.EventID)
	if cost != nil {
		r.costs = append(r.costs, *cost)
	}
	return event, nil
}
func (r *retryRepository) setFail(fail bool) { r.lock.Lock(); r.fail = fail; r.lock.Unlock() }

func TestPublishRetainsFrozenFailureAndCompletionUntilRetry(t *testing.T) {
	catalog, err := pricing.ParseCatalog([]byte(`{"gpt-test":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	repo := &retryRepository{fail: true}
	writer, _ := NewWriter(repo, Config{Catalog: catalog})
	manager := usage.NewManager(1)
	manager.Register(writer)
	ctx, cancel := context.WithCancel(context.Background())
	ctx = usage.WithSynchronousObserver(ctx, writer.ObserveUsage)
	cancel()
	record := usage.Record{EventID: "failed", RequestID: "request", AuthID: "auth", Provider: "openai", Model: "gpt-test", UsageKnown: true, Detail: usage.Detail{InputTokens: 2, TotalTokens: 2}, APIKey: "secret-key", Source: "secret-source", Fail: usage.Failure{Body: "secret-body"}}
	manager.Publish(ctx, record)
	manager.Stop()
	if len(writer.pending) != 1 || repo.calls != 1 {
		t.Fatal("async duplicate retried or lost failure")
	}
	frozenCost := writer.pending[0].cost
	if frozenCost == nil || *frozenCost != 2000 {
		t.Fatalf("prepared cost = %v", frozenCost)
	}
	if strings.Contains(fmt.Sprintf("%#v", writer.pending[0].event), "secret") {
		t.Fatal("retained sensitive record fields")
	}
	writer.catalog = nil
	writer.ObserveUsage(ctx, record)
	if len(writer.pending) != 1 {
		t.Fatal("duplicate pending event retained twice")
	}
	writer.HandleRequestCompletion(ctx, pluginapi.RequestCompletion{RequestID: "request", Outcome: pluginapi.RequestCompletionCanceled})
	if len(repo.completions) != 0 {
		t.Fatal("completion overtook failed usage")
	}
	if err := writer.CheckAdmission(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("gate error = %v", err)
	}
	if err := writer.Close(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) {
		t.Fatalf("Close lost failure: %v", err)
	}
	repo.setFail(false)
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.costs) != 1 || repo.costs[0] != *frozenCost || len(repo.completions) != 1 {
		t.Fatal("retry repriced/lost facts")
	}
	if writer.Snapshot().PendingRecords != 0 {
		t.Fatal("retry did not drain")
	}
}

func TestPublishBlocksUntilWriteAndCloseRespectsCanceledBudget(t *testing.T) {
	repo := &retryRepository{entered: make(chan struct{}, 1), release: make(chan struct{})}
	writer, _ := NewWriter(repo, Config{})
	manager := usage.NewManager(1)
	ctx := usage.WithSynchronousObserver(context.Background(), writer.ObserveUsage)
	published := make(chan struct{})
	go func() {
		manager.Publish(ctx, usage.Record{EventID: "event", RequestID: "request", AuthID: "auth"})
		close(published)
	}()
	<-repo.entered
	select {
	case <-published:
		t.Fatal("Publish returned before DB write")
	default:
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close budget = %v", err)
	}
	close(repo.release)
	<-published
	manager.Stop()
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentPublishAndAdmissionRecoverOnlyAfterPersistence(t *testing.T) {
	repo := &retryRepository{fail: true}
	writer, _ := NewWriter(repo, Config{})
	manager := usage.NewManager(1)
	manager.Register(writer)
	ctx := usage.WithSynchronousObserver(context.Background(), writer.ObserveUsage)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			manager.Publish(ctx, usage.Record{EventID: fmt.Sprint(i), RequestID: "request", AuthID: "auth"})
		}(i)
	}
	group.Wait()
	manager.Stop()
	if writer.Snapshot().PendingRecords != 12 {
		t.Fatal("concurrent pending records lost")
	}
	if err := writer.CheckAdmission(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) {
		t.Fatal("failed gate reopened")
	}
	repo.setFail(false)
	if err := writer.CheckAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.Snapshot().PendingRecords != 0 {
		t.Fatal("gate opened before drain")
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationGateCannotBeClearedByEmptyRetry(t *testing.T) {
	writer, _ := NewWriter(&retryRepository{}, Config{})
	writer.reconciliationRequired = true
	if err := writer.RetryPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := writer.CheckAdmission(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) {
		t.Fatal("runtime reconciliation bypassed")
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type panicCatalogProvider struct{}

func (panicCatalogProvider) Current() *pricing.Catalog { panic("sensitive-panic-detail") }

func TestPublishPreparationPanicCannotReopenAdmission(t *testing.T) {
	writer, err := NewWriter(&retryRepository{}, Config{CatalogProvider: panicCatalogProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	manager := usage.NewManager(1)
	ctx := usage.WithSynchronousObserver(context.Background(), writer.ObserveUsage)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("observer panic was silently swallowed")
			}
		}()
		manager.Publish(ctx, usage.Record{EventID: "panic", RequestID: "request", AuthID: "auth", UsageKnown: true})
	}()
	manager.Stop()
	if errRetry := writer.RetryPending(context.Background()); errRetry != nil {
		t.Fatal(errRetry)
	}
	if errGate := writer.CheckAdmission(context.Background()); !errors.Is(errGate, domain.ErrAccountingUnavailable) {
		t.Fatalf("recovered panic left generation healthy: %v", errGate)
	}
	if !writer.Snapshot().ReconciliationRequired {
		t.Fatal("lost event did not require reconciliation")
	}
	if errClose := writer.Close(context.Background()); errClose != nil {
		t.Fatal(errClose)
	}
}

func TestBoundedPendingWaitsRecoversAndDrainsCanceledPublishers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &retryRepository{fail: true}
		writer, err := NewWriter(repo, Config{QueueSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ctx = usage.WithSynchronousObserver(ctx, writer.ObserveUsage)
		manager := usage.NewManager(1)
		publish := func(id string) { manager.Publish(ctx, usage.Record{EventID: id, RequestID: "request", AuthID: "auth"}) }
		publish("one")
		publish("two")
		cancel()
		var group sync.WaitGroup
		for _, id := range []string{"three", "four", "five"} {
			group.Add(1)
			go func() { defer group.Done(); publish(id) }()
		}
		synctest.Wait()
		if got := writer.Snapshot().PendingRecords; got != 2 {
			t.Fatalf("pending = %d", got)
		}
		writer.billingMu.Lock()
		waiting := writer.waiters
		writer.billingMu.Unlock()
		if waiting != 3 {
			t.Fatalf("waiting = %d", waiting)
		}
		if err := writer.CheckAdmission(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) {
			t.Fatalf("gate = %v", err)
		}
		// The caller's canceled shutdown budget must not discard blocked callbacks.
		canceled, stop := context.WithCancel(context.Background())
		stop()
		if err := writer.Close(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("close = %v", err)
		}
		repo.setFail(false)
		// Signal the one retry driver deterministically rather than waiting on time.
		writer.retryWake <- struct{}{}
		synctest.Wait()
		group.Wait()
		manager.Stop()
		if err := writer.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := writer.Snapshot().PendingRecords; got != 0 {
			t.Fatalf("pending = %d", got)
		}
		repo.lock.Lock()
		calls := len(repo.succeeded)
		repo.lock.Unlock()
		if calls != 5 {
			t.Fatalf("lost writes, calls = %d", calls)
		}
	})
}

func TestNonBillableCompletionsNeverEnterFullBillingCache(t *testing.T) {
	repo := &retryRepository{fail: true}
	writer, err := NewWriter(repo, Config{QueueSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	writer.ObserveUsage(context.Background(), usage.Record{EventID: "event", RequestID: "request", AuthID: "auth"})
	for i := 0; i < 20; i++ {
		writer.HandleNonBillableRequestCompletion(context.Background(), pluginapi.RequestCompletion{RequestID: fmt.Sprint(i), Outcome: pluginapi.RequestCompletionSucceeded})
	}
	if got := writer.Snapshot().PendingRecords; got != 1 {
		t.Fatalf("pending = %d", got)
	}
	repo.setFail(false)
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.Snapshot().PendingRecords != 0 {
		t.Fatal("model completions poisoned cache")
	}
}

func TestAdmissionDoesNotOvertakeWaitingIncurredUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &retryRepository{fail: true}
		writer, err := NewWriter(repo, Config{QueueSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		writer.ObserveUsage(context.Background(), usage.Record{EventID: "first", RequestID: "request", AuthID: "auth"})
		go writer.ObserveUsage(context.Background(), usage.Record{EventID: "waiting", RequestID: "request", AuthID: "auth"})
		synctest.Wait()
		repo.setFail(false)
		// This call drains retained facts while the second callback is still waiting
		// for billingMu; admission must not open in that scheduling window.
		if err := writer.CheckAdmission(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) {
			t.Fatalf("admission overtook waiting usage: %v", err)
		}
		synctest.Wait()
		if err := writer.CheckAdmission(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(repo.succeeded) != 2 {
			t.Fatalf("persisted = %v", repo.succeeded)
		}
	})
}

func TestCloseDoesNotOvertakeLateWaitingUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &retryRepository{}
		writer, err := NewWriter(repo, Config{QueueSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		writer.ObserveUsage(context.Background(), usage.Record{EventID: "late-first", RequestID: "request", AuthID: "auth"})
		go writer.ObserveUsage(context.Background(), usage.Record{EventID: "late-waiting", RequestID: "request", AuthID: "auth"})
		synctest.Wait()
		// The worker has exited. Explicit retry releases space, but cannot declare
		// the store safe to close while a callback still owns an incurred event.
		if err := writer.Close(context.Background()); !errors.Is(err, domain.ErrAccountingUnavailable) {
			t.Fatalf("close overtook late waiting usage: %v", err)
		}
		synctest.Wait()
		if err := writer.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(repo.succeeded) != 2 || writer.Snapshot().PendingRecords != 0 {
			t.Fatalf("late events not drained: %v", repo.succeeded)
		}
	})
}
