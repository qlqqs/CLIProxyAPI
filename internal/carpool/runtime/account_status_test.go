package runtime

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestProjectAccountHealth(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		auth       *coreauth.Auth
		wantStatus AccountStatus
		wantStale  bool
	}{
		{name: "missing", wantStatus: AccountStatusUnknown, wantStale: true},
		{name: "healthy", auth: &coreauth.Auth{Status: coreauth.StatusActive, UpdatedAt: now.Add(-time.Minute)}, wantStatus: AccountStatusHealthy},
		{name: "disabled wins", auth: &coreauth.Auth{Status: coreauth.StatusActive, Disabled: true, UpdatedAt: now.Add(-time.Hour)}, wantStatus: AccountStatusDisabled, wantStale: true},
		{name: "stale", auth: &coreauth.Auth{Status: coreauth.StatusActive, UpdatedAt: now.Add(-5*time.Minute - time.Nanosecond)}, wantStatus: AccountStatusUnknown, wantStale: true},
		{name: "boundary is fresh", auth: &coreauth.Auth{Status: coreauth.StatusActive, UpdatedAt: now.Add(-5 * time.Minute)}, wantStatus: AccountStatusHealthy},
		{name: "temporary cooldown", auth: &coreauth.Auth{Status: coreauth.StatusActive, Unavailable: true, NextRetryAfter: now.Add(time.Minute), UpdatedAt: now}, wantStatus: AccountStatusDegraded},
		{name: "permanent unavailable", auth: &coreauth.Auth{Status: coreauth.StatusActive, Unavailable: true, UpdatedAt: now}, wantStatus: AccountStatusUnavailable},
		{name: "recent error", auth: &coreauth.Auth{Status: coreauth.StatusActive, LastError: &coreauth.Error{Code: "test"}, UpdatedAt: now}, wantStatus: AccountStatusDegraded},
		{name: "partial model", auth: &coreauth.Auth{Status: coreauth.StatusActive, UpdatedAt: now, ModelStates: map[string]*coreauth.ModelState{"model": {Unavailable: true, UpdatedAt: now}}}, wantStatus: AccountStatusDegraded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectAccountHealth(test.auth, now, DefaultAccountObservationMaxAge)
			if got.Status != test.wantStatus || got.Stale != test.wantStale {
				t.Fatalf("ProjectAccountHealth() = %#v, want status=%q stale=%t", got, test.wantStatus, test.wantStale)
			}
		})
	}
}

func TestProjectAccountHealthUsesLatestModelObservation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		Status:    coreauth.StatusActive,
		UpdatedAt: now.Add(-time.Hour),
		ModelStates: map[string]*coreauth.ModelState{
			"model": {Status: coreauth.StatusActive, UpdatedAt: now.Add(-time.Minute)},
		},
	}
	got := ProjectAccountHealth(auth, now, DefaultAccountObservationMaxAge)
	if got.Status != AccountStatusHealthy || !got.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("ProjectAccountHealth() = %#v", got)
	}
}
