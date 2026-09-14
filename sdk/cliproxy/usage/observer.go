package usage

import "context"

type synchronousObserverKey struct{}
type synchronousObservedKey struct{}

// WithSynchronousObserver installs a server-owned request observer. Publish calls
// it before asynchronous delivery, even after cancellation or manager shutdown.
// The observer owns persistence, failure admission policy, and panic safety;
// failures must not be silently recovered as successful accounting.
func WithSynchronousObserver(ctx context.Context, observer func(context.Context, Record)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, synchronousObserverKey{}, observer)
}

// SynchronouslyObserved reports that this delivery already passed its request
// observer. An asynchronous sink can use it to avoid handling the event twice.
func SynchronouslyObserved(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	observed, _ := ctx.Value(synchronousObservedKey{}).(bool)
	return observed
}

func observeSynchronously(ctx context.Context, record Record) context.Context {
	if ctx == nil {
		return ctx
	}
	if observer, ok := ctx.Value(synchronousObserverKey{}).(func(context.Context, Record)); ok {
		observer(ctx, record)
		return context.WithValue(ctx, synchronousObservedKey{}, true)
	}
	return ctx
}
