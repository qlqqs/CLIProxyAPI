package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultLoginLimitBurst      = 5
	defaultLoginLimitMaxEntries = 10_000
)

// LoginLimiterConfig controls the bounded in-memory login limiter.
type LoginLimiterConfig struct {
	Burst          int
	RefillInterval time.Duration
	MaxEntries     int
	Now            func() time.Time
}

// LoginLimiter limits password verification work by opaque user/address key.
type LoginLimiter struct {
	mu             sync.Mutex
	buckets        map[string]loginBucket
	burst          float64
	refillInterval time.Duration
	maxEntries     int
	now            func() time.Time
}

type loginBucket struct {
	tokens       float64
	updatedAt    time.Time
	lastActivity time.Time
}

// NewLoginLimiter creates a bounded token-bucket limiter.
func NewLoginLimiter(cfg LoginLimiterConfig) *LoginLimiter {
	burst := cfg.Burst
	if burst <= 0 {
		burst = defaultLoginLimitBurst
	}
	refillInterval := cfg.RefillInterval
	if refillInterval <= 0 {
		refillInterval = time.Minute
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultLoginLimitMaxEntries
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &LoginLimiter{
		buckets:        make(map[string]loginBucket),
		burst:          float64(burst),
		refillInterval: refillInterval,
		maxEntries:     maxEntries,
		now:            now,
	}
}

// LoginLimitKey returns a non-reversible key for a normalized username and client address.
func LoginLimitKey(normalizedUsername, clientAddress string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(normalizedUsername) + "\x00" + strings.TrimSpace(clientAddress)))
	return hex.EncodeToString(digest[:])
}

// Allow consumes one attempt and returns a retry delay when the bucket is empty.
func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now().UTC()
	key = strings.TrimSpace(key)
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.buckets[key]
	if !exists {
		if len(l.buckets) >= l.maxEntries {
			l.evictOldestLocked()
		}
		bucket = loginBucket{tokens: l.burst, updatedAt: now, lastActivity: now}
	}
	if now.After(bucket.updatedAt) {
		elapsed := now.Sub(bucket.updatedAt)
		bucket.tokens += float64(elapsed) / float64(l.refillInterval)
		if bucket.tokens > l.burst {
			bucket.tokens = l.burst
		}
		bucket.updatedAt = now
	}
	bucket.lastActivity = now
	if bucket.tokens < 1 {
		l.buckets[key] = bucket
		missing := 1 - bucket.tokens
		retryAfter := time.Duration(missing * float64(l.refillInterval))
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		return false, retryAfter
	}
	bucket.tokens--
	l.buckets[key] = bucket
	return true, 0
}

// Reset removes a bucket after a successful login.
func (l *LoginLimiter) Reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.buckets, strings.TrimSpace(key))
	l.mu.Unlock()
}

func (l *LoginLimiter) evictOldestLocked() {
	if len(l.buckets) == 0 {
		return
	}
	keys := make([]string, 0, len(l.buckets))
	for key := range l.buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	oldestKey := keys[0]
	oldest := l.buckets[oldestKey].lastActivity
	for _, key := range keys[1:] {
		if candidate := l.buckets[key].lastActivity; candidate.Before(oldest) {
			oldestKey = key
			oldest = candidate
		}
	}
	delete(l.buckets, oldestKey)
}
