package runtime

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrConcurrencyQueueFull = errors.New("carpool: concurrency queue is full")
	ErrConcurrencyClosed    = errors.New("carpool: concurrency limiter is closed")
)

// ConcurrencyLimiter admits logical requests against shared user and account limits.
// Construct it with NewConcurrencyLimiter; its zero value is not usable.
type ConcurrencyLimiter struct {
	mu            sync.Mutex
	capacity      int
	closed        bool
	userLimits    map[string]int
	accountLimits map[string]int
	userActive    map[string]int
	accountActive map[string]int
	active        int
	queue         []*concurrencyWaiter
}

type concurrencyWaiter struct {
	ctx    context.Context
	userID string
	authID string
	done   chan struct{}
	permit *ConcurrencyPermit
	err    error
}

// ConcurrencyPermit owns both slots until Release, including during retries and streams.
// A permit must not be copied after first use.
type ConcurrencyPermit struct {
	once    sync.Once
	limiter *ConcurrencyLimiter
	userID  string
	authID  string
}

// ConcurrencySnapshot is a detached view of counts and limits, without identities.
// Total counts and account counts are for internal or administrator use only.
type ConcurrencySnapshot struct {
	UserActive    int
	AccountActive int
	UserQueued    int
	AccountQueued int
	TotalActive   int
	QueueLength   int
	QueueCapacity int
	UserLimit     *int
	AccountLimit  *int
	Closed        bool
}

// NewConcurrencyLimiter creates one module-wide waiting queue, excluding active requests.
func NewConcurrencyLimiter(capacity int) (*ConcurrencyLimiter, error) {
	if capacity <= 0 {
		return nil, errors.New("carpool: concurrency queue capacity must be positive")
	}
	return &ConcurrencyLimiter{
		capacity:      capacity,
		userLimits:    make(map[string]int),
		accountLimits: make(map[string]int),
		userActive:    make(map[string]int),
		accountActive: make(map[string]int),
	}, nil
}

// SetUserLimit copies a positive limit or removes the limit when nil.
// Invalid nonpositive values are ignored; callers must validate persisted policies.
// Lowering a limit does not revoke active permits.
func (l *ConcurrencyLimiter) SetUserLimit(userID string, limit *int) {
	l.setLimit(l.userLimits, userID, limit)
}

// SetAccountLimit has the same semantics as SetUserLimit, keyed by stable AuthID.
// An empty AuthID is reserved for user-only admission and is not a shared account.
func (l *ConcurrencyLimiter) SetAccountLimit(authID string, limit *int) {
	l.setLimit(l.accountLimits, authID, limit)
}

func (l *ConcurrencyLimiter) setLimit(limits map[string]int, id string, limit *int) {
	if id == "" || (limit != nil && *limit <= 0) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if limit == nil {
		delete(limits, id)
	} else {
		limits[id] = *limit
	}
	l.dispatchLocked()
}

// Acquire obtains both slots atomically or waits without holding either slot.
// The context controls admission only: successful callers must explicitly Release,
// even after cancellation. An empty AuthID requests only a user slot.
func (l *ConcurrencyLimiter) Acquire(ctx context.Context, userID, authID string) (*ConcurrencyPermit, error) {
	if ctx == nil || userID == "" {
		return nil, errors.New("carpool: concurrency admission requires context and user ID")
	}
	l.mu.Lock()
	if errContext := ctx.Err(); errContext != nil {
		l.mu.Unlock()
		return nil, errContext
	}
	if l.closed {
		l.mu.Unlock()
		return nil, ErrConcurrencyClosed
	}
	// Older eligible waiters take priority, even when a new arrival can run now.
	l.dispatchLocked()
	if l.availableLocked(userID, authID) {
		permit := l.grantLocked(userID, authID)
		l.mu.Unlock()
		if errContext := ctx.Err(); errContext != nil {
			permit.Release()
			return nil, errContext
		}
		return permit, nil
	}
	if len(l.queue) >= l.capacity {
		l.mu.Unlock()
		return nil, ErrConcurrencyQueueFull
	}
	waiter := &concurrencyWaiter{ctx: ctx, userID: userID, authID: authID, done: make(chan struct{})}
	l.queue = append(l.queue, waiter)
	l.mu.Unlock()

	select {
	case <-waiter.done:
	case <-ctx.Done():
	}

	l.mu.Lock()
	if errContext := ctx.Err(); errContext != nil {
		// A concurrent grant may already own slots. Release them after unlocking;
		// otherwise remove the waiter before returning the cancellation.
		if waiter.permit == nil && waiter.err == nil {
			for i, queued := range l.queue {
				if queued == waiter {
					copy(l.queue[i:], l.queue[i+1:])
					l.queue[len(l.queue)-1] = nil
					l.queue = l.queue[:len(l.queue)-1]
					break
				}
			}
			l.dispatchLocked()
		}
		permit := waiter.permit
		l.mu.Unlock()
		permit.Release()
		return nil, errContext
	}
	permit, errWait := waiter.permit, waiter.err
	l.mu.Unlock()
	return permit, errWait
}

func (l *ConcurrencyLimiter) availableLocked(userID, authID string) bool {
	if limit, exists := l.userLimits[userID]; exists && l.userActive[userID] >= limit {
		return false
	}
	if authID != "" {
		if limit, exists := l.accountLimits[authID]; exists && l.accountActive[authID] >= limit {
			return false
		}
	}
	return true
}

func (l *ConcurrencyLimiter) grantLocked(userID, authID string) *ConcurrencyPermit {
	l.userActive[userID]++
	if authID != "" {
		l.accountActive[authID]++
	}
	l.active++
	return &ConcurrencyPermit{limiter: l, userID: userID, authID: authID}
}

// dispatchLocked scans in arrival order, skipping waiters blocked on either dimension.
func (l *ConcurrencyLimiter) dispatchLocked() {
	if l.closed {
		return
	}
	kept := 0
	for _, waiter := range l.queue {
		if errContext := waiter.ctx.Err(); errContext != nil {
			waiter.err = errContext
			close(waiter.done)
		} else if l.availableLocked(waiter.userID, waiter.authID) {
			waiter.permit = l.grantLocked(waiter.userID, waiter.authID)
			close(waiter.done)
		} else {
			l.queue[kept] = waiter
			kept++
		}
	}
	clear(l.queue[kept:])
	l.queue = l.queue[:kept]
}

// Release returns both slots exactly once. It is safe on a nil permit.
func (p *ConcurrencyPermit) Release() {
	if p == nil || p.limiter == nil {
		return
	}
	p.once.Do(func() {
		l := p.limiter
		l.mu.Lock()
		defer l.mu.Unlock()
		decrementConcurrencyCount(l.userActive, p.userID)
		if p.authID != "" {
			decrementConcurrencyCount(l.accountActive, p.authID)
		}
		l.active--
		l.dispatchLocked()
	})
}

func decrementConcurrencyCount(counts map[string]int, id string) {
	if counts[id] <= 1 {
		delete(counts, id)
	} else {
		counts[id]--
	}
}

// Close wakes queued requests without revoking permits already granted.
func (l *ConcurrencyLimiter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for _, waiter := range l.queue {
		waiter.err = ErrConcurrencyClosed
		close(waiter.done)
	}
	l.queue = nil
}

// Snapshot returns copied policy values and counts at one point in time.
func (l *ConcurrencyLimiter) Snapshot(userID, authID string) ConcurrencySnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := ConcurrencySnapshot{
		UserActive:    l.userActive[userID],
		AccountActive: l.accountActive[authID],
		TotalActive:   l.active,
		QueueLength:   len(l.queue),
		QueueCapacity: l.capacity,
		Closed:        l.closed,
	}
	if limit, exists := l.userLimits[userID]; exists {
		result.UserLimit = &limit
	}
	if limit, exists := l.accountLimits[authID]; exists {
		result.AccountLimit = &limit
	}
	for _, waiter := range l.queue {
		if waiter.userID == userID {
			result.UserQueued++
		}
		if authID != "" && waiter.authID == authID {
			result.AccountQueued++
		}
	}
	return result
}
