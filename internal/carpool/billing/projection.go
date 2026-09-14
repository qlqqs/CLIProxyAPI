package billing

import (
	"math/big"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

const (
	StatusActive        = "active"
	StatusExhausted     = "exhausted"
	StatusOverage       = "overage"
	StatusNotConfigured = "not_configured"
)

// Snapshot projects a billing ledger into the stable API representation.
func Snapshot(period Period, limit *int64, confirmed, resetBaseline, unknown int64, timezone string, coverage *time.Time) domain.BillingSnapshot {
	used := confirmed - resetBaseline
	if used < 0 {
		used = 0
	}
	remaining := (*int64)(nil)
	overage := int64(0)
	var percent *int64
	status := StatusActive
	if limit == nil {
		status = StatusNotConfigured
	} else if *limit == 0 {
		status = StatusExhausted
	} else {
		if used < *limit {
			v := *limit - used
			remaining = &v
		} else {
			v := int64(0)
			remaining = &v
			status = StatusExhausted
		}
		if used > *limit {
			overage = used - *limit
			status = StatusOverage
		}
		// Use arbitrary precision for the percentage so malformed/large values cannot overflow.
		n := new(big.Int).Mul(big.NewInt(used), big.NewInt(100))
		n.Quo(n, big.NewInt(*limit))
		if n.IsInt64() {
			v := n.Int64()
			percent = &v
		}
	}
	return domain.BillingSnapshot{Currency: "USD", Status: status, LimitNanoUSD: cloneInt(limit), ConfirmedNanoUSD: used,
		RemainingNanoUSD: remaining, OverageNanoUSD: overage, UsagePercent: percent,
		UnknownCostEvents: unknown, DataComplete: unknown == 0, PeriodFrom: period.From.UTC(), PeriodTo: period.To.UTC(), Timezone: timezone, CoverageFrom: cloneTime(coverage)}
}
func cloneInt(v *int64) *int64 {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
func cloneTime(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	x := v.UTC()
	return &x
}
