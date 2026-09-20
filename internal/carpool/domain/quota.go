package domain

import "time"

const (
	QuotaWindowFiveHour = "5h"
	QuotaWindowWeekly   = "7d"
)

type MemberQuotaLimits struct {
	// MonthlyNanoUSD is retained for source compatibility and is not active.
	MonthlyNanoUSD  *int64
	FiveHourNanoUSD *int64
	WeeklyNanoUSD   *int64
	UserConcurrency *int
}

type MemberLimitsUpdate struct {
	// Monthly fields are ignored by active quota writes.
	MonthlySet                                 bool
	MonthlyNanoUSD                             *int64
	FiveHourSet, WeeklySet, UserConcurrencySet bool
	FiveHourNanoUSD, WeeklyNanoUSD             *int64
	UserConcurrency                            *int
}

type MemberQuotaWindow struct {
	Kind                                string
	LimitNanoUSD                        *int64
	ConfirmedNanoUSD, UnknownCostEvents int64
	From, ResetAt                       time.Time
}
