package runtime

import (
	"strconv"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCheckerCodexAbsoluteResetPreferred(t *testing.T) {
	observed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	absolute := observed.Add(time.Hour)
	for _, test := range []struct {
		name, reset string
		want        time.Time
	}{
		{"valid absolute overrides relative", strconv.FormatInt(absolute.Unix(), 10), absolute},
		{"invalid absolute retains relative", "invalid", observed.Add(30 * time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &coreauth.Auth{Provider: "codex", Quota: coreauth.QuotaState{ObservedAt: observed, Signals: map[string]string{
				"x-codex-primary-reset-at":            test.reset,
				"x-codex-primary-reset-after-seconds": "1800",
				"x-codex-primary-window-minutes":      "300",
			}}}
			// Repeated projection must not depend on randomized signal map traversal.
			for range 100 {
				quota := ProjectAccountQuota(auth, observed.Add(10*time.Minute), time.Hour)
				if len(quota.Windows) != 1 || quota.Windows[0].ResetAt == nil || !quota.Windows[0].ResetAt.Equal(test.want) {
					t.Fatalf("quota = %+v; want reset %v", quota, test.want)
				}
			}
		})
	}
}
