package usage

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type usagePluginFunc func(context.Context, Record)

func (f usagePluginFunc) HandleUsage(ctx context.Context, record Record) {
	f(ctx, record)
}

func TestRequestIDContextRoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), " request-1 ")
	if got := RequestIDFromContext(ctx); got != "request-1" {
		t.Fatalf("RequestIDFromContext() = %q, want %q", got, "request-1")
	}
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("RequestIDFromContext(background) = %q, want empty", got)
	}
}

func TestStreamFromContextDefaultsMissingToFalse(t *testing.T) {
	if StreamFromContext(context.Background()) {
		t.Fatalf("StreamFromContext(background) = true, want false")
	}
}

func TestStreamFromContextHonorsExplicitTrue(t *testing.T) {
	ctx := WithStream(context.Background(), true)
	if !StreamFromContext(ctx) {
		t.Fatalf("StreamFromContext(true) = false, want true")
	}
}

func TestRecordStreamField(t *testing.T) {
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
		Stream:   true,
	}
	if !record.Stream {
		t.Fatalf("Record.Stream = false, want true")
	}
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}

func TestManagerStopWaitsForQueuedDelivery(t *testing.T) {
	manager := NewManager(4)
	entered := make(chan struct{})
	release := make(chan struct{})
	var delivered atomic.Int64
	manager.Register(usagePluginFunc(func(context.Context, Record) {
		if delivered.Add(1) == 1 {
			close(entered)
			<-release
		}
	}))
	manager.Start(context.Background())
	manager.Publish(context.Background(), Record{EventID: "event-1"})
	manager.Publish(context.Background(), Record{EventID: "event-2"})
	<-entered

	stopped := make(chan struct{})
	go func() {
		manager.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop() returned before an in-flight delivery completed")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop() did not finish after queued delivery was released")
	}
	if got := delivered.Load(); got != 2 {
		t.Fatalf("delivered records = %d, want 2", got)
	}
}

func TestManagerUnregisterNamedStopsDeliveryAndReusesSlot(t *testing.T) {
	manager := NewManager(2)
	var removedCalls atomic.Int64
	var replacementCalls atomic.Int64
	manager.RegisterNamed("carpool", usagePluginFunc(func(context.Context, Record) {
		removedCalls.Add(1)
	}))
	manager.UnregisterNamed(" carpool ")
	manager.RegisterNamed("replacement", usagePluginFunc(func(context.Context, Record) {
		replacementCalls.Add(1)
	}))

	manager.Publish(context.Background(), Record{EventID: "event-after-unregister"})
	manager.Stop()

	if got := removedCalls.Load(); got != 0 {
		t.Fatalf("removed plugin calls = %d, want 0", got)
	}
	if got := replacementCalls.Load(); got != 1 {
		t.Fatalf("replacement plugin calls = %d, want 1", got)
	}
	if len(manager.plugins) != 1 || manager.named["replacement"] != 0 {
		t.Fatalf("manager registration state = plugins %d, named %#v", len(manager.plugins), manager.named)
	}
}
