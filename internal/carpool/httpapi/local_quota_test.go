package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestMemberLimitsRequestUsesZeroAsUnlimitedAndIgnoresMonthly(t *testing.T) {
	var request memberLimitsRequest
	if err := json.Unmarshal([]byte(`{"monthly_limit_usd":"1","five_hour_limit_usd":"0","weekly_limit_usd":"3"}`), &request); err != nil {
		t.Fatal(err)
	}
	update, err := request.update()
	if err != nil || !update.FiveHourSet || update.FiveHourNanoUSD == nil || *update.FiveHourNanoUSD != 0 || !update.WeeklySet || update.WeeklyNanoUSD == nil || *update.WeeklyNanoUSD != 3_000_000_000 {
		t.Fatalf("update=%+v err=%v", update, err)
	}
}

func TestCurrentQuotaViewsDoNotCreateMonthlyPeriodsAndLegacyPatchFieldIsIgnored(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	ctx := t.Context()
	login := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": "operator", "password": "administrator-pass"}, nil, "", true)
	assertHTTPStatus(t, login, http.StatusCreated)
	cookie := responseCookie(t, login, sessionCookieName)
	csrf := stringField(t, decodeResponseObject(t, login), "csrf_token")
	admin, err := fixture.store.GetUserByNormalizedUsername(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	user, err := fixture.store.CreateUser(ctx, domain.User{Username: "local-quota-passenger", DefaultDisplayName: "Local Quota", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test"})
	if err != nil {
		t.Fatal(err)
	}
	car, err := fixture.store.CreateCar(ctx, domain.Car{CarRef: "car_AAAAAAAAAAAAAAAA", Name: "Local Quota Car", Status: domain.CarStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	monthly, five, weekly := int64(25_000_000_000), int64(1_000_000_000), int64(2_000_000_000)
	member, err := fixture.store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: user.ID, CarID: car.ID, DisplayName: "Local Quota", CreatedByUserID: admin.ID, MonthlyLimitNanoUSD: &monthly, FiveHourLimitNanoUSD: &five, WeeklyLimitNanoUSD: &weekly}})
	if err != nil {
		t.Fatal(err)
	}

	list := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/cars/"+car.CarRef+"/members", nil, cookie, "", false)
	assertHTTPStatus(t, list, http.StatusOK)
	item := decodeResponseObject(t, list)["items"].([]any)[0].(map[string]any)
	if _, ok := item["monthly_limit_usd"]; ok {
		t.Fatalf("active member response leaked monthly limit: %#v", item)
	}
	if _, ok := item["billing"]; ok {
		t.Fatalf("active member response leaked monthly billing: %#v", item)
	}
	for _, raw := range item["quota_windows"].([]any) {
		window := raw.(map[string]any)
		if window["status"] != "not_started" || window["used_usd"] != "0" || window["reset_at"] != nil {
			t.Fatalf("new local window projection = %#v", window)
		}
	}

	patched := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/cars/"+car.CarRef+"/members/"+member.MemberRef+"/quota", map[string]any{"monthly_limit_usd": "0", "weekly_limit_usd": "3"}, cookie, csrf, true)
	assertHTTPStatus(t, patched, http.StatusOK)
	body := decodeResponseObject(t, patched)
	if body["five_hour_limit_usd"] != "1" || body["weekly_limit_usd"] != "3" {
		t.Fatalf("legacy field altered local patch: %#v", body)
	}
	if _, ok := body["monthly_limit_usd"]; ok {
		t.Fatalf("quota patch response leaked monthly limit: %#v", body)
	}
	stored, err := fixture.store.CurrentMembership(ctx, user.ID)
	if err != nil || stored.MonthlyLimitNanoUSD == nil || *stored.MonthlyLimitNanoUSD != monthly {
		t.Fatalf("legacy monthly storage changed: %+v err=%v", stored, err)
	}
	periods, err := fixture.store.ListBillingPeriods(ctx, member.ID, "", time.Time{}, time.Time{}, 10)
	if err != nil || len(periods) != 0 {
		t.Fatalf("current quota views created monthly periods: %+v err=%v", periods, err)
	}
}
