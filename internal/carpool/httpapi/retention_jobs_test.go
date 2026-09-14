package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestConfirmedRetentionHTTPHandshakeAndSecurity(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	login := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": "operator", "password": "administrator-pass"}, nil, "", true)
	assertHTTPStatus(t, login, http.StatusCreated)
	cookie := responseCookie(t, login, sessionCookieName)
	csrf := stringField(t, decodeResponseObject(t, login), "csrf_token")
	const base = "/carpool/api/v1/admin/retention"
	operation := map[string]any{"operation": "reset_current_period"}
	assertHTTPStatus(t, fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, "", true), http.StatusForbidden)
	assertHTTPStatus(t, fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, csrf, false), http.StatusForbidden)
	for _, body := range []map[string]any{{"operation": "reset_current_period", "confirm": true}, {"operation": "reset_current_period", "job_id": "unknown"}} {
		response := fixture.request(t, http.MethodPost, base+"/jobs", body, cookie, csrf, true)
		assertHTTPStatus(t, response, http.StatusUnprocessableEntity)
		if got := decodeResponseObject(t, response)["error"].(map[string]any)["code"]; got != "confirmation_required" {
			t.Fatalf("missing confirmation code=%v", got)
		}
	}
	ctx := context.Background()
	admin, err := fixture.store.GetUserByNormalizedUsername(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	user, err := fixture.store.CreateUser(ctx, domain.User{Username: "retention-passenger", DefaultDisplayName: "Passenger", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test-only"})
	if err != nil {
		t.Fatal(err)
	}
	car, err := fixture.store.CreateCar(ctx, domain.Car{Name: "Retention", Status: domain.CarStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(5_000_000_000)
	membership, err := fixture.store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: user.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID, MonthlyLimitNanoUSD: &limit}})
	if err != nil {
		t.Fatal(err)
	}
	now := fixture.now()
	period, err := fixture.store.EnsureBillingPeriod(ctx, domain.BillingPeriod{MembershipID: membership.ID, MemberRefSnapshot: membership.MemberRef, CarID: car.ID, Timezone: "UTC", AnchorAt: now, From: now, To: now.Add(24 * time.Hour), LimitNanoUSD: &limit, ConfirmedNanoUSD: 225_000_000, UnknownCostEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	preview := fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, csrf, true)
	assertHTTPStatus(t, preview, http.StatusOK)
	previewBody := decodeResponseObject(t, preview)
	id := stringField(t, previewBody, "job_id")
	if previewBody["status"] != "queued" || previewBody["expected_count"] != float64(1) || previewBody["batch_limit"] != float64(1000) || previewBody["confirmation_required"] != true || previewBody["expires_at"] == nil {
		t.Fatalf("preview contract=%+v", previewBody)
	}
	if _, exists := previewBody["confirmation"]; exists {
		t.Fatal("private confirmation snapshot leaked")
	}
	bad := fixture.request(t, http.MethodPost, base+"/jobs", map[string]any{"operation": "closed_periods", "job_id": id, "confirm": true}, cookie, csrf, true)
	assertHTTPStatus(t, bad, http.StatusConflict)
	if code := decodeResponseObject(t, bad)["error"].(map[string]any)["code"]; code != "retention_preview_invalid" {
		t.Fatalf("mismatch code=%v", code)
	}
	next := fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, csrf, true)
	assertHTTPStatus(t, next, http.StatusOK)
	nextID := stringField(t, decodeResponseObject(t, next), "job_id")
	confirm := map[string]any{"operation": "reset_current_period", "job_id": id, "confirm": true}
	assertHTTPStatus(t, fixture.request(t, http.MethodPost, base+"/jobs", confirm, cookie, "", true), http.StatusForbidden)
	result := fixture.request(t, http.MethodPost, base+"/jobs", confirm, cookie, csrf, true)
	assertHTTPStatus(t, result, http.StatusAccepted)
	resultBody := decodeResponseObject(t, result)
	if resultBody["status"] != "completed" || resultBody["deleted_count"] != float64(1) {
		t.Fatalf("result=%+v", resultBody)
	}
	current, err := fixture.store.GetBillingPeriod(ctx, membership.ID, now)
	if err != nil || current.ResetBaselineNanoUSD != period.ConfirmedNanoUSD || current.UnknownCostEvents != 0 {
		t.Fatalf("HTTP reset did not mutate amount: %+v err=%v", current, err)
	}
	repeated := fixture.request(t, http.MethodPost, base+"/jobs", confirm, cookie, csrf, true)
	assertHTTPStatus(t, repeated, http.StatusAccepted)
	fetched := fixture.request(t, http.MethodGet, base+"/jobs/"+id, nil, cookie, "", true)
	assertHTTPStatus(t, fetched, http.StatusOK)
	if !reflect.DeepEqual(resultBody, decodeResponseObject(t, repeated)) || !reflect.DeepEqual(resultBody, decodeResponseObject(t, fetched)) {
		t.Fatal("duplicate/read result differs")
	}
	limit++
	if _, err = fixture.store.SetMonthlyLimit(ctx, membership.ID, &limit, nil); err != nil {
		t.Fatal(err)
	}
	stale := fixture.request(t, http.MethodPost, base+"/jobs", map[string]any{"operation": "reset_current_period", "job_id": nextID, "confirm": true}, cookie, csrf, true)
	assertHTTPStatus(t, stale, http.StatusConflict)
	if code := decodeResponseObject(t, stale)["error"].(map[string]any)["code"]; code != "retention_preview_invalid" {
		t.Fatalf("stale code=%v", code)
	}
}
