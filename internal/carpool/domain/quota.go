package domain

import "time"

const (
	QuotaWindowFiveHour = "5h"
	QuotaWindowWeekly   = "7d"
)

type MemberQuotaLimits struct {
	MonthlyNanoUSD  *int64
	FiveHourNanoUSD *int64
	WeeklyNanoUSD   *int64
	UserConcurrency *int
}

type MemberLimitsUpdate struct {
	MonthlySet, FiveHourSet, WeeklySet, UserConcurrencySet bool
	MonthlyNanoUSD, FiveHourNanoUSD, WeeklyNanoUSD         *int64
	UserConcurrency                                        *int
}

type QuotaWindowObservation struct {
	AuthID, Kind              string
	From, ResetAt, ObservedAt time.Time
}

type MemberQuotaWindow struct {
	Kind                                string
	LimitNanoUSD                        *int64
	ConfirmedNanoUSD, UnknownCostEvents int64
	From, ResetAt, CoverageFrom         time.Time
	PendingSync                         bool
}
