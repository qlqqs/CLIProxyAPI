// Package accounting persists carpool request completion and usage facts.
package accounting

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
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

// Config controls the bounded persistence queue.
type Config struct {
	QueueSize int
	Now       func() time.Time
}

// Metrics is a point-in-time writer health snapshot.
type Metrics struct {
	QueueDepth         int
	DroppedUsage       uint64
	DroppedCompletions uint64
	WriteFailures      uint64
}

type queueItem struct {
	usage      *domain.UsageEvent
	completion *domain.RequestCompletion
}

// Writer implements the shared usage plugin and request-completion observer.
type Writer struct {
	repository Repository
	now        func() time.Time
	queue      chan queueItem
	done       chan struct{}
	startOnce  sync.Once
	closeOnce  sync.Once
	mu         sync.RWMutex
	closed     bool

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
		repository: repository,
		now:        now,
		queue:      make(chan queueItem, queueSize),
		done:       make(chan struct{}),
	}, nil
}

// Start launches the single database writer. Repeated calls are harmless.
func (w *Writer) Start() {
	if w == nil {
		return
	}
	w.startOnce.Do(func() { go w.run() })
}

// HandleUsage implements usage.Plugin without blocking the caller.
func (w *Writer) HandleUsage(_ context.Context, record usage.Record) {
	if w == nil || record.RequestID == "" || record.AuthID == "" {
		return
	}
	event := usageEvent(record, w.now().UTC())
	if !w.enqueue(queueItem{usage: &event}) {
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
	if !w.enqueue(queueItem{completion: &mapped}) {
		w.droppedCompletions.Add(1)
	}
}

// Close stops accepting records and waits for queued writes to finish.
func (w *Writer) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.Start()
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.queue)
		w.mu.Unlock()
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("carpool accounting: drain writer: %w", ctx.Err())
	}
}

// Snapshot reports bounded writer health without exposing request data.
func (w *Writer) Snapshot() Metrics {
	if w == nil {
		return Metrics{}
	}
	return Metrics{
		QueueDepth:         len(w.queue),
		DroppedUsage:       w.droppedUsage.Load(),
		DroppedCompletions: w.droppedCompletions.Load(),
		WriteFailures:      w.writeFailures.Load(),
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
	for item := range w.queue {
		var errWrite error
		switch {
		case item.usage != nil:
			_, errWrite = w.repository.InsertUsageEvent(context.Background(), *item.usage)
		case item.completion != nil:
			errWrite = w.repository.CompleteProxyRequest(context.Background(), *item.completion)
		}
		if errWrite != nil {
			w.writeFailures.Add(1)
			log.WithError(errWrite).WithField("reason", "carpool_accounting_write_failed").Warn("carpool accounting record was not persisted")
		}
	}
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
		EventID:     eventID,
		RequestID:   record.RequestID,
		AuthID:      record.AuthID,
		Provider:    record.Provider,
		Model:       record.Model,
		UsageKnown:  record.UsageKnown,
		Failed:      record.Failed,
		StatusClass: statusClass(record.Fail.StatusCode),
		RequestedAt: requestedAt,
		RecordedAt:  now,
	}
	if record.UsageKnown {
		event.InputTokens = int64Value(record.Detail.InputTokens)
		event.OutputTokens = int64Value(record.Detail.OutputTokens)
		cached := record.Detail.CachedTokens
		if cached == 0 {
			cached = record.Detail.CacheReadTokens
		}
		event.CachedTokens = int64Value(cached)
		event.ReasoningTokens = int64Value(record.Detail.ReasoningTokens)
		event.TotalTokens = int64Value(record.Detail.TotalTokens)
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
