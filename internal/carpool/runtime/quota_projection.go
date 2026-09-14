package runtime

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// QuotaWindow is the safe, provider-neutral projection of one upstream quota
// window. Nil values mean that the upstream did not provide that field.
type QuotaWindow struct {
	ID            string
	Label         string
	UsedPercent   *float64
	ResetAt       *time.Time
	WindowMinutes *int64
	Status        string
}

// AccountQuota is a read-only snapshot of the quota watermarks observed by
// CPA. It deliberately does not expose raw response headers or scheduler
// cooldown state.
type AccountQuota struct {
	Supported  bool
	ObservedAt time.Time
	Stale      bool
	Windows    []QuotaWindow
}

// ProjectAccountQuota converts a runtime Auth quota snapshot into the bounded
// DTO used by the carpool service and browser API.
func ProjectAccountQuota(auth *coreauth.Auth, now time.Time, maxAge time.Duration) AccountQuota {
	if auth == nil {
		return AccountQuota{Stale: true}
	}
	supported := coreauth.ProviderSupportsQuotaObservation(auth.Provider)
	observedAt := auth.Quota.ObservedAt.UTC()
	if maxAge <= 0 {
		maxAge = DefaultAccountObservationMaxAge
	}
	now = now.UTC()
	stale := observedAt.IsZero() || now.Before(observedAt) || now.Sub(observedAt) > maxAge
	projection := AccountQuota{Supported: supported, ObservedAt: observedAt, Stale: stale}
	if !supported || len(auth.Quota.Signals) == 0 {
		return projection
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		projection.Windows = projectClaudeWindows(auth.Quota.Signals)
	} else {
		projection.Windows = projectCodexWindows(auth.Quota.Signals, observedAt)
	}
	return projection
}

type quotaWindowBuilder struct {
	usedPercent   *float64
	resetAt       *time.Time
	windowMinutes *int64
	status        string
}

func projectClaudeWindows(signals map[string]string) []QuotaWindow {
	windows := make(map[string]*quotaWindowBuilder)
	for key, value := range signals {
		lower := strings.ToLower(strings.TrimSpace(key))
		const prefix = "anthropic-ratelimit-unified-"
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		field := strings.TrimPrefix(lower, prefix)
		name, kind := splitQuotaField(field)
		if name == "" || (kind == "" && !strings.HasSuffix(field, "-status")) {
			continue
		}
		// Fallback and representative claims describe routing decisions rather
		// than an independently measurable window.
		if name == "fallback" || name == "representative-claim" {
			continue
		}
		builder := windows[name]
		if builder == nil {
			builder = &quotaWindowBuilder{}
			windows[name] = builder
		}
		switch kind {
		case "status":
			builder.status = quotaStatus(value)
		case "utilization":
			if used, ok := parsePercent(value, true); ok {
				builder.usedPercent = &used
			}
		case "reset":
			if reset, ok := parseUnixTime(value); ok {
				builder.resetAt = &reset
			}
		}
	}
	return buildQuotaWindows(windows, nil)
}

func projectCodexWindows(signals map[string]string, observedAt time.Time) []QuotaWindow {
	windows := make(map[string]*quotaWindowBuilder)
	for key, value := range signals {
		lower := strings.ToLower(strings.TrimSpace(key))
		const prefix = "x-codex-"
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		field := strings.TrimPrefix(lower, prefix)
		name, kind := splitQuotaField(field)
		if name == "" || kind == "" {
			continue
		}
		builder := windows[name]
		if builder == nil {
			builder = &quotaWindowBuilder{}
			windows[name] = builder
		}
		switch kind {
		case "used-percent":
			if used, ok := parsePercent(value, false); ok {
				builder.usedPercent = &used
			}
		case "window-minutes":
			if minutes, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64); errParse == nil && minutes > 0 {
				builder.windowMinutes = &minutes
			}
		case "reset-after-seconds":
			if seconds, errParse := strconv.ParseInt(strings.TrimSpace(value), 10, 64); errParse == nil && seconds >= 0 && seconds <= 366*24*60*60 && !observedAt.IsZero() {
				reset := observedAt.Add(time.Duration(seconds) * time.Second)
				builder.resetAt = &reset
			}
		case "reset-at":
			if reset, ok := parseUnixTime(value); ok {
				builder.resetAt = &reset
			}
		case "allowed":
			builder.status = boolQuotaStatus(value, "allowed", "rejected", builder.status)
		case "limit-reached":
			builder.status = boolQuotaStatus(value, "exceeded", "allowed", builder.status)
		case "limit-name":
			// The namespace itself is the stable ID. The value is provider text
			// and is intentionally not copied into the public DTO.
		}
	}
	return buildQuotaWindows(windows, nil)
}

func splitQuotaField(field string) (string, string) {
	for _, marker := range []string{"-reset-after-seconds", "-window-minutes", "-used-percent", "-over-secondary-limit-percent", "-limit-reached", "-limit-name", "-allowed", "-status", "-utilization", "-reset"} {
		if strings.HasSuffix(field, marker) {
			return strings.TrimSuffix(field, marker), strings.TrimPrefix(marker, "-")
		}
	}
	return "", ""
}

func buildQuotaWindows(builders map[string]*quotaWindowBuilder, _ map[string]string) []QuotaWindow {
	ids := make([]string, 0, len(builders))
	for id := range builders {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		rank := func(id string) int {
			switch id {
			case "5h", "primary":
				return 0
			case "7d", "secondary":
				return 1
			default:
				return 2
			}
		}
		ri, rj := rank(ids[i]), rank(ids[j])
		if ri != rj {
			return ri < rj
		}
		return ids[i] < ids[j]
	})
	result := make([]QuotaWindow, 0, len(ids))
	for _, id := range ids {
		builder := builders[id]
		if builder == nil {
			continue
		}
		status := builder.status
		if status == "" {
			status = "observed"
			if builder.usedPercent != nil && *builder.usedPercent >= 100 {
				status = "exceeded"
			}
		}
		result = append(result, QuotaWindow{ID: id, Label: quotaWindowLabel(id), UsedPercent: builder.usedPercent, ResetAt: builder.resetAt, WindowMinutes: builder.windowMinutes, Status: status})
	}
	return result
}

func quotaWindowLabel(id string) string {
	switch id {
	case "5h":
		return "5 小时窗口"
	case "7d":
		return "7 天窗口"
	case "primary":
		return "主窗口"
	case "secondary":
		return "次窗口"
	default:
		return id
	}
}

func parsePercent(raw string, ratio bool) (float64, bool) {
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	if ratio && value <= 1 {
		value *= 100
	}
	if value > 100 {
		return 0, false
	}
	return value, true
}

func parseUnixTime(raw string) (time.Time, bool) {
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(math.MaxInt64) {
		return time.Time{}, false
	}
	seconds := int64(value)
	nanos := int64((value - float64(seconds)) * 1e9)
	return time.Unix(seconds, nanos).UTC(), true
}

func quotaStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "allowed", "active", "ok", "available":
		return "allowed"
	case "rejected", "blocked", "denied", "not_allowed":
		return "rejected"
	case "exceeded", "limit_reached", "over_limit":
		return "exceeded"
	default:
		return "unknown"
	}
}

func boolQuotaStatus(raw, trueStatus, falseStatus, current string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes":
		return trueStatus
	case "false", "0", "no":
		if current == "" || current == "unknown" {
			return falseStatus
		}
	}
	return current
}
