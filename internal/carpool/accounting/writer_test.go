package accounting

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type billedWriterRepository struct {
	writerRepository
	muBilled sync.Mutex
	billed   []domain.UsageEvent
}

func (r *billedWriterRepository) RecordUsageEventBilled(_ context.Context, event domain.UsageEvent, _ string, _ *int64, status, reason string) (domain.UsageEvent, error) {
	event.PricingStatus, event.PricingReason = status, reason
	r.muBilled.Lock()
	r.billed = append(r.billed, event)
	r.muBilled.Unlock()
	return event, nil
}

type writerRepository struct {
	mu          sync.Mutex
	usage       []domain.UsageEvent
	completions []domain.RequestCompletion
	block       chan struct{}
}

func (r *writerRepository) CompleteProxyRequest(_ context.Context, completion domain.RequestCompletion) error {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.completions = append(r.completions, completion)
	r.mu.Unlock()
	return nil
}

func (r *writerRepository) InsertUsageEvent(_ context.Context, event domain.UsageEvent) (domain.UsageEvent, error) {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.usage = append(r.usage, event)
	r.mu.Unlock()
	return event, nil
}

func TestWriterPersistsKnownAndUnknownUsageWithoutSensitiveFields(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := &writerRepository{}
	writer, errWriter := NewWriter(repository, Config{Now: func() time.Time { return now }})
	if errWriter != nil {
		t.Fatalf("NewWriter() error = %v", errWriter)
	}
	writer.Start()
	writer.HandleUsage(context.Background(), usage.Record{
		EventID: "event-known", RequestID: "request-1", AuthID: "auth-1", Provider: "codex",
		Model: "gpt-test", UsageKnown: true, Detail: usage.Detail{InputTokens: 0, OutputTokens: 2, TotalTokens: 2},
		Fail:            usage.Failure{StatusCode: 200, Body: "must-not-be-persisted"},
		ResponseHeaders: map[string][]string{"Authorization": {"must-not-be-persisted"}},
	})
	writer.HandleUsage(context.Background(), usage.Record{
		EventID: "event-unknown", RequestID: "request-1", AuthID: "auth-1", Provider: "codex", UsageKnown: false,
	})
	if errClose := writer.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	if len(repository.usage) != 2 {
		t.Fatalf("usage event count = %d", len(repository.usage))
	}
	known := repository.usage[0]
	if known.InputTokens == nil || *known.InputTokens != 0 || known.TotalTokens == nil || *known.TotalTokens != 2 || known.StatusClass != "2xx" {
		t.Fatalf("known usage = %#v", known)
	}
	unknown := repository.usage[1]
	if unknown.InputTokens != nil || unknown.TotalTokens != nil || unknown.UsageKnown {
		t.Fatalf("unknown usage = %#v", unknown)
	}
}

func TestWriterMapsCompletionAndDrainsOnClose(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repository := &writerRepository{}
	writer, _ := NewWriter(repository, Config{QueueSize: 2, Now: func() time.Time { return now }})
	writer.Start()
	writer.HandleRequestCompletion(context.Background(), pluginapi.RequestCompletion{
		RequestID: "request-1", RequestedModel: "gpt-requested", Stream: true,
		Outcome: pluginapi.RequestCompletionCanceled, StatusCode: 499,
	})
	if errClose := writer.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if len(repository.completions) != 1 {
		t.Fatalf("completion count = %d", len(repository.completions))
	}
	completion := repository.completions[0]
	if completion.RequestedModel != "gpt-requested" || !completion.Stream || completion.Outcome != domain.RequestOutcomeCanceled || completion.StatusClass != "4xx" || completion.ReasonCode != "client_canceled" || !completion.UpstreamAttempted {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestWriterQueueSaturationDoesNotBlock(t *testing.T) {
	repository := &writerRepository{block: make(chan struct{})}
	writer, _ := NewWriter(repository, Config{QueueSize: 1})
	writer.Start()
	writer.HandleUsage(context.Background(), usage.Record{RequestID: "request-1", AuthID: "auth-1"})
	writer.HandleUsage(context.Background(), usage.Record{RequestID: "request-2", AuthID: "auth-2"})
	writer.HandleUsage(context.Background(), usage.Record{RequestID: "request-3", AuthID: "auth-3"})
	if writer.Snapshot().DroppedUsage == 0 {
		t.Fatal("queue saturation did not increment dropped usage")
	}
	close(repository.block)
	if errClose := writer.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
}

func TestWriterPersistsBillingSynchronouslyOutsideQueue(t *testing.T) {
	repository := &billedWriterRepository{}
	writer, errWriter := NewWriter(repository, Config{QueueSize: 1})
	if errWriter != nil {
		t.Fatalf("NewWriter() error = %v", errWriter)
	}
	for index := 0; index < 3; index++ {
		writer.HandleUsage(context.Background(), usage.Record{
			EventID: "billed-event-" + string(rune('a'+index)), RequestID: "request-billed", AuthID: "auth-billed", UsageKnown: false,
		})
	}
	if got := writer.Snapshot().DroppedUsage; got != 0 {
		t.Fatalf("dropped billed usage = %d, want zero", got)
	}
	repository.muBilled.Lock()
	defer repository.muBilled.Unlock()
	if len(repository.billed) != 3 {
		t.Fatalf("billed usage count = %d, want 3", len(repository.billed))
	}
}
