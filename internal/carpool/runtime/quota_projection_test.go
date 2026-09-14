package runtime

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestProjectAccountQuotaClaudeWindows(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{Provider: "claude", Quota: coreauth.QuotaState{
		ObservedAt: now.Add(-time.Minute),
		Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Status":           "allowed",
			"Anthropic-Ratelimit-Unified-5h-Utilization":      "0.53",
			"Anthropic-Ratelimit-Unified-5h-Reset":            "1787296800",
			"Anthropic-Ratelimit-Unified-7d-Status":           "rejected",
			"Anthropic-Ratelimit-Unified-7d-Utilization":      "53",
			"Anthropic-Ratelimit-Unified-7d-Reset":            "1787695200",
			"Anthropic-Ratelimit-Unified-Fallback-Percentage": "0.5",
		},
	}}
	got := ProjectAccountQuota(auth, now, 5*time.Minute)
	if !got.Supported || got.Stale || len(got.Windows) != 2 {
		t.Fatalf("projection = %#v, want two fresh windows", got)
	}
	if got.Windows[0].ID != "5h" || got.Windows[0].UsedPercent == nil || *got.Windows[0].UsedPercent != 53 || got.Windows[0].Status != "allowed" {
		t.Fatalf("5h window = %#v", got.Windows[0])
	}
	if got.Windows[1].ID != "7d" || got.Windows[1].UsedPercent == nil || *got.Windows[1].UsedPercent != 53 || got.Windows[1].Status != "rejected" {
		t.Fatalf("7d window = %#v", got.Windows[1])
	}
	if got.Windows[0].ResetAt == nil || !got.Windows[0].ResetAt.Equal(time.Unix(1787296800, 0).UTC()) {
		t.Fatalf("5h reset = %v", got.Windows[0].ResetAt)
	}
}

func TestProjectAccountQuotaCodexRelativeResetAndOrdering(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{Provider: "codex", Quota: coreauth.QuotaState{
		ObservedAt: now,
		Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":         "101", // invalid, must remain absent
			"X-Codex-Primary-Used-Percent":           "81",
			"X-Codex-Primary-Window-Minutes":         "300",
			"X-Codex-Primary-Reset-After-Seconds":    "120",
			"X-Codex-Primary-Limit-Reached":          "false",
			"X-Codex-Secondary-Allowed":              "false",
			"X-Codex-Bengalfox-Primary-Used-Percent": "20",
			"X-Codex-Bengalfox-Primary-Reset-At":     "1787296800",
			"X-Codex-Active-Limit":                   "bengalfox",
		},
	}}
	got := ProjectAccountQuota(auth, now, time.Minute)
	if len(got.Windows) != 3 {
		t.Fatalf("windows = %#v, want primary, secondary, and named primary", got.Windows)
	}
	if got.Windows[0].ID != "primary" || got.Windows[1].ID != "secondary" {
		t.Fatalf("window ordering = %#v", got.Windows)
	}
	if got.Windows[0].UsedPercent == nil || *got.Windows[0].UsedPercent != 81 || got.Windows[0].Status != "allowed" {
		t.Fatalf("primary = %#v", got.Windows[0])
	}
	if got.Windows[0].ResetAt == nil || !got.Windows[0].ResetAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("relative reset = %v", got.Windows[0].ResetAt)
	}
	if got.Windows[1].UsedPercent != nil || got.Windows[1].Status != "rejected" {
		t.Fatalf("secondary = %#v", got.Windows[1])
	}
	if got.Windows[2].ID != "bengalfox-primary" || got.Windows[2].UsedPercent == nil {
		t.Fatalf("named window = %#v", got.Windows[2])
	}
}

func TestProjectAccountQuotaMissingAndStaleStates(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	unsupported := ProjectAccountQuota(&coreauth.Auth{Provider: "gemini"}, now, time.Minute)
	if unsupported.Supported || !unsupported.Stale || len(unsupported.Windows) != 0 {
		t.Fatalf("unsupported = %#v", unsupported)
	}
	stale := ProjectAccountQuota(&coreauth.Auth{Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now.Add(-2 * time.Minute), Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Status": "allowed"}}}, now, time.Minute)
	if !stale.Supported || !stale.Stale || len(stale.Windows) != 1 {
		t.Fatalf("stale = %#v", stale)
	}
	if missing := ProjectAccountQuota(nil, now, time.Minute); !missing.Stale || missing.Supported {
		t.Fatalf("missing auth = %#v", missing)
	}
}
