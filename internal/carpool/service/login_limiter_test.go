package service

import (
	"fmt"
	"testing"
	"time"
)

func TestLoginLimiterRefillsWithoutWallClockSleep(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginLimiter(LoginLimiterConfig{
		Burst:          2,
		RefillInterval: time.Minute,
		MaxEntries:     10,
		Now:            func() time.Time { return now },
	})
	key := LoginLimitKey("alice", "192.0.2.10")
	for attempt := 0; attempt < 2; attempt++ {
		allowed, retryAfter := limiter.Allow(key)
		if !allowed || retryAfter != 0 {
			t.Fatalf("Allow(attempt=%d) = (%t, %s)", attempt, allowed, retryAfter)
		}
	}
	allowed, retryAfter := limiter.Allow(key)
	if allowed || retryAfter != time.Minute {
		t.Fatalf("Allow(exhausted) = (%t, %s), want (false, 1m)", allowed, retryAfter)
	}
	now = now.Add(30 * time.Second)
	allowed, retryAfter = limiter.Allow(key)
	if allowed || retryAfter != 30*time.Second {
		t.Fatalf("Allow(half-refill) = (%t, %s), want (false, 30s)", allowed, retryAfter)
	}
	now = now.Add(30 * time.Second)
	allowed, retryAfter = limiter.Allow(key)
	if !allowed || retryAfter != 0 {
		t.Fatalf("Allow(refilled) = (%t, %s)", allowed, retryAfter)
	}
}

func TestLoginLimiterReset(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginLimiter(LoginLimiterConfig{Burst: 1, Now: func() time.Time { return now }})
	key := LoginLimitKey("alice", "192.0.2.10")
	if allowed, _ := limiter.Allow(key); !allowed {
		t.Fatal("first Allow() rejected")
	}
	if allowed, _ := limiter.Allow(key); allowed {
		t.Fatal("second Allow() accepted")
	}
	limiter.Reset(key)
	if allowed, _ := limiter.Allow(key); !allowed {
		t.Fatal("Allow() rejected after Reset()")
	}
}

func TestLoginLimiterEvictsOldestBucket(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginLimiter(LoginLimiterConfig{Burst: 1, MaxEntries: 2, Now: func() time.Time { return now }})
	oldest := LoginLimitKey("oldest", "192.0.2.1")
	newer := LoginLimitKey("newer", "192.0.2.2")
	replacement := LoginLimitKey("replacement", "192.0.2.3")
	_, _ = limiter.Allow(oldest)
	now = now.Add(time.Second)
	_, _ = limiter.Allow(newer)
	now = now.Add(time.Second)
	_, _ = limiter.Allow(replacement)
	if allowed, _ := limiter.Allow(oldest); !allowed {
		t.Fatal("oldest bucket was not evicted")
	}
}

func TestLoginLimitKeyDoesNotExposeUsername(t *testing.T) {
	key := LoginLimitKey("sensitive-user", "192.0.2.10")
	if key == "" || key == "sensitive-user" {
		t.Fatalf("LoginLimitKey() = %q", key)
	}
}

func TestLoginSourceLimitIsIndependentAndRefills(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginLimiter(LoginLimiterConfig{SourceBurst: 1, SourceRefillInterval: time.Minute, MaxEntries: 1, Now: func() time.Time { return now }})
	release, retry := limiter.AcquireVerification("192.0.2.1")
	if release == nil || retry != 0 {
		t.Fatal("first source attempt rejected")
	}
	release()
	// Account resets and account-map eviction cannot replenish the independent source bucket.
	limiter.Reset(LoginLimitKey("alice", "192.0.2.1"))
	_, _ = limiter.Allow(LoginLimitKey("alice", "192.0.2.1"))
	_, _ = limiter.Allow(LoginLimitKey("bob", "192.0.2.1"))
	if release, retry := limiter.AcquireVerification("192.0.2.1"); release != nil || retry != time.Minute {
		t.Fatalf("exhausted source = %t, %s", release != nil, retry)
	}
	now = now.Add(time.Minute)
	release, retry = limiter.AcquireVerification("192.0.2.1")
	if release == nil || retry != 0 {
		t.Fatal("source did not refill")
	}
	release()
	other, _ := limiter.AcquireVerification("192.0.2.2")
	if other == nil {
		t.Fatal("unrelated source rejected")
	}
	other()
	if len(limiter.sourceBuckets) > 1 {
		t.Fatal("source map exceeded configured bound")
	}
}

func TestLoginVerificationConcurrencyIsBoundedAndReleaseIsIdempotent(t *testing.T) {
	limiter := NewLoginLimiter(LoginLimiterConfig{MaxConcurrent: 2})
	first, _ := limiter.AcquireVerification("192.0.2.1")
	second, _ := limiter.AcquireVerification("192.0.2.2")
	if first == nil || second == nil {
		t.Fatal("initial admission rejected")
	}
	if release, retry := limiter.AcquireVerification("192.0.2.3"); release != nil || retry <= 0 {
		t.Fatal("concurrent hashing limit bypassed")
	}
	first()
	first()
	third, _ := limiter.AcquireVerification("192.0.2.3")
	if third == nil {
		t.Fatal("released slot was not reusable")
	}
	if release, _ := limiter.AcquireVerification("192.0.2.4"); release != nil {
		t.Fatal("double release created an extra slot")
	}
	second()
	third()
}

func TestLoginVerificationConcurrentAdmission(t *testing.T) {
	limiter := NewLoginLimiter(LoginLimiterConfig{MaxConcurrent: 2})
	const workers = 20
	start := make(chan struct{})
	results := make(chan func(), workers)
	for i := 0; i < workers; i++ {
		go func(index int) {
			<-start
			release, _ := limiter.AcquireVerification(fmt.Sprintf("192.0.2.%d", index+1))
			results <- release
		}(i)
	}
	close(start)
	releases := make([]func(), 0, 2)
	for i := 0; i < workers; i++ {
		if release := <-results; release != nil {
			releases = append(releases, release)
		}
	}
	for _, release := range releases {
		release()
	}
	if len(releases) != 2 {
		t.Fatalf("concurrent admissions = %d, want 2", len(releases))
	}
}
