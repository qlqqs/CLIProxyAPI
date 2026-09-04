package service

import (
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
