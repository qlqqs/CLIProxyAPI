// Package accounting persists carpool request completion and usage facts.
package accounting

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/pricing"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const defaultQueueSize = 512

// Repository is the append/update surface required by the accounting writer.
type Repository interface {
	CompleteProxyRequest(context.Context, domain.RequestCompletion) error
	InsertUsageEvent(context.Context, domain.UsageEvent) (domain.UsageEvent, error)
}

type billedRepository interface {
	RecordUsageEventBilled(context.Context, domain.UsageEvent, string, *int64, string, string) (domain.UsageEvent, error)
}

type catalogProvider interface {
	Current() *pricing.Catalog
}

// Config controls the bounded persistence queue.
type Config struct {
	QueueSize       int
	Now             func() time.Time
	Catalog         *pricing.Catalog
	CatalogProvider catalogProvider
}

// Metrics is a point-in-time writer health snapshot.
type Metrics struct {
	QueueDepth             int
	DroppedUsage           uint64
	DroppedCompletions     uint64
	WriteFailures          uint64
	PendingRecords         int
	ReconciliationRequired bool
}

type queueItem struct {
	usage      *domain.UsageEvent
	completion *domain.RequestCompletion
}

// Writer implements the shared usage plugin and request-completion observer.
type Writer struct {
	repository             Repository
	now                    func() time.Time
	catalog                *pricing.Catalog
	provider               catalogProvider
	queue                  chan queueItem
	done                   chan struct{}
	startOnce              sync.Once
	closeOnce              sync.Once
	mu                     sync.RWMutex
	closed                 bool
	closing                atomic.Bool
	billingMu              sync.Mutex
	pending                []preparedRecord
	pendingChanged         chan struct{}
	retryWake              chan struct{}
	waiters                int
	reconciliationRequired bool

	droppedUsage       atomic.Uint64
	droppedCompletions atomic.Uint64
	writeFailures      atomic.Uint64
}

// NewWriter creates a stopped accounting writer.
func NewWriter(repository Repository, cfg Config) (*Writer, error) {
	if repository == nil {
		return nil, fmt.Errorf("carpool accounting: repository is required")
	}
	queueSize := cfg.QueueSize
	if queueSize == 0 {
		queueSize = defaultQueueSize
	}
	if queueSize < 1 {
		return nil, fmt.Errorf("carpool accounting: queue size must be positive")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Writer{
		repository:     repository,
		now:            now,
		catalog:        cfg.Catalog,
		provider:       cfg.CatalogProvider,
		pendingChanged: make(chan struct{}),
		retryWake:      make(chan struct{}, 1),
		queue:          make(chan queueItem, queueSize),
		done:           make(chan struct{}),
	}, nil
}

// Start launches the single database writer. Repeated calls are harmless.
func (w *Writer) Start() {
	if w == nil {
		return
	}
	w.startOnce.Do(func() { go w.run() })
}

// HandleUsage is the asynchronous compatibility sink. Request-scoped observed
// deliveries are skipped, including failed writes retained for retry.
func (w *Writer) HandleUsage(ctx context.Context, record usage.Record) {
	if w == nil || usage.SynchronouslyObserved(ctx) {
		return
	}
	w.mu.RLock()
	closed := w.closed
	w.mu.RUnlock()
	if closed {
		w.droppedUsage.Add(1)
		return
	}
	w.ObserveUsage(ctx, record)
}

// ObserveUsage persists billing synchronously, independent of client cancellation.
// Only the sanitized event and prepared prices are retained on failure.
func (w *Writer) ObserveUsage(ctx context.Context, record usage.Record) {
	if w == nil || record.RequestID == "" || record.AuthID == "" {
		return
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	event := usageEvent(record, w.now().UTC())
	if _, billed := w.repository.(billedRepository); billed {
		w.billingMu.Lock()
		defer w.billingMu.Unlock()
		defer w.requireReconciliationOnPanic()
		// Do not replace an unresolved event's original pricing on redelivery.
		for _, pending := range w.pending {
			if pending.event != nil && pending.event.EventID == event.EventID {
				return
			}
		}
		w.retainLocked(w.prepareUsage(ctx, event))
		if !w.closed {
			_ = w.retryLocked(context.Background())
		}
		return
	}
	if w.closed {
		w.droppedUsage.Add(1)
		return
	}
	select {
	case w.queue <- queueItem{usage: &event}:
	default:
		w.droppedUsage.Add(1)
	}
}

// HandleRequestCompletion accepts one sanitized logical request terminal event.
func (w *Writer) HandleRequestCompletion(_ context.Context, completion pluginapi.RequestCompletion) {
	if w == nil || completion.RequestID == "" {
		return
	}
	mapped, ok := requestCompletion(completion, w.now().UTC())
	if !ok {
		return
	}
	w.handleMappedRequestCompletion(mapped)
}

// HandleHTTPFallbackCompletion records only the HTTP fallback chosen by the
// request owner. Unjoined execution is incomplete, never fabricated as finished.
func (w *Writer) HandleHTTPFallbackCompletion(_ context.Context, completion pluginapi.RequestCompletion, executionPossible bool) {
	if w == nil || completion.RequestID == "" {
		return
	}
	mapped, ok := requestCompletion(completion, w.now().UTC())
	if !ok {
		return
	}
	mapped.UpstreamAttempted = executionPossible
	if executionPossible {
		mapped.Outcome = domain.RequestOutcomeIncomplete
		mapped.ReasonCode = "http_ended_before_completion"
	}
	w.handleMappedRequestCompletion(mapped)
}

func (w *Writer) handleMappedRequestCompletion(mapped domain.RequestCompletion) {
	if _, billed := w.repository.(billedRepository); billed {
		w.mu.RLock()
		defer w.mu.RUnlock()
		w.billingMu.Lock()
		defer w.billingMu.Unlock()
		w.retainLocked(preparedRecord{completion: &mapped})
		// Never mark requests complete ahead of unresolved usage. Their durable
		// in_progress state preserves the possible-incompleteness marker.
		if !w.closed {
			_ = w.retryLocked(context.Background())
		}
		return
	}
	if !w.enqueue(queueItem{completion: &mapped}) {
		w.droppedCompletions.Add(1)
	}
}

// HandleNonBillableRequestCompletion uses the original best-effort queue for
// server-identified model reads, never the billed retry cache or admission gate.
func (w *Writer) HandleNonBillableRequestCompletion(_ context.Context, completion pluginapi.RequestCompletion) {
	if w == nil || completion.RequestID == "" {
		return
	}
	mapped, ok := requestCompletion(completion, w.now().UTC())
	if ok && !w.enqueue(queueItem{completion: &mapped}) {
		w.droppedCompletions.Add(1)
	}
}

// retainLocked bounds retained failures only. Waiting callbacks retain their own
// prepared record and ignore client cancellation because usage already occurred.
// mu remains read-locked so Close cannot close the store beneath a waiting write.
func (w *Writer) retainLocked(item preparedRecord) {
	for len(w.pending) >= cap(w.queue) {
		if !w.closed {
			_ = w.retryLocked(context.Background())
		}
		if len(w.pending) < cap(w.queue) {
			break
		}
		w.Start()
		w.waiters++
		changed := w.pendingChanged
		select {
		case w.retryWake <- struct{}{}:
		default:
		}
		w.billingMu.Unlock()
		<-changed
		w.billingMu.Lock()
		w.waiters--
	}
	w.pending = append(w.pending, item)
}

func (w *Writer) notifyPendingLocked() {
	close(w.pendingChanged)
	w.pendingChanged = make(chan struct{})
}

// retryWaiting is driven by one worker, never one goroutine per failed event.
func (w *Writer) retryWaiting() {
	w.billingMu.Lock()
	defer w.billingMu.Unlock()
	defer func() {
		if recover() != nil {
			w.reconciliationRequired = true
			log.WithField("reason", "carpool_accounting_retry_panicked").Warn("carpool accounting retry interrupted")
		}
	}()
	if w.waiters > 0 {
		_ = w.retryLocked(context.Background())
	}
}

// Close stops accepting records and waits for queued writes to finish.
func (w *Writer) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.Start()
	w.closeOnce.Do(func() {
		w.closing.Store(true)
		go func() {
			w.mu.Lock()
			w.closed = true
			close(w.queue)
			w.mu.Unlock()
		}()
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-w.done:
		return w.RetryPending(ctx)
	case <-ctx.Done():
		return fmt.Errorf("carpool accounting: drain writer: %w", ctx.Err())
	}
}

// Snapshot reports bounded writer health without exposing request data.
func (w *Writer) Snapshot() Metrics {
	if w == nil {
		return Metrics{}
	}
	w.billingMu.Lock()
	defer w.billingMu.Unlock()
	return Metrics{
		PendingRecords:         len(w.pending),
		ReconciliationRequired: w.reconciliationRequired,
		QueueDepth:             len(w.queue),
		DroppedUsage:           w.droppedUsage.Load(),
		DroppedCompletions:     w.droppedCompletions.Load(),
		WriteFailures:          w.writeFailures.Load(),
	}
}

func (w *Writer) enqueue(item queueItem) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return false
	}
	select {
	case w.queue <- item:
		return true
	default:
		return false
	}
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var item queueItem
		select {
		case queued, ok := <-w.queue:
			if !ok {
				return
			}
			item = queued
		case <-ticker.C:
			w.retryWaiting()
			continue
		case <-w.retryWake:
			w.retryWaiting()
			continue
		}
		var errWrite error
		switch {
		case item.usage != nil:
			errWrite = w.writeUsage(context.Background(), *item.usage)
		case item.completion != nil:
			errWrite = w.repository.CompleteProxyRequest(context.Background(), *item.completion)
		}
		if errWrite != nil {
			w.writeFailures.Add(1)
			log.WithField("reason", "carpool_accounting_write_failed").Warn("carpool accounting record was not persisted")
		}
	}
}

// preparedRecord contains only allowlisted accounting data, never the SDK Record.
type preparedRecord struct {
	event      *domain.UsageEvent
	completion *domain.RequestCompletion
	cost       *int64
	status     string
	reason     string
}

// CheckAdmission retries unresolved facts before admitting another generation.
// No elapsed time or unrelated successful write can clear the gate.
func (w *Writer) CheckAdmission(ctx context.Context) error {
	if w.closing.Load() {
		return domain.ErrAccountingUnavailable
	}
	w.billingMu.Lock()
	defer w.billingMu.Unlock()
	if w.closing.Load() || w.reconciliationRequired {
		return domain.ErrAccountingUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := w.retryLocked(ctx); err != nil {
		return err
	}
	if w.waiters > 0 {
		return domain.ErrAccountingUnavailable
	}
	return nil
}

// RetryPending retries the frozen sanitized records in order. It is safe to call
// after a failed Close; unresolved entries remain available for another attempt.
func (w *Writer) RetryPending(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.billingMu.Lock()
	defer w.billingMu.Unlock()
	if err := w.retryLocked(ctx); err != nil {
		return err
	}
	// A closed worker may still have a late callback waiting for cache space.
	// Do not let Close release its store before that callback retains its fact.
	if w.waiters > 0 {
		return domain.ErrAccountingUnavailable
	}
	return nil
}

// requireReconciliationOnPanic runs with billingMu held. An outer HTTP or plugin
// recovery must not turn an interrupted accounting callback into healthy state.
func (w *Writer) requireReconciliationOnPanic() {
	if value := recover(); value != nil {
		w.reconciliationRequired = true
		panic(value)
	}
}

func (w *Writer) retryLocked(ctx context.Context) error {
	defer w.requireReconciliationOnPanic()
	for len(w.pending) > 0 {
		item := w.pending[0]
		var errWrite error
		if item.event != nil {
			_, errWrite = w.repository.(billedRepository).RecordUsageEventBilled(ctx, *item.event, "", item.cost, item.status, item.reason)
		} else {
			errWrite = w.repository.CompleteProxyRequest(ctx, *item.completion)
		}
		if errWrite != nil {
			w.writeFailures.Add(1)
			log.WithField("reason", "carpool_billing_write_failed").WithField("pending_records", len(w.pending)).Warn("carpool accounting unavailable; retry or reconciliation required")
			return fmt.Errorf("carpool accounting: %d records require retry: %w", len(w.pending), domain.ErrAccountingUnavailable)
		}
		w.pending[0] = preparedRecord{}
		w.pending = w.pending[1:]
		w.notifyPendingLocked()
	}
	return nil
}

func (w *Writer) writeUsage(ctx context.Context, event domain.UsageEvent) error {
	_, err := w.repository.InsertUsageEvent(ctx, event)
	return err
}

func (w *Writer) prepareUsage(ctx context.Context, event domain.UsageEvent) preparedRecord {
	var cost *int64
	pricingStatus, pricingReason := "unknown", "usage_unknown"
	if event.UsageKnown {
		pricingStatus, pricingReason = "unpriced", "catalog_unavailable"
		var catalog interface {
			Lookup(string) (pricing.ModelPrice, bool)
		}
		if snapshot, ok := carpoolruntime.AuthorizationFromContext(ctx); ok && snapshot.Prices() != nil {
			catalog = snapshot.Prices()
		} else {
			catalog = w.catalog
			if w.provider != nil {
				catalog = w.provider.Current()
			}
		}
		if catalog != nil {
			if modelPrice, found := catalog.Lookup(event.Model); found {
				event.PriceInputPerToken, event.PriceOutputPerToken, event.PriceCacheRead, event.PriceCacheWrite = priceSnapshot(modelPrice)
				result := pricing.Calculate(modelPrice, eventTokenBreakdown(event), event.ResponseServiceTier)
				if result.Known {
					value := result.NanoUSD
					cost = &value
					pricingStatus, pricingReason = "priced", ""
				} else {
					pricingReason = result.Reason
				}
			} else {
				pricingReason = "model_price_missing"
			}
		}
	}
	return preparedRecord{event: &event, cost: cost, status: pricingStatus, reason: pricingReason}
}

func priceSnapshot(model pricing.ModelPrice) (string, string, string, string) {
	format := func(value *big.Rat) string {
		if value == nil {
			return ""
		}
		return value.FloatString(18)
	}
	return format(model.InputPerToken), format(model.OutputPerToken), format(model.CacheReadPerToken), format(model.CacheWritePerToken)
}

func eventTokenBreakdown(event domain.UsageEvent) usage.TokenBreakdown {
	inputUncached, inputRead, inputWrite, outputNonReasoning, reasoning := int64(0), int64(0), int64(0), int64(0), int64(0)
	if event.UncachedInputTokens != nil {
		inputUncached = *event.UncachedInputTokens
	}
	if event.CacheReadTokens != nil {
		inputRead = *event.CacheReadTokens
	}
	if event.CacheWriteTokens != nil {
		inputWrite = *event.CacheWriteTokens
	}
	if event.NonReasoningTokens != nil {
		outputNonReasoning = *event.NonReasoningTokens
	}
	if event.ReasoningTokens != nil {
		reasoning = *event.ReasoningTokens
	}
	inputTotal := inputUncached + inputRead + inputWrite
	outputTotal := outputNonReasoning + reasoning
	total := inputTotal + outputTotal
	if event.TotalTokens != nil {
		total = *event.TotalTokens
	}
	return usage.TokenBreakdown{SchemaVersion: event.CanonicalSchema, Quality: usage.TokenAccountingQuality(event.CanonicalQuality), TotalTokens: total, Input: usage.TokenInputBreakdown{TotalTokens: inputTotal, UncachedTokens: inputUncached, CacheReadTokens: inputRead, CacheWriteTokens: inputWrite}, Output: usage.TokenOutputBreakdown{TotalTokens: outputTotal, NonReasoningTokens: outputNonReasoning, ReasoningTokens: reasoning}}
}

func usageEvent(record usage.Record, now time.Time) domain.UsageEvent {
	eventID := record.EventID
	if eventID == "" {
		eventID = uuid.NewString()
	}
	requestedAt := record.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = now
	}
	event := domain.UsageEvent{
		EventID:             eventID,
		RequestID:           record.RequestID,
		AuthID:              record.AuthID,
		Provider:            record.Provider,
		Model:               record.Model,
		UsageKnown:          record.UsageKnown,
		Failed:              record.Failed,
		StatusClass:         statusClass(record.Fail.StatusCode),
		RequestedAt:         requestedAt,
		RecordedAt:          now,
		RequestServiceTier:  record.ServiceTier,
		ResponseServiceTier: record.ResponseServiceTier,
	}
	if record.UsageKnown {
		detail := usage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
		breakdown := detail.TokenBreakdown
		event.CanonicalSchema = breakdown.SchemaVersion
		event.CanonicalQuality = string(breakdown.Quality)
		event.UncachedInputTokens = int64Value(breakdown.Input.UncachedTokens)
		event.CacheReadTokens = int64Value(breakdown.Input.CacheReadTokens)
		event.CacheWriteTokens = int64Value(breakdown.Input.CacheWriteTokens)
		event.NonReasoningTokens = int64Value(breakdown.Output.NonReasoningTokens)
		event.InputTokens = int64Value(detail.InputTokens)
		event.OutputTokens = int64Value(detail.OutputTokens)
		cached := detail.CachedTokens
		if cached == 0 {
			cached = detail.CacheReadTokens
		}
		event.CachedTokens = int64Value(cached)
		event.ReasoningTokens = int64Value(detail.ReasoningTokens)
		event.TotalTokens = int64Value(detail.TotalTokens)
	}
	return event
}

func requestCompletion(completion pluginapi.RequestCompletion, now time.Time) (domain.RequestCompletion, bool) {
	outcome := domain.RequestOutcome("")
	switch completion.Outcome {
	case pluginapi.RequestCompletionSucceeded:
		outcome = domain.RequestOutcomeSucceeded
	case pluginapi.RequestCompletionFailed:
		outcome = domain.RequestOutcomeFailed
	case pluginapi.RequestCompletionCanceled:
		outcome = domain.RequestOutcomeCanceled
	case pluginapi.RequestCompletionRejected:
		outcome = domain.RequestOutcomeRejected
	default:
		return domain.RequestCompletion{}, false
	}
	completedAt := completion.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = now
	}
	return domain.RequestCompletion{
		RequestID:         completion.RequestID,
		RequestedModel:    completion.RequestedModel,
		Stream:            completion.Stream,
		Outcome:           outcome,
		CompletedAt:       completedAt,
		StatusClass:       statusClass(completion.StatusCode),
		ReasonCode:        completionReason(outcome, completion.StatusCode),
		UpstreamAttempted: outcome != domain.RequestOutcomeRejected,
	}, true
}

func completionReason(outcome domain.RequestOutcome, status int) string {
	if outcome == domain.RequestOutcomeCanceled {
		return "client_canceled"
	}
	if outcome == domain.RequestOutcomeRejected {
		return "request_rejected"
	}
	if status >= 500 {
		return "upstream_or_internal_error"
	}
	if status >= 400 {
		return "request_error"
	}
	return ""
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return fmt.Sprintf("%dxx", status/100)
}

func int64Value(value int64) *int64 {
	result := value
	return &result
}
