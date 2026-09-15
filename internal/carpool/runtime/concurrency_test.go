package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
)

type concurrencyResult struct {
	permit *ConcurrencyPermit
	err    error
}

func newTestConcurrencyLimiter(t *testing.T, capacity int) *ConcurrencyLimiter {
	t.Helper()
	limiter, errNew := NewConcurrencyLimiter(capacity)
	if errNew != nil {
		t.Fatal(errNew)
	}
	t.Cleanup(limiter.Close)
	return limiter
}

func acquireConcurrency(t *testing.T, limiter *ConcurrencyLimiter, userID, authID string) *ConcurrencyPermit {
	t.Helper()
	permit, errAcquire := limiter.Acquire(context.Background(), userID, authID)
	if errAcquire != nil || permit == nil {
		t.Fatalf("Acquire(%q, %q) = %v, %v", userID, authID, permit, errAcquire)
	}
	t.Cleanup(permit.Release)
	return permit
}

func queueConcurrency(t *testing.T, limiter *ConcurrencyLimiter, ctx context.Context, userID, authID string) <-chan concurrencyResult {
	t.Helper()
	before := limiter.Snapshot(userID, authID).QueueLength
	result := make(chan concurrencyResult, 1)
	go func() {
		permit, errAcquire := limiter.Acquire(ctx, userID, authID)
		result <- concurrencyResult{permit: permit, err: errAcquire}
	}()
	synctest.Wait()
	if got := limiter.Snapshot(userID, authID).QueueLength; got != before+1 {
		t.Fatalf("queue length = %d, want %d", got, before+1)
	}
	return result
}

func receiveConcurrency(t *testing.T, result <-chan concurrencyResult, want error) *ConcurrencyPermit {
	t.Helper()
	synctest.Wait()
	select {
	case got := <-result:
		if !errors.Is(got.err, want) || (got.permit == nil) != (want != nil) {
			t.Fatalf("admission = %v, %v, want error %v", got.permit, got.err, want)
		}
		if got.permit != nil {
			t.Cleanup(got.permit.Release)
		}
		return got.permit
	default:
		t.Fatal("admission did not wake")
		return nil
	}
}

func assertConcurrencyWaiting(t *testing.T, result <-chan concurrencyResult) {
	t.Helper()
	synctest.Wait()
	select {
	case got := <-result:
		t.Fatalf("admission unexpectedly returned: %v, %v", got.permit, got.err)
	default:
	}
}

func assertConcurrencyEmpty(t *testing.T, limiter *ConcurrencyLimiter) {
	t.Helper()
	if got := limiter.Snapshot("", ""); got.TotalActive != 0 || got.QueueLength != 0 {
		t.Fatalf("limiter leaked resources: %+v", got)
	}
}

func TestConcurrencyLimiterValidation(t *testing.T) {
	for _, capacity := range []int{-1, 0} {
		if limiter, errNew := NewConcurrencyLimiter(capacity); limiter != nil || errNew == nil {
			t.Fatalf("NewConcurrencyLimiter(%d) = %v, %v", capacity, limiter, errNew)
		}
	}
	limiter := newTestConcurrencyLimiter(t, 1)
	if permit, errAcquire := limiter.Acquire(nil, "user", "account"); permit != nil || errAcquire == nil {
		t.Fatalf("nil context admission = %v, %v", permit, errAcquire)
	}
	if permit, errAcquire := limiter.Acquire(context.Background(), "", "account"); permit != nil || errAcquire == nil {
		t.Fatalf("empty user admission = %v, %v", permit, errAcquire)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if permit, errAcquire := limiter.Acquire(ctx, "user", "account"); permit != nil || !errors.Is(errAcquire, context.Canceled) {
		t.Fatalf("canceled admission = %v, %v", permit, errAcquire)
	}
	assertConcurrencyEmpty(t, limiter)
}

func TestConcurrencyUnlimitedAndExplicitRelease(t *testing.T) {
	limiter := newTestConcurrencyLimiter(t, 1)
	limiter.SetUserLimit("user", nil)
	limiter.SetAccountLimit("account", nil)
	var permits []*ConcurrencyPermit
	for range 12 {
		permits = append(permits, acquireConcurrency(t, limiter, "user", "account"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	permit, errAcquire := limiter.Acquire(ctx, "user", "")
	if errAcquire != nil {
		t.Fatal(errAcquire)
	}
	permits = append(permits, permit)
	cancel()
	got := limiter.Snapshot("user", "account")
	if got.UserActive != 13 || got.AccountActive != 12 || got.TotalActive != 13 || got.QueueLength != 0 || got.UserLimit != nil || got.AccountLimit != nil {
		t.Fatalf("unlimited/active cancellation snapshot = %+v", got)
	}
	if gotBlank := limiter.Snapshot("user", ""); gotBlank.AccountActive != 0 {
		t.Fatalf("user-only admission fabricated an account: %+v", gotBlank)
	}
	var group sync.WaitGroup
	for _, permit := range permits {
		for range 3 {
			group.Go(permit.Release)
		}
	}
	group.Wait()
	var nilPermit *ConcurrencyPermit
	nilPermit.Release()
	assertConcurrencyEmpty(t, limiter)
	if len(limiter.userActive) != 0 || len(limiter.accountActive) != 0 {
		t.Fatal("idle identities retained active counter entries")
	}
}

func TestConcurrencyQueueCapacityIsGlobalAndExcludesActive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newTestConcurrencyLimiter(t, 10)
		defer limiter.Close()
		one := 1
		limiter.SetAccountLimit("account-a", &one)
		limiter.SetAccountLimit("account-b", &one)
		activeA := acquireConcurrency(t, limiter, "holder-a", "account-a")
		activeB := acquireConcurrency(t, limiter, "holder-b", "account-b")
		var queued []<-chan concurrencyResult
		for i := range 10 {
			authID := "account-a"
			if i%2 == 1 {
				authID = "account-b"
			}
			queued = append(queued, queueConcurrency(t, limiter, context.Background(), fmt.Sprintf("user-%d", i), authID))
		}
		if permit, errAcquire := limiter.Acquire(context.Background(), "eleventh", "account-a"); permit != nil || !errors.Is(errAcquire, ErrConcurrencyQueueFull) {
			t.Fatalf("eleventh waiter = %v, %v", permit, errAcquire)
		}
		// A full queue must not reject an unrelated request that can execute now.
		unrelated := acquireConcurrency(t, limiter, "other", "spare-account")
		got := limiter.Snapshot("user-0", "account-a")
		if got.QueueLength != 10 || got.QueueCapacity != 10 || got.TotalActive != 3 || got.UserQueued != 1 || got.AccountQueued != 5 || got.UserActive != 0 || got.AccountActive != 1 {
			t.Fatalf("global queue snapshot = %+v", got)
		}
		limiter.Close()
		limiter.Close()
		for _, result := range queued {
			receiveConcurrency(t, result, ErrConcurrencyClosed)
		}
		got = limiter.Snapshot("holder-a", "account-a")
		if !got.Closed || got.TotalActive != 3 || got.QueueLength != 0 {
			t.Fatalf("Close revoked active work: %+v", got)
		}
		if permit, errAcquire := limiter.Acquire(context.Background(), "other", "spare"); permit != nil || !errors.Is(errAcquire, ErrConcurrencyClosed) {
			t.Fatalf("closed admission = %v, %v", permit, errAcquire)
		}
		activeA.Release()
		activeB.Release()
		unrelated.Release()
		assertConcurrencyEmpty(t, limiter)
	})
}

func TestConcurrencyUserAndAccountAdmissionIsAtomic(t *testing.T) {
	for _, test := range []struct {
		name      string
		waitUser  string
		waitAuth  string
		spareUser string
		spareAuth string
	}{
		{name: "same user across keys does not reserve account", waitUser: "user-a", waitAuth: "account-b", spareUser: "user-b", spareAuth: "account-b"},
		{name: "same account across users does not reserve user", waitUser: "user-b", waitAuth: "account-a", spareUser: "user-b", spareAuth: "account-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				limiter := newTestConcurrencyLimiter(t, 1)
				defer limiter.Close()
				one := 1
				for _, id := range []string{"user-a", "user-b"} {
					limiter.SetUserLimit(id, &one)
				}
				for _, id := range []string{"account-a", "account-b"} {
					limiter.SetAccountLimit(id, &one)
				}
				active := acquireConcurrency(t, limiter, "user-a", "account-a")
				queued := queueConcurrency(t, limiter, context.Background(), test.waitUser, test.waitAuth)
				spare := acquireConcurrency(t, limiter, test.spareUser, test.spareAuth)
				active.Release()
				assertConcurrencyWaiting(t, queued)
				spare.Release()
				admitted := receiveConcurrency(t, queued, nil)
				got := limiter.Snapshot(test.waitUser, test.waitAuth)
				if got.UserActive != 1 || got.AccountActive != 1 || got.TotalActive != 1 {
					t.Fatalf("atomic grant = %+v", got)
				}
				admitted.Release()
				assertConcurrencyEmpty(t, limiter)
			})
		})
	}
}

func TestConcurrencyFIFOAmongEligibleWithoutHeadBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newTestConcurrencyLimiter(t, 10)
		defer limiter.Close()
		one := 1
		limiter.SetUserLimit("blocked-user", &one)
		limiter.SetAccountLimit("shared", &one)
		userBlocker := acquireConcurrency(t, limiter, "blocked-user", "other-account")
		accountBlocker := acquireConcurrency(t, limiter, "holder", "shared")
		head := queueConcurrency(t, limiter, context.Background(), "blocked-user", "shared")
		second := queueConcurrency(t, limiter, context.Background(), "second", "shared")
		third := queueConcurrency(t, limiter, context.Background(), "third", "shared")
		accountBlocker.Release()
		secondPermit := receiveConcurrency(t, second, nil)
		assertConcurrencyWaiting(t, head)
		assertConcurrencyWaiting(t, third)
		newcomer := queueConcurrency(t, limiter, context.Background(), "newcomer", "shared")
		secondPermit.Release()
		thirdPermit := receiveConcurrency(t, third, nil)
		assertConcurrencyWaiting(t, newcomer)
		userBlocker.Release()
		assertConcurrencyWaiting(t, head)
		thirdPermit.Release()
		headPermit := receiveConcurrency(t, head, nil)
		assertConcurrencyWaiting(t, newcomer)
		headPermit.Release()
		receiveConcurrency(t, newcomer, nil).Release()
		assertConcurrencyEmpty(t, limiter)
	})
}

func TestConcurrencyLimitUpdatesWakeAndDoNotRevoke(t *testing.T) {
	for _, dimension := range []string{"user", "account"} {
		t.Run(dimension, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				limiter := newTestConcurrencyLimiter(t, 10)
				defer limiter.Close()
				setLimit := func(limit *int) { limiter.SetUserLimit("user", limit) }
				if dimension == "account" {
					setLimit = func(limit *int) { limiter.SetAccountLimit("account", limit) }
				}
				one, two := 1, 2
				setLimit(&one)
				active := acquireConcurrency(t, limiter, "user", "account")
				first := queueConcurrency(t, limiter, context.Background(), "user", "account")
				second := queueConcurrency(t, limiter, context.Background(), "user", "account")
				setLimit(&two)
				firstPermit := receiveConcurrency(t, first, nil)
				assertConcurrencyWaiting(t, second)
				setLimit(&one)
				if got := limiter.Snapshot("user", "account"); got.TotalActive != 2 {
					t.Fatalf("lowering revoked active permits: %+v", got)
				}
				active.Release()
				assertConcurrencyWaiting(t, second)
				setLimit(nil)
				secondPermit := receiveConcurrency(t, second, nil)
				firstPermit.Release()
				secondPermit.Release()
				assertConcurrencyEmpty(t, limiter)
			})
		})
	}
}

func TestConcurrencyPoliciesAndSnapshotsAreCopied(t *testing.T) {
	limiter := newTestConcurrencyLimiter(t, 1)
	one := 1
	limiter.SetUserLimit("user", &one)
	limiter.SetAccountLimit("account", &one)
	limiter.SetAccountLimit("", &one)
	one = 9
	got := limiter.Snapshot("user", "account")
	if got.UserLimit == nil || *got.UserLimit != 1 || got.AccountLimit == nil || *got.AccountLimit != 1 {
		t.Fatalf("caller mutated policies: %+v", got)
	}
	*got.UserLimit, *got.AccountLimit = 8, 8
	for _, invalid := range []int{0, -1} {
		limiter.SetUserLimit("user", &invalid)
		limiter.SetAccountLimit("account", &invalid)
	}
	got = limiter.Snapshot("user", "account")
	if *got.UserLimit != 1 || *got.AccountLimit != 1 {
		t.Fatalf("invalid policies/snapshot mutation changed limits: %+v", got)
	}
	if limiter.Snapshot("user", "").AccountLimit != nil {
		t.Fatal("blank AuthID must not create an account policy")
	}
}

func TestConcurrencyCancellationReclaimsQueueCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newTestConcurrencyLimiter(t, 1)
		defer limiter.Close()
		one := 1
		limiter.SetAccountLimit("account", &one)
		active := acquireConcurrency(t, limiter, "holder", "account")
		ctx, cancel := context.WithCancel(context.Background())
		queued := queueConcurrency(t, limiter, ctx, "canceled", "account")
		cancel()
		receiveConcurrency(t, queued, context.Canceled)
		if got := limiter.Snapshot("canceled", "account"); got.TotalActive != 1 || got.QueueLength != 0 || got.UserActive != 0 {
			t.Fatalf("canceling waiter affected active resources: %+v", got)
		}
		replacement := queueConcurrency(t, limiter, context.Background(), "replacement", "account")
		active.Release()
		receiveConcurrency(t, replacement, nil).Release()
		assertConcurrencyEmpty(t, limiter)
	})
}

func TestConcurrencyCancellationGrantRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newTestConcurrencyLimiter(t, 1)
		defer limiter.Close()
		one := 1
		limiter.SetUserLimit("user", &one)
		limiter.SetAccountLimit("account", &one)
		for range 200 {
			active := acquireConcurrency(t, limiter, "user", "account")
			ctx, cancel := context.WithCancel(context.Background())
			queued := queueConcurrency(t, limiter, ctx, "user", "account")
			start := make(chan struct{})
			var group sync.WaitGroup
			group.Go(func() { <-start; active.Release() })
			group.Go(func() { <-start; cancel() })
			close(start)
			group.Wait()
			synctest.Wait()
			result := <-queued
			if result.err == nil {
				if result.permit == nil {
					t.Fatal("successful race admission returned nil permit")
				}
				result.permit.Release()
				result.permit.Release()
			} else if !errors.Is(result.err, context.Canceled) || result.permit != nil {
				t.Fatalf("racing admission = %v, %v", result.permit, result.err)
			}
			assertConcurrencyEmpty(t, limiter)
		}
	})
}

func TestConcurrencyCloseCancelReleaseRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 100 {
			limiter := newTestConcurrencyLimiter(t, 1)
			one := 1
			limiter.SetAccountLimit("account", &one)
			active := acquireConcurrency(t, limiter, "holder", "account")
			ctx, cancel := context.WithCancel(context.Background())
			queued := queueConcurrency(t, limiter, ctx, "waiter", "account")
			start := make(chan struct{})
			var group sync.WaitGroup
			group.Go(func() { <-start; active.Release() })
			group.Go(func() { <-start; cancel() })
			group.Go(func() { <-start; limiter.Close(); limiter.Close() })
			group.Go(func() { <-start; limiter.SetAccountLimit("account", nil) })
			close(start)
			group.Wait()
			synctest.Wait()
			result := <-queued
			if result.err == nil {
				if result.permit == nil {
					t.Fatal("successful race admission returned nil permit")
				}
				result.permit.Release()
			} else if result.permit != nil || (!errors.Is(result.err, context.Canceled) && !errors.Is(result.err, ErrConcurrencyClosed)) {
				t.Fatalf("close race admission = %v, %v", result.permit, result.err)
			}
			assertConcurrencyEmpty(t, limiter)
		}
	})
}
