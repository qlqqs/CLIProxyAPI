package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	carpoolbilling "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/billing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

// memberLimitsRequest distinguishes omitted fields from explicitly removed limits.
type memberLimitsRequest struct {
	Monthly     optionalJSON[string] `json:"monthly_limit_usd"`
	FiveHour    optionalJSON[string] `json:"five_hour_limit_usd"`
	Weekly      optionalJSON[string] `json:"weekly_limit_usd"`
	Concurrency optionalJSON[int]    `json:"concurrency_limit"`
}

func (r memberLimitsRequest) update() (domain.MemberLimitsUpdate, error) {
	update := domain.MemberLimitsUpdate{MonthlySet: r.Monthly.Set, FiveHourSet: r.FiveHour.Set, WeeklySet: r.Weekly.Set, UserConcurrencySet: r.Concurrency.Set}
	if !r.Monthly.Set && !r.FiveHour.Set && !r.Weekly.Set && !r.Concurrency.Set {
		return update, domain.ErrInvalid
	}
	for _, field := range []struct {
		source   optionalJSON[string]
		target   **int64
		required bool
	}{
		{r.Monthly, &update.MonthlyNanoUSD, true}, {r.FiveHour, &update.FiveHourNanoUSD, false}, {r.Weekly, &update.WeeklyNanoUSD, false},
	} {
		if !field.source.Set {
			continue
		}
		if field.source.Null {
			if field.required {
				return update, domain.ErrInvalid
			}
			continue
		}
		value, errParse := carpoolbilling.ParseNanoUSD(field.source.Value)
		if errParse != nil {
			return update, domain.ErrInvalid
		}
		*field.target = &value
	}
	if r.Concurrency.Set && !r.Concurrency.Null {
		if r.Concurrency.Value <= 0 {
			return update, domain.ErrInvalid
		}
		value := r.Concurrency.Value
		update.UserConcurrency = &value
	}
	return update, nil
}

func appendMemberLimits(response gin.H, limits domain.MemberQuotaLimits, windows []domain.MemberQuotaWindow) gin.H {
	response["five_hour_limit_usd"] = formatNanoUSD(limits.FiveHourNanoUSD)
	response["weekly_limit_usd"] = formatNanoUSD(limits.WeeklyNanoUSD)
	response["concurrency_limit"] = limits.UserConcurrency
	if windows != nil {
		response["quota_windows"] = memberQuotaWindowsResponse(windows)
	}
	return response
}

func memberQuotaWindowsResponse(windows []domain.MemberQuotaWindow) []gin.H {
	result := make([]gin.H, 0, len(windows))
	for _, window := range windows {
		status := "active"
		var used, remaining, overage *int64
		if window.PendingSync {
			status = "pending_sync"
		} else {
			used = pointerInt64(window.ConfirmedNanoUSD)
			overage = pointerInt64(0)
			if window.LimitNanoUSD != nil {
				left := *window.LimitNanoUSD - window.ConfirmedNanoUSD
				if left <= 0 {
					status = "exhausted"
				}
				if left < 0 {
					status = "overage"
					overage = pointerInt64(-left)
					left = 0
				}
				remaining = &left
			}
		}
		if window.LimitNanoUSD == nil && !window.PendingSync {
			status = "unlimited"
		}
		complete := !window.PendingSync && window.UnknownCostEvents == 0 && !window.CoverageFrom.After(window.From)
		result = append(result, gin.H{
			"kind": window.Kind, "status": status, "limit_usd": formatNanoUSD(window.LimitNanoUSD),
			"used_usd": formatNanoUSD(used), "remaining_usd": formatNanoUSD(remaining), "overage_usd": formatNanoUSD(overage),
			"period_from": optionalTime(window.From), "reset_at": optionalTime(window.ResetAt), "coverage_from": optionalTime(window.CoverageFrom),
			"unknown_cost_events": window.UnknownCostEvents, "data_complete": complete,
		})
	}
	return result
}

func (a *API) updateAccountConcurrency(c *gin.Context) {
	var request struct {
		Limit optionalJSON[int] `json:"concurrency_limit"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if !request.Limit.Set || (!request.Limit.Null && request.Limit.Value <= 0) {
		writeMappedError(c, domain.ErrInvalid)
		return
	}
	var limit *int
	if !request.Limit.Null {
		limit = &request.Limit.Value
	}
	identity, _ := currentIdentity(c)
	if errUpdate := a.control.SetAccountConcurrency(c.Request.Context(), identity.User, c.Param("car_ref"), c.Param("account_ref"), limit); errUpdate != nil {
		writeMappedError(c, errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"account_ref": c.Param("account_ref"), "concurrency_limit": limit})
}
