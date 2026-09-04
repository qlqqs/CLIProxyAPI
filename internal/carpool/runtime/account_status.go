package runtime

import (
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const DefaultAccountObservationMaxAge = 5 * time.Minute

// AccountStatus is the bounded status vocabulary exposed to carpool users.
type AccountStatus string

const (
	AccountStatusHealthy     AccountStatus = "healthy"
	AccountStatusDegraded    AccountStatus = "degraded"
	AccountStatusUnavailable AccountStatus = "unavailable"
	AccountStatusDisabled    AccountStatus = "disabled"
	AccountStatusUnknown     AccountStatus = "unknown"
)

// AccountHealth is a safe projection of runtime credential health.
type AccountHealth struct {
	Status     AccountStatus
	ObservedAt time.Time
	Stale      bool
}

// ProjectAccountHealth folds an Auth clone into the public carpool status vocabulary.
func ProjectAccountHealth(auth *coreauth.Auth, now time.Time, maxAge time.Duration) AccountHealth {
	if auth == nil {
		return AccountHealth{Status: AccountStatusUnknown, Stale: true}
	}
	if maxAge <= 0 {
		maxAge = DefaultAccountObservationMaxAge
	}
	now = now.UTC()
	observedAt := latestAuthObservation(auth).UTC()
	stale := observedAt.IsZero() || now.Sub(observedAt) > maxAge
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return AccountHealth{Status: AccountStatusDisabled, ObservedAt: observedAt, Stale: stale}
	}
	if stale {
		return AccountHealth{Status: AccountStatusUnknown, ObservedAt: observedAt, Stale: true}
	}
	if permanentlyUnavailable(auth, now) {
		return AccountHealth{Status: AccountStatusUnavailable, ObservedAt: observedAt}
	}
	if temporarilyDegraded(auth, now) {
		return AccountHealth{Status: AccountStatusDegraded, ObservedAt: observedAt}
	}
	if auth.Status != coreauth.StatusActive {
		return AccountHealth{Status: AccountStatusUnavailable, ObservedAt: observedAt}
	}
	return AccountHealth{Status: AccountStatusHealthy, ObservedAt: observedAt}
}

func latestAuthObservation(auth *coreauth.Auth) time.Time {
	latest := latestTime(auth.UpdatedAt, auth.LastRefreshedAt, auth.Quota.ObservedAt)
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		latest = latestTime(latest, state.UpdatedAt, state.Quota.ObservedAt)
	}
	return latest
}

func permanentlyUnavailable(auth *coreauth.Auth, now time.Time) bool {
	if auth.Status == coreauth.StatusError {
		return true
	}
	if auth.Unavailable && (auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(now)) {
		return true
	}
	return auth.Quota.Exceeded && (auth.Quota.NextRecoverAt.IsZero() || !auth.Quota.NextRecoverAt.After(now))
}

func temporarilyDegraded(auth *coreauth.Auth, now time.Time) bool {
	if auth.Unavailable || auth.NextRetryAfter.After(now) || auth.Quota.Exceeded || auth.Quota.NextRecoverAt.After(now) || auth.LastError != nil {
		return true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable || state.NextRetryAfter.After(now) || state.Quota.Exceeded || state.Quota.NextRecoverAt.After(now) || state.LastError != nil || state.Status == coreauth.StatusError {
			return true
		}
	}
	return false
}

func latestTime(values ...time.Time) time.Time {
	var latest time.Time
	for _, value := range values {
		if value.After(latest) {
			latest = value
		}
	}
	return latest
}
