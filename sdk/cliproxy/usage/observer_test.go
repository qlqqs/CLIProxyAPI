package usage

import (
	"context"
	"sync/atomic"
	"testing"
)

type observerTestPlugin func(context.Context, Record)

func (f observerTestPlugin) HandleUsage(ctx context.Context, r Record) { f(ctx, r) }

func TestPublishSynchronousObserverPrecedesAsyncDelivery(t *testing.T) {
	m := NewManager(1)
	var observed atomic.Bool
	var deliveries atomic.Int64
	m.Register(observerTestPlugin(func(ctx context.Context, r Record) {
		if !observed.Load() || !SynchronouslyObserved(ctx) {
			t.Error("async delivery preceded observation")
		}
		deliveries.Add(1)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithSynchronousObserver(ctx, func(ctx context.Context, r Record) {
		if ctx.Err() != context.Canceled {
			t.Error("observer lost original context")
		}
		if r.EventID != "event" {
			t.Error("observer received different record")
		}
		observed.Store(true)
	})
	cancel()
	m.Publish(ctx, Record{EventID: "event"})
	if !observed.Load() {
		t.Fatal("Publish returned before observation")
	}
	m.Stop()
	if deliveries.Load() != 1 {
		t.Fatal("async delivery lost")
	}
	observed.Store(false)
	m.Publish(ctx, Record{EventID: "event"})
	if !observed.Load() {
		t.Fatal("closed manager dropped synchronous observation")
	}
}

func TestPublishWithoutObserverRetainsLegacyDelivery(t *testing.T) {
	m := NewManager(1)
	var deliveries atomic.Int64
	m.Register(observerTestPlugin(func(ctx context.Context, r Record) {
		if SynchronouslyObserved(ctx) {
			t.Error("legacy record marked observed")
		}
		deliveries.Add(1)
	}))
	m.Publish(nil, Record{})
	m.Publish(context.Background(), Record{})
	m.Stop()
	if deliveries.Load() != 2 {
		t.Fatal("legacy deliveries changed")
	}
}

func TestStopDrainsAcceptedSynchronousPublisher(t *testing.T) {
	m := NewManager(1)
	entered, release, published, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var delivered atomic.Bool
	m.Register(observerTestPlugin(func(context.Context, Record) { delivered.Store(true) }))
	ctx := WithSynchronousObserver(context.Background(), func(context.Context, Record) { close(entered); <-release })
	go func() { m.Publish(ctx, Record{}); close(published) }()
	<-entered
	go func() { m.Stop(); close(stopped) }()
	// Explicitly wait for Stop's closed-state transition, not a timer.
	m.mu.Lock()
	for !m.closed {
		m.cond.Wait()
	}
	m.mu.Unlock()
	select {
	case <-stopped:
		t.Fatal("Stop returned with an active observer")
	default:
	}
	close(release)
	<-published
	<-stopped
	if !delivered.Load() {
		t.Fatal("Stop lost accepted publisher's async delivery")
	}
}
