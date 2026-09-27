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
	defaultLoginSourceBurst     = 20
	defaultLoginMaxConcurrent   = 2
)

// LoginLimiterConfig controls the bounded in-memory login limiter.
type LoginLimiterConfig struct {
	Burst                int
	RefillInterval       time.Duration
	MaxEntries           int
	Now                  func() time.Time
	SourceBurst          int
	SourceRefillInterval time.Duration
	MaxConcurrent        int
}

// LoginLimiter bounds password verification by account, source address, and concurrent work.
type LoginLimiter struct {
	mu                   sync.Mutex
	buckets              map[string]loginBucket
	sourceBuckets        map[string]loginBucket
	sourceBurst          float64
	sourceRefillInterval time.Duration
	maxConcurrent        int
	activeVerifications  int
	burst                float64
	refillInterval       time.Duration
	maxEntries           int
	now                  func() time.Time
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
	sourceBurst := cfg.SourceBurst
	if sourceBurst <= 0 {
		sourceBurst = defaultLoginSourceBurst
	}
	sourceRefillInterval := cfg.SourceRefillInterval
	if sourceRefillInterval <= 0 {
		sourceRefillInterval = time.Minute
	}
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultLoginMaxConcurrent
	}
	return &LoginLimiter{
		buckets:              make(map[string]loginBucket),
		sourceBuckets:        make(map[string]loginBucket),
		sourceBurst:          float64(sourceBurst),
		sourceRefillInterval: sourceRefillInterval,
		maxConcurrent:        maxConcurrent,
		burst:                float64(burst),
		refillInterval:       refillInterval,
		maxEntries:           maxEntries,
		now:                  now,
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

	return l.allowLocked(l.buckets, key, l.burst, l.refillInterval, now)
}

// AcquireVerification admits source-address work before any password hashing begins.
// A nil release function means admission was rejected; successful login must not reset this budget.
func (l *LoginLimiter) AcquireVerification(clientAddress string) (release func(), retryAfter time.Duration) {
	if l == nil {
		return func() {}, 0
	}
	now := l.now().UTC()
	key := LoginLimitKey("", clientAddress)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.activeVerifications >= l.maxConcurrent {
		return nil, time.Second
	}
	if allowed, retry := l.allowLocked(l.sourceBuckets, key, l.sourceBurst, l.sourceRefillInterval, now); !allowed {
		return nil, retry
	}
	l.activeVerifications++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.activeVerifications--
			l.mu.Unlock()
		})
	}, 0
}

func (l *LoginLimiter) allowLocked(buckets map[string]loginBucket, key string, burst float64, refillInterval time.Duration, now time.Time) (bool, time.Duration) {
	bucket, exists := buckets[key]
	if !exists {
		if len(buckets) >= l.maxEntries {
			evictOldestLoginBucket(buckets)
		}
		bucket = loginBucket{tokens: burst, updatedAt: now, lastActivity: now}
	}
	if now.After(bucket.updatedAt) {
		elapsed := now.Sub(bucket.updatedAt)
		bucket.tokens += float64(elapsed) / float64(refillInterval)
		if bucket.tokens > burst {
			bucket.tokens = burst
		}
		bucket.updatedAt = now
	}
	bucket.lastActivity = now
	if bucket.tokens < 1 {
		buckets[key] = bucket
		missing := 1 - bucket.tokens
		retryAfter := time.Duration(missing * float64(refillInterval))
		if retryAfter <= 0 {
			retryAfter = time.Second
		}
		return false, retryAfter
	}
	bucket.tokens--
	buckets[key] = bucket
	return true, 0
}

// Reset removes only the account bucket after a successful login, preserving the source budget.
func (l *LoginLimiter) Reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.buckets, strings.TrimSpace(key))
	l.mu.Unlock()
}

func evictOldestLoginBucket(buckets map[string]loginBucket) {
	if len(buckets) == 0 {
		return
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	oldestKey := keys[0]
	oldest := buckets[oldestKey].lastActivity
	for _, key := range keys[1:] {
		if candidate := buckets[key].lastActivity; candidate.Before(oldest) {
			oldestKey = key
			oldest = candidate
		}
	}
	delete(buckets, oldestKey)
}
