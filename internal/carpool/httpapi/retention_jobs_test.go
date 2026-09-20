package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestConfirmedRetentionHTTPHandshakeAndSecurity(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	login := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{"username": "operator", "password": "administrator-pass"}, nil, "", true)
	assertHTTPStatus(t, login, http.StatusCreated)
	cookie := responseCookie(t, login, sessionCookieName)
	csrf := stringField(t, decodeResponseObject(t, login), "csrf_token")
	const base = "/carpool/api/v1/admin/retention"
	operation := map[string]any{"operation": "reset_quota_windows"}
	assertHTTPStatus(t, fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, "", true), http.StatusForbidden)
	assertHTTPStatus(t, fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, csrf, false), http.StatusForbidden)
	for _, body := range []map[string]any{{"operation": "reset_quota_windows", "confirm": true}, {"operation": "reset_quota_windows", "job_id": "unknown"}} {
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
	membership, err := fixture.store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: user.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID, FiveHourLimitNanoUSD: &limit, WeeklyLimitNanoUSD: &limit}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := fixture.store.CreateAPIKey(ctx, domain.APIKey{UserID: user.ID, Name: "retention", SecretDigest: []byte("digest")})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := fixture.store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{CarID: car.ID, AuthID: "retention-auth", SafeLabel: "Retention", ProviderSnapshot: "codex", CreatedByUserID: admin.ID}})
	if err != nil {
		t.Fatal(err)
	}
	now := fixture.now()
	snapshot, err := fixture.store.AuthorizeAndBeginProxyRequest(ctx, domain.ProxyAuthorization{RequestID: "retention-request", UserID: user.ID, APIKeyID: key.KeyID, SourceFormat: "openai", StartedAt: now, RuntimeAuthIDs: []string{assignment.AuthID}})
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(225_000_000)
	if _, err = fixture.store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "retention-event", RequestID: snapshot.Request.RequestID, AuthID: assignment.AuthID, UsageKnown: true, Model: "test"}, "", &cost, "priced", ""); err != nil {
		t.Fatal(err)
	}
	preview := fixture.request(t, http.MethodPost, base+"/preview", operation, cookie, csrf, true)
	assertHTTPStatus(t, preview, http.StatusOK)
	previewBody := decodeResponseObject(t, preview)
	id := stringField(t, previewBody, "job_id")
	if previewBody["status"] != "queued" || previewBody["expected_count"] != float64(2) || previewBody["batch_limit"] != float64(1000) || previewBody["confirmation_required"] != true || previewBody["expires_at"] == nil {
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
	confirm := map[string]any{"operation": "reset_quota_windows", "job_id": id, "confirm": true}
	assertHTTPStatus(t, fixture.request(t, http.MethodPost, base+"/jobs", confirm, cookie, "", true), http.StatusForbidden)
	result := fixture.request(t, http.MethodPost, base+"/jobs", confirm, cookie, csrf, true)
	assertHTTPStatus(t, result, http.StatusAccepted)
	resultBody := decodeResponseObject(t, result)
	if resultBody["status"] != "completed" || resultBody["deleted_count"] != float64(2) {
		t.Fatalf("result=%+v", resultBody)
	}
	windows, err := fixture.store.MemberQuotaWindows(ctx, membership.ID, now)
	if err != nil || len(windows) != 2 || !windows[0].From.IsZero() || !windows[1].From.IsZero() {
		t.Fatalf("HTTP reset did not clear both local windows: %+v err=%v", windows, err)
	}
	repeated := fixture.request(t, http.MethodPost, base+"/jobs", confirm, cookie, csrf, true)
	assertHTTPStatus(t, repeated, http.StatusAccepted)
	fetched := fixture.request(t, http.MethodGet, base+"/jobs/"+id, nil, cookie, "", true)
	assertHTTPStatus(t, fetched, http.StatusOK)
	if !reflect.DeepEqual(resultBody, decodeResponseObject(t, repeated)) || !reflect.DeepEqual(resultBody, decodeResponseObject(t, fetched)) {
		t.Fatal("duplicate/read result differs")
	}
	stale := fixture.request(t, http.MethodPost, base+"/jobs", map[string]any{"operation": "reset_quota_windows", "job_id": nextID, "confirm": true}, cookie, csrf, true)
	assertHTTPStatus(t, stale, http.StatusConflict)
	if code := decodeResponseObject(t, stale)["error"].(map[string]any)["code"]; code != "retention_preview_invalid" {
		t.Fatalf("stale code=%v", code)
	}
}
