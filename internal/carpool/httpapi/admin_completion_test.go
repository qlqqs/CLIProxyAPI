package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestAdministratorUserKeyAndPublicCursorAPI(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	adminCookie, adminCSRF := loginForAdminCompletion(t, fixture, "operator", "administrator-pass")

	created := make([]map[string]any, 0, 3)
	for _, username := range []string{"cursor-charlie", "cursor-alpha", "cursor-bravo"} {
		response := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/users", map[string]any{
			"username": username, "display_name": username, "role": "passenger",
		}, adminCookie, adminCSRF, true)
		assertHTTPStatus(t, response, http.StatusCreated)
		created = append(created, decodeResponseObject(t, response))
	}
	firstUserRef := stringField(t, created[0], "user_ref")
	secondUserRef := stringField(t, created[1], "user_ref")

	update := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/users/"+firstUserRef, map[string]any{
		"default_display_name": "  新展示名  ",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, update, http.StatusOK)
	if got := stringField(t, decodeResponseObject(t, update), "display_name"); got != "新展示名" {
		t.Fatalf("updated display_name = %q", got)
	}
	emptyUpdate := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/users/"+firstUserRef, map[string]any{}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, emptyUpdate, http.StatusUnprocessableEntity)
	roleUpdate := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/users/"+firstUserRef, map[string]any{"role": "carpool_admin"}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, roleUpdate, http.StatusBadRequest)
	storedUser, errUser := fixture.store.GetUserByRef(t.Context(), firstUserRef)
	if errUser != nil || storedUser.Role != domain.UserRolePassenger || storedUser.DefaultDisplayName != "新展示名" {
		t.Fatalf("stored user after updates = (%#v, %v)", storedUser, errUser)
	}

	userRefs := make(map[string]struct{})
	userCursor := ""
	for {
		path := "/carpool/api/v1/admin/users?limit=2"
		if userCursor != "" {
			path += "&cursor=" + url.QueryEscape(userCursor)
		}
		response := fixture.request(t, http.MethodGet, path, nil, adminCookie, "", false)
		assertHTTPStatus(t, response, http.StatusOK)
		body := decodeResponseObject(t, response)
		if body["total"] != float64(4) {
			t.Fatalf("user page total = %#v, want 4", body["total"])
		}
		for _, item := range objectItems(t, body) {
			userRefs[stringField(t, item, "user_ref")] = struct{}{}
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		if strings.Contains(next, firstUserRef) || strings.Contains(next, storedUser.ID) {
			t.Fatalf("public user cursor exposes row identity: %q", next)
		}
		userCursor = next
	}
	if len(userRefs) != 4 {
		t.Fatalf("paginated users = %v", userRefs)
	}
	invalidLimit := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/users?limit=201", nil, adminCookie, "", false)
	assertHTTPStatus(t, invalidLimit, http.StatusUnprocessableEntity)

	carRefs := make([]string, 0, 3)
	for _, name := range []string{"Cursor Car C", "Cursor Car A", "Cursor Car B"} {
		response := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars", map[string]any{"name": name}, adminCookie, adminCSRF, true)
		assertHTTPStatus(t, response, http.StatusCreated)
		carRefs = append(carRefs, stringField(t, decodeResponseObject(t, response), "car_ref"))
	}
	firstCars := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/cars?limit=1", nil, adminCookie, "", false)
	assertHTTPStatus(t, firstCars, http.StatusOK)
	firstCarsBody := decodeResponseObject(t, firstCars)
	carCursor := stringField(t, firstCarsBody, "next_cursor")
	secondCars := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/cars?limit=1&cursor="+url.QueryEscape(carCursor), nil, adminCookie, "", false)
	assertHTTPStatus(t, secondCars, http.StatusOK)
	if firstRef := stringField(t, objectItems(t, firstCarsBody)[0], "car_ref"); firstRef == stringField(t, objectItems(t, decodeResponseObject(t, secondCars))[0], "car_ref") {
		t.Fatalf("car cursor repeated %q", firstRef)
	}
	crossCursor := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/cars?cursor="+url.QueryEscape(userCursor), nil, adminCookie, "", false)
	assertHTTPStatus(t, crossCursor, http.StatusUnprocessableEntity)

	firstOwner, errFirstOwner := fixture.store.GetUserByRef(t.Context(), firstUserRef)
	_, errSecondOwner := fixture.store.GetUserByRef(t.Context(), secondUserRef)
	if errFirstOwner != nil || errSecondOwner != nil {
		t.Fatalf("load key owners errors = (%v, %v)", errFirstOwner, errSecondOwner)
	}
	firstKey, errFirstKey := fixture.store.CreateAPIKey(t.Context(), domain.APIKey{
		KeyID: "AAAAAAAAAAAAAAAA", Token: "cpk_v1_AAAAAAAAAAAAAAAA.first-secret", UserID: firstOwner.ID, Name: "first", SecretDigest: []byte("first-digest"),
	})
	secondKey, errSecondKey := fixture.store.CreateAPIKey(t.Context(), domain.APIKey{
		KeyID: "AQEBAQEBAQEBAQEB", Token: "cpk_v1_AQEBAQEBAQEBAQEB.second-secret", UserID: firstOwner.ID, Name: "second", SecretDigest: []byte("second-digest"),
	})
	if errFirstKey != nil || errSecondKey != nil {
		t.Fatalf("create administrator-visible keys errors = (%v, %v)", errFirstKey, errSecondKey)
	}
	adminKeys := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/users/"+firstUserRef+"/api-keys?limit=1", nil, adminCookie, "", false)
	assertHTTPStatus(t, adminKeys, http.StatusOK)
	adminKeysBody := decodeResponseObject(t, adminKeys)
	if adminKeysBody["total"] != float64(2) || len(objectItems(t, adminKeysBody)) != 1 {
		t.Fatalf("administrator key page = %#v", adminKeysBody)
	}
	if strings.Contains(adminKeys.Body.String(), "first-digest") || strings.Contains(adminKeys.Body.String(), "second-digest") || strings.Contains(adminKeys.Body.String(), "first-secret") || strings.Contains(adminKeys.Body.String(), "second-secret") {
		t.Fatalf("administrator key page exposes credential material: %s", adminKeys.Body.String())
	}
	keyCursor := stringField(t, adminKeysBody, "next_cursor")
	adminKeysNext := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/users/"+firstUserRef+"/api-keys?limit=1&cursor="+url.QueryEscape(keyCursor), nil, adminCookie, "", false)
	assertHTTPStatus(t, adminKeysNext, http.StatusOK)

	wrongOwnerRevoke := fixture.request(t, http.MethodDelete, "/carpool/api/v1/admin/users/"+secondUserRef+"/api-keys/"+firstKey.KeyID, nil, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, wrongOwnerRevoke, http.StatusNotFound)
	stillActive, errStillActive := fixture.store.GetAPIKey(t.Context(), firstKey.KeyID)
	if errStillActive != nil || stillActive.RevokedAt != nil {
		t.Fatalf("wrong-owner revoke changed key = (%#v, %v)", stillActive, errStillActive)
	}
	properRevoke := fixture.request(t, http.MethodDelete, "/carpool/api/v1/admin/users/"+firstUserRef+"/api-keys/"+firstKey.KeyID, nil, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, properRevoke, http.StatusNoContent)
	revoked, errRevoked := fixture.store.GetAPIKey(t.Context(), firstKey.KeyID)
	if errRevoked != nil || revoked.RevokedAt == nil {
		t.Fatalf("proper revoke key = (%#v, %v)", revoked, errRevoked)
	}
	revokeAll := fixture.request(t, http.MethodDelete, "/carpool/api/v1/admin/users/"+firstUserRef+"/api-keys", nil, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, revokeAll, http.StatusNoContent)
	secondRevoked, errSecondRevoked := fixture.store.GetAPIKey(t.Context(), secondKey.KeyID)
	if errSecondRevoked != nil || secondRevoked.RevokedAt == nil {
		t.Fatalf("revoke all key = (%#v, %v)", secondRevoked, errSecondRevoked)
	}
}

func TestAdministratorCarPatchPreservesOmittedFields(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	adminCookie, adminCSRF := loginForAdminCompletion(t, fixture, "operator", "administrator-pass")

	created := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars", map[string]any{
		"name": "原车辆", "description": "保留说明", "seat_limit": 3,
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, created, http.StatusCreated)
	carRef := stringField(t, decodeResponseObject(t, created), "car_ref")

	updated := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/cars/"+carRef, map[string]any{
		"name": "新车辆",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, updated, http.StatusOK)
	updatedBody := decodeResponseObject(t, updated)
	if got := stringField(t, updatedBody, "name"); got != "新车辆" {
		t.Fatalf("patched car name = %q", got)
	}
	if got := stringField(t, updatedBody, "description"); got != "保留说明" {
		t.Fatalf("description after omitted patch = %q", got)
	}
	if got := updatedBody["seat_limit"]; got != float64(3) {
		t.Fatalf("seat_limit after omitted patch = %#v", got)
	}

	clearedSeat := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/cars/"+carRef, map[string]any{
		"seat_limit": nil,
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, clearedSeat, http.StatusOK)
	if got := decodeResponseObject(t, clearedSeat)["seat_limit"]; got != nil {
		t.Fatalf("seat_limit after explicit null = %#v, want nil", got)
	}

	emptyPatch := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/cars/"+carRef, map[string]any{}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, emptyPatch, http.StatusUnprocessableEntity)
	nullDescription := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/cars/"+carRef, map[string]any{
		"description": nil,
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, nullDescription, http.StatusUnprocessableEntity)
}

func TestAdministratorUsageAndAuditCursorAPI(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	adminCookie, adminCSRF := loginForAdminCompletion(t, fixture, "operator", "administrator-pass")
	passengerCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/users", map[string]any{
		"username": "report-passenger", "display_name": "报表乘客", "role": "passenger",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, passengerCreate, http.StatusCreated)
	passengerRef := stringField(t, decodeResponseObject(t, passengerCreate), "user_ref")
	carCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars", map[string]any{"name": "报表车辆"}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, carCreate, http.StatusCreated)
	carRef := stringField(t, decodeResponseObject(t, carCreate), "car_ref")

	passenger, errPassenger := fixture.store.GetUserByRef(t.Context(), passengerRef)
	car, errCar := fixture.store.GetCarByRef(t.Context(), carRef)
	if errPassenger != nil || errCar != nil {
		t.Fatalf("load report entities errors = (%v, %v)", errPassenger, errCar)
	}
	admin, errAdmin := fixture.store.GetUserByNormalizedUsername(t.Context(), "operator")
	if errAdmin != nil {
		t.Fatalf("load report administrator error = %v", errAdmin)
	}
	key, errKey := fixture.store.CreateAPIKey(t.Context(), domain.APIKey{
		KeyID: "AgICAgICAgICAgIC", UserID: passenger.ID, Name: "report", SecretDigest: []byte("report-digest"),
	})
	if errKey != nil {
		t.Fatalf("CreateAPIKey(report) error = %v", errKey)
	}
	membership, errMembership := fixture.store.MoveMembership(t.Context(), domain.MembershipMove{Membership: domain.Membership{
		MemberRef: "mem_AAAAAAAAAAAAAAAA", UserID: passenger.ID, CarID: car.ID,
		DisplayName: "报表乘客", CreatedByUserID: admin.ID,
	}})
	if errMembership != nil {
		t.Fatalf("MoveMembership(report) error = %v", errMembership)
	}
	assignment, errAssignment := fixture.store.MoveAuthAssignment(t.Context(), domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		AccountRef: "acct_AAAAAAAAAAAAAAAA", CarID: car.ID, AuthID: "secret-auth-alice@example.com",
		SafeLabel: "报表账号", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	if errAssignment != nil {
		t.Fatalf("MoveAuthAssignment(report) error = %v", errAssignment)
	}
	now := fixture.now()
	request, errRequest := fixture.store.BeginProxyRequest(t.Context(), domain.ProxyRequest{
		RequestID: "administrator-report-request", UserID: passenger.ID, APIKeyID: key.KeyID,
		CarID: car.ID, MembershipID: membership.ID, MemberRefSnapshot: membership.MemberRef,
		DisplayNameSnapshot: membership.DisplayName, ScopeHash: "scope", ScopeSize: 1,
		SourceFormat: "openai", StartedAt: now, Outcome: domain.RequestOutcomeInProgress,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: assignment.AuthID, AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	if errRequest != nil {
		t.Fatalf("BeginProxyRequest(report) error = %v", errRequest)
	}
	input, output, cached, reasoning, total := int64(20), int64(8), int64(3), int64(2), int64(28)
	if _, errUsage := fixture.store.InsertUsageEvent(t.Context(), domain.UsageEvent{
		EventID: "administrator-report-usage", RequestID: request.RequestID, AuthID: assignment.AuthID,
		Provider: assignment.ProviderSnapshot, UsageKnown: true, InputTokens: &input,
		OutputTokens: &output, CachedTokens: &cached, ReasoningTokens: &reasoning,
		TotalTokens: &total, RequestedAt: now,
	}); errUsage != nil {
		t.Fatalf("InsertUsageEvent(report) error = %v", errUsage)
	}
	if errComplete := fixture.store.CompleteProxyRequest(t.Context(), domain.RequestCompletion{
		RequestID: request.RequestID, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: now,
	}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest(report) error = %v", errComplete)
	}

	from := now.Add(-time.Hour).Format(time.RFC3339Nano)
	to := now.Add(time.Hour).Format(time.RFC3339Nano)
	usagePath := "/carpool/api/v1/admin/usage?from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to) +
		"&group_by=user&car_ref=" + url.QueryEscape(carRef) + "&user_ref=" + url.QueryEscape(passengerRef)
	usage := fixture.request(t, http.MethodGet, usagePath, nil, adminCookie, "", false)
	assertHTTPStatus(t, usage, http.StatusOK)
	usageBody := decodeResponseObject(t, usage)
	if usageBody["period"] != "custom" || usageBody["group_by"] != "user" || usageBody["retention_cutoff"] == nil {
		t.Fatalf("administrator usage envelope = %#v", usageBody)
	}
	rows := objectItems(t, usageBody)
	if len(rows) != 1 || stringField(t, rows[0], "user_ref") != passengerRef || rows[0]["known_cached_tokens"] != float64(3) || rows[0]["known_reasoning_tokens"] != float64(2) {
		t.Fatalf("administrator usage rows = %#v", rows)
	}
	accountUsage := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/usage?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to)+
		"&group_by=account&account_ref="+url.QueryEscape(assignment.AccountRef), nil, adminCookie, "", false)
	assertHTTPStatus(t, accountUsage, http.StatusOK)
	accountRows := objectItems(t, decodeResponseObject(t, accountUsage))
	if len(accountRows) != 1 || stringField(t, accountRows[0], "account_ref") != assignment.AccountRef || accountRows[0]["provider"] != "codex" {
		t.Fatalf("administrator account usage rows = %#v", accountRows)
	}
	if strings.Contains(accountUsage.Body.String(), assignment.AuthID) {
		t.Fatalf("administrator usage exposes AuthID: %s", accountUsage.Body.String())
	}
	nonUTC := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/usage?from="+url.QueryEscape("2026-09-04T12:00:00+08:00")+"&to="+url.QueryEscape(to), nil, adminCookie, "", false)
	assertHTTPStatus(t, nonUTC, http.StatusUnprocessableEntity)
	missingTo := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/usage?from="+url.QueryEscape(from), nil, adminCookie, "", false)
	assertHTTPStatus(t, missingTo, http.StatusUnprocessableEntity)
	tooOld := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/usage?from="+url.QueryEscape(now.Add(-91*24*time.Hour).Format(time.RFC3339Nano))+"&to="+url.QueryEscape(to), nil, adminCookie, "", false)
	assertHTTPStatus(t, tooOld, http.StatusUnprocessableEntity)

	firstAudit := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/audit-events?limit=1", nil, adminCookie, "", false)
	assertHTTPStatus(t, firstAudit, http.StatusOK)
	firstAuditBody := decodeResponseObject(t, firstAudit)
	auditCursor := stringField(t, firstAuditBody, "next_cursor")
	if _, exposed := objectItems(t, firstAuditBody)[0]["id"]; exposed {
		t.Fatalf("audit response exposes internal ID: %#v", firstAuditBody)
	}
	secondAudit := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/audit-events?limit=1&cursor="+url.QueryEscape(auditCursor), nil, adminCookie, "", false)
	assertHTTPStatus(t, secondAudit, http.StatusOK)
	if len(objectItems(t, decodeResponseObject(t, secondAudit))) != 1 {
		t.Fatalf("second audit page = %s", secondAudit.Body.String())
	}
	badAudit := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/audit-events?cursor=not-a-cursor", nil, adminCookie, "", false)
	assertHTTPStatus(t, badAudit, http.StatusUnprocessableEntity)
}

func loginForAdminCompletion(t *testing.T, fixture carpoolHTTPFlowFixture, username, password string) (*http.Cookie, string) {
	t.Helper()
	response := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
		"username": username, "password": password,
	}, nil, "", true)
	assertHTTPStatus(t, response, http.StatusCreated)
	return responseCookie(t, response, sessionCookieName), stringField(t, decodeResponseObject(t, response), "csrf_token")
}

func objectItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	rawItems, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %#v", body["items"])
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("item = %#v", rawItem)
		}
		items = append(items, item)
	}
	return items
}
