package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestMemberLimitsPatchAndPendingProjection(t *testing.T) {
	f := newCarpoolHTTPFlowFixture(t)
	ctx := t.Context()
	login := f.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": "operator", "password": "administrator-pass"}, nil, "", true)
	assertHTTPStatus(t, login, http.StatusCreated)
	cookie := responseCookie(t, login, sessionCookieName)
	csrf := stringField(t, decodeResponseObject(t, login), "csrf_token")
	admin, errAdmin := f.store.GetUserByNormalizedUsername(ctx, "operator")
	if errAdmin != nil {
		t.Fatal(errAdmin)
	}
	user, errUser := f.store.CreateUser(ctx, domain.User{Username: "limits-passenger", DefaultDisplayName: "Limits Passenger", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test"})
	if errUser != nil {
		t.Fatal(errUser)
	}
	car, errCar := f.store.CreateCar(ctx, domain.Car{CarRef: "car_AAAAAAAAAAAAAAAA", Name: "Limits Car", Status: domain.CarStatusActive})
	if errCar != nil {
		t.Fatal(errCar)
	}
	monthly := int64(25_000_000_000)
	member, errMember := f.store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: user.ID, CarID: car.ID, DisplayName: "Limits Passenger", DisplayNameKey: "limits passenger", CreatedByUserID: admin.ID, MonthlyLimitNanoUSD: &monthly}})
	if errMember != nil {
		t.Fatal(errMember)
	}
	assignment, errAssign := f.store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{CarID: car.ID, AuthID: "secret-auth-alice@example.com", SafeLabel: "Shared Account", SafeLabelKey: "shared account", ProviderSnapshot: "codex", CreatedByUserID: admin.ID}})
	if errAssign != nil {
		t.Fatal(errAssign)
	}
	memberURL := "/carpool/api/v1/admin/cars/" + car.CarRef + "/members/" + member.MemberRef + "/quota"
	patched := f.request(t, http.MethodPatch, memberURL, map[string]any{"five_hour_limit_usd": "1.25", "weekly_limit_usd": "5.000000001", "concurrency_limit": 2}, cookie, csrf, true)
	assertHTTPStatus(t, patched, http.StatusOK)
	body := decodeResponseObject(t, patched)
	if body["monthly_limit_usd"] != "25" || body["five_hour_limit_usd"] != "1.25" || body["weekly_limit_usd"] != "5.000000001" || body["concurrency_limit"] != float64(2) {
		t.Fatalf("limits = %#v", body)
	}
	list := f.request(t, http.MethodGet, "/carpool/api/v1/admin/cars/"+car.CarRef+"/members", nil, cookie, "", false)
	assertHTTPStatus(t, list, http.StatusOK)
	entries := decodeResponseObject(t, list)["items"].([]any)
	item := entries[0].(map[string]any)
	windows := item["quota_windows"].([]any)
	if len(windows) != 2 {
		t.Fatalf("windows=%#v", windows)
	}
	for _, raw := range windows {
		window := raw.(map[string]any)
		if window["status"] != "pending_sync" || window["reset_at"] != nil || window["used_usd"] != nil {
			t.Fatalf("pending window=%#v", window)
		}
	}
	for _, invalid := range []map[string]any{{}, {"concurrency_limit": 0}, {"concurrency_limit": 1.5}, {"monthly_limit_usd": nil}, {"weekly_limit_usd": "-1"}, {"five_hour_limit_usd": ""}, {"weekly_limit_usd": "999999999999999999999"}} {
		response := f.request(t, http.MethodPatch, memberURL, invalid, cookie, csrf, true)
		if response.Code < 400 {
			t.Fatalf("accepted invalid %#v: %s", invalid, response.Body.String())
		}
	}
	removed := f.request(t, http.MethodPatch, memberURL, map[string]any{"weekly_limit_usd": nil, "concurrency_limit": nil}, cookie, csrf, true)
	assertHTTPStatus(t, removed, http.StatusOK)
	body = decodeResponseObject(t, removed)
	if body["five_hour_limit_usd"] != "1.25" || body["weekly_limit_usd"] != nil || body["concurrency_limit"] != nil {
		t.Fatalf("partial removal=%#v", body)
	}
	accountURL := "/carpool/api/v1/admin/cars/" + car.CarRef + "/accounts/" + assignment.AccountRef + "/concurrency"
	updated := f.request(t, http.MethodPatch, accountURL, map[string]any{"concurrency_limit": 3}, cookie, csrf, true)
	assertHTTPStatus(t, updated, http.StatusOK)
	limit, errLimit := f.store.GetAccountConcurrency(ctx, assignment.AuthID)
	if errLimit != nil || limit == nil || *limit != 3 {
		t.Fatalf("account limit=%v error=%v", limit, errLimit)
	}
	otherCar, errOther := f.store.CreateCar(ctx, domain.Car{CarRef: "car_BBBBBBBBBBBBBBBB", Name: "Other Car", Status: domain.CarStatusActive})
	if errOther != nil {
		t.Fatal(errOther)
	}
	cross := strings.Replace(accountURL, car.CarRef, otherCar.CarRef, 1)
	assertHTTPStatus(t, f.request(t, http.MethodPatch, cross, map[string]any{"concurrency_limit": 9}, cookie, csrf, true), http.StatusNotFound)
	assertHTTPStatus(t, f.request(t, http.MethodPatch, accountURL, map[string]any{"concurrency_limit": 1}, cookie, "", true), http.StatusForbidden)
	assertHTTPStatus(t, f.request(t, http.MethodPatch, accountURL, map[string]any{"concurrency_limit": nil}, cookie, csrf, true), http.StatusOK)
}

func TestProxyGuardReleasesOnEarlyFailureAndStreamEnd(t *testing.T) {
	f := newProxyAuthorizationFixture(t)
	ctx := t.Context()
	one := 1
	if errSet := f.store.SetAccountConcurrency(ctx, f.authID, &one, nil); errSet != nil {
		t.Fatal(errSet)
	}
	engine := gin.New()
	engine.Use(f.api.ProxyCredentialGuard())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		_, errAuth := f.accessManager.Authenticate(c.Request.Context(), c.Request)
		if errAuth != nil {
			c.AbortWithStatusJSON(errAuth.HTTPStatusCode(), gin.H{"error": errAuth.Message})
			return
		}
		if c.GetHeader("X-Test-Early") == "true" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: done\n\n")
	})
	for index := 0; index < 4; index++ {
		// A guard deadline makes leaked ownership fail the test rather than hang it.
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+f.model+`"}`)).WithContext(requestCtx)
		request.Header.Set("Authorization", "Bearer "+f.token)
		if index%2 == 0 {
			request.Header.Set("X-Test-Early", "true")
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		cancel()
		want := http.StatusOK
		if index%2 == 0 {
			want = http.StatusBadRequest
		}
		if response.Code != want {
			t.Fatalf("request %d: %d %s", index, response.Code, response.Body.String())
		}
	}
}

func TestPendingWindowWithoutLimitStaysPending(t *testing.T) {
	windows := memberQuotaWindowsResponse([]domain.MemberQuotaWindow{
		{Kind: domain.QuotaWindowFiveHour, PendingSync: true},
		{Kind: domain.QuotaWindowWeekly, ConfirmedNanoUSD: 5, From: time.Unix(0, 0).UTC(), ResetAt: time.Unix(3600, 0).UTC()},
	})
	if windows[0]["status"] != "pending_sync" || windows[0]["limit_usd"] != nil || windows[0]["used_usd"] != nil {
		t.Fatalf("pending window without limit=%#v", windows[0])
	}
	if windows[1]["status"] != "unlimited" || windows[1]["used_usd"] == nil {
		t.Fatalf("synced window without limit=%#v", windows[1])
	}
}

func TestMemberLimitsRequestNullAndOmitted(t *testing.T) {
	var request memberLimitsRequest
	if err := json.Unmarshal([]byte(`{"five_hour_limit_usd":"0","weekly_limit_usd":null}`), &request); err != nil {
		t.Fatal(err)
	}
	update, errUpdate := request.update()
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if update.MonthlySet || update.UserConcurrencySet || !update.FiveHourSet || update.FiveHourNanoUSD == nil || *update.FiveHourNanoUSD != 0 || !update.WeeklySet || update.WeeklyNanoUSD != nil {
		t.Fatalf("update=%#v", update)
	}
}
