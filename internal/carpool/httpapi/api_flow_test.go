package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	flowOrigin     = "https://carpool.test"
	flowLoginBurst = 5
)

type carpoolHTTPFlowFixture struct {
	engine   *gin.Engine
	store    *carpoolsqlite.Store
	provider *carpoolaccess.Provider
	now      func() time.Time
}

func newCarpoolHTTPFlowFixture(t *testing.T) carpoolHTTPFlowFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }
	store, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{
		Path: filepath.Join(t.TempDir(), "carpool.db"),
		Now:  nowFunc,
	})
	if errOpen != nil {
		t.Fatalf("sqlite.Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Store.Close() error = %v", errClose)
		}
	})

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "secret-auth-alice@example.com", Provider: "codex", Status: coreauth.StatusActive, UpdatedAt: now},
		{ID: "/sensitive/path/claude.json", Provider: "claude", Status: coreauth.StatusActive, UpdatedAt: now},
	} {
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("AuthManager.Register(%q) error = %v", auth.ID, errRegister)
		}
	}
	control, errControl := carpoolservice.NewControl(store, manager, carpoolservice.ControlConfig{
		SessionAbsoluteTTL: 24 * time.Hour,
		SessionIdleTTL:     2 * time.Hour,
		ReportLocation:     time.UTC,
		UsageRetention:     90 * 24 * time.Hour,
		Now:                nowFunc,
		CandidateRefKey:    bytes.Repeat([]byte{9}, 32),
		PasswordHasher: carpoolservice.PasswordHasher{Params: carpoolservice.PasswordParams{
			Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
		}},
	})
	if errControl != nil {
		t.Fatalf("service.NewControl() error = %v", errControl)
	}
	if _, errBootstrap := control.BootstrapAdmin(ctx, "operator", "拼车管理员", "administrator-pass"); errBootstrap != nil {
		t.Fatalf("BootstrapAdmin() error = %v", errBootstrap)
	}
	api, errAPI := New(control, Config{SessionTTL: 24 * time.Hour, Now: nowFunc})
	if errAPI != nil {
		t.Fatalf("httpapi.New() error = %v", errAPI)
	}
	engine := gin.New()
	api.RegisterRoutes(engine)
	return carpoolHTTPFlowFixture{engine: engine, store: store, provider: carpoolaccess.NewProvider(store, nowFunc), now: nowFunc}
}

func TestCarpoolHTTPAdministratorAndPassengerFlow(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)

	missingOrigin := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
		"username": "operator", "password": "administrator-pass",
	}, nil, "", false)
	assertHTTPStatus(t, missingOrigin, http.StatusForbidden)

	adminLogin := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
		"username": "operator", "password": "administrator-pass",
	}, nil, "", true)
	assertHTTPStatus(t, adminLogin, http.StatusCreated)
	adminCookie := responseCookie(t, adminLogin, sessionCookieName)
	adminSession := decodeResponseObject(t, adminLogin)
	adminCSRF := stringField(t, adminSession, "csrf_token")
	if adminCookie.HttpOnly != true || adminCookie.SameSite != http.SameSiteStrictMode || adminCookie.Path != "/carpool/" {
		t.Fatalf("admin session cookie = %#v", adminCookie)
	}

	withoutCSRF := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/users", map[string]any{
		"username": "rider", "display_name": "乘客", "role": "passenger",
	}, adminCookie, "", true)
	assertHTTPStatus(t, withoutCSRF, http.StatusForbidden)

	passengerCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/users", map[string]any{
		"username": "rider", "display_name": "乘客", "role": "passenger",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, passengerCreate, http.StatusCreated)
	passengerBody := decodeResponseObject(t, passengerCreate)
	passengerRef := stringField(t, passengerBody, "user_ref")
	temporaryPassword := stringField(t, passengerBody, "temporary_password")

	secondCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/users", map[string]any{
		"username": "second-rider", "display_name": "第二位乘客", "role": "passenger",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, secondCreate, http.StatusCreated)
	secondBody := decodeResponseObject(t, secondCreate)

	passengerLogin := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
		"username": "rider", "password": temporaryPassword,
	}, nil, "", true)
	assertHTTPStatus(t, passengerLogin, http.StatusCreated)
	passengerCookie := responseCookie(t, passengerLogin, sessionCookieName)
	passengerSession := decodeResponseObject(t, passengerLogin)
	passengerCSRF := stringField(t, passengerSession, "csrf_token")
	if mustChange, _ := passengerSession["must_change_password"].(bool); !mustChange {
		t.Fatalf("temporary-password session = %#v", passengerSession)
	}
	blockedBeforeChange := fixture.request(t, http.MethodGet, "/carpool/api/v1/me/car", nil, passengerCookie, "", false)
	assertHTTPStatus(t, blockedBeforeChange, http.StatusForbidden)

	passwordChange := fixture.request(t, http.MethodPost, "/carpool/api/v1/me/password", map[string]any{
		"current_password": temporaryPassword, "new_password": "new-passenger-password",
	}, passengerCookie, passengerCSRF, true)
	assertHTTPStatus(t, passwordChange, http.StatusNoContent)
	oldSession := fixture.request(t, http.MethodGet, "/carpool/api/v1/session", nil, passengerCookie, "", false)
	assertHTTPStatus(t, oldSession, http.StatusUnauthorized)

	passengerLogin = fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
		"username": "rider", "password": "new-passenger-password",
	}, nil, "", true)
	assertHTTPStatus(t, passengerLogin, http.StatusCreated)
	passengerCookie = responseCookie(t, passengerLogin, sessionCookieName)
	passengerSession = decodeResponseObject(t, passengerLogin)
	passengerCSRF = stringField(t, passengerSession, "csrf_token")
	passengerAdminAttempt := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/users", nil, passengerCookie, "", false)
	assertHTTPStatus(t, passengerAdminAttempt, http.StatusForbidden)
	adminPassengerAttempt := fixture.request(t, http.MethodGet, "/carpool/api/v1/me/api-keys", nil, adminCookie, "", false)
	assertHTTPStatus(t, adminPassengerAttempt, http.StatusForbidden)

	carCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars", map[string]any{
		"name": "第一辆车", "description": "集成测试", "seat_limit": 1,
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, carCreate, http.StatusCreated)
	carRef := stringField(t, decodeResponseObject(t, carCreate), "car_ref")

	memberCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars/"+carRef+"/members", map[string]any{
		"user_ref": passengerRef, "display_name": "一号乘客", "monthly_limit_usd": "25.00",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, memberCreate, http.StatusCreated)
	overCapacity := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars/"+carRef+"/members", map[string]any{
		"user_ref": stringField(t, secondBody, "user_ref"), "display_name": "二号乘客", "monthly_limit_usd": "25.00",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, overCapacity, http.StatusConflict)

	accountList := fixture.request(t, http.MethodGet, "/carpool/api/v1/admin/cars/"+carRef+"/accounts", nil, adminCookie, "", false)
	assertHTTPStatus(t, accountList, http.StatusOK)
	for _, secret := range []string{"secret-auth-alice@example.com", "/sensitive/path/claude.json"} {
		if strings.Contains(accountList.Body.String(), secret) {
			t.Fatalf("account candidate response exposes AuthID %q: %s", secret, accountList.Body.String())
		}
	}
	accountBody := decodeResponseObject(t, accountList)
	candidates, ok := accountBody["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("account candidates = %#v", accountBody["candidates"])
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		t.Fatalf("first candidate = %#v", candidates[0])
	}
	candidateRef := stringField(t, candidate, "candidate_ref")
	if !strings.HasPrefix(candidateRef, "cand_v1_") {
		t.Fatalf("candidate_ref = %q", candidateRef)
	}
	rawAuthAttempt := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars/"+carRef+"/accounts", map[string]any{
		"auth_id": "secret-auth-alice@example.com", "safe_label": "账号 A",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, rawAuthAttempt, http.StatusBadRequest)
	accountMove := fixture.request(t, http.MethodPost, "/carpool/api/v1/admin/cars/"+carRef+"/accounts", map[string]any{
		"candidate_ref": candidateRef, "safe_label": "账号 A",
	}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, accountMove, http.StatusCreated)
	if strings.Contains(accountMove.Body.String(), "secret-auth-alice@example.com") || strings.Contains(accountMove.Body.String(), "/sensitive/path/claude.json") {
		t.Fatalf("account assignment response exposes AuthID: %s", accountMove.Body.String())
	}

	firstKeyCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/me/api-keys", map[string]any{"name": "laptop"}, passengerCookie, passengerCSRF, true)
	assertHTTPStatus(t, firstKeyCreate, http.StatusCreated)
	firstKeyBody := decodeResponseObject(t, firstKeyCreate)
	firstToken := stringField(t, firstKeyBody, "api_key")
	firstKeyRef := stringField(t, firstKeyBody, "key_ref")
	secondKeyCreate := fixture.request(t, http.MethodPost, "/carpool/api/v1/me/api-keys", map[string]any{"name": "server"}, passengerCookie, passengerCSRF, true)
	assertHTTPStatus(t, secondKeyCreate, http.StatusCreated)
	secondKeyBody := decodeResponseObject(t, secondKeyCreate)
	secondToken := stringField(t, secondKeyBody, "api_key")

	keyList := fixture.request(t, http.MethodGet, "/carpool/api/v1/me/api-keys", nil, passengerCookie, "", false)
	assertHTTPStatus(t, keyList, http.StatusOK)
	if strings.Contains(keyList.Body.String(), firstToken) || strings.Contains(keyList.Body.String(), secondToken) {
		t.Fatalf("API key list exposes a token: %s", keyList.Body.String())
	}
	assertProviderAuthentication(t, fixture.provider, firstToken, true)
	assertProviderAuthentication(t, fixture.provider, secondToken, true)

	revokeFirst := fixture.request(t, http.MethodDelete, "/carpool/api/v1/me/api-keys/"+firstKeyRef, nil, passengerCookie, passengerCSRF, true)
	assertHTTPStatus(t, revokeFirst, http.StatusNoContent)
	assertProviderAuthentication(t, fixture.provider, firstToken, false)
	assertProviderAuthentication(t, fixture.provider, secondToken, true)

	for _, path := range []string{
		"/carpool/api/v1/me/car",
		"/carpool/api/v1/me/members/usage?period=today",
		"/carpool/api/v1/me/accounts?period=7d",
	} {
		response := fixture.request(t, http.MethodGet, path, nil, passengerCookie, "", false)
		assertHTTPStatus(t, response, http.StatusOK)
		for _, secret := range []string{"secret-auth-alice@example.com", "/sensitive/path/claude.json", firstToken, secondToken} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("passenger response %s exposes secret %q: %s", path, secret, response.Body.String())
			}
		}
	}

	disablePassenger := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/users/"+passengerRef, map[string]any{"status": "disabled"}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, disablePassenger, http.StatusOK)
	disabledSession := fixture.request(t, http.MethodGet, "/carpool/api/v1/session", nil, passengerCookie, "", false)
	assertHTTPStatus(t, disabledSession, http.StatusUnauthorized)
	assertProviderAuthentication(t, fixture.provider, secondToken, false)

	enablePassenger := fixture.request(t, http.MethodPatch, "/carpool/api/v1/admin/users/"+passengerRef, map[string]any{"status": "active"}, adminCookie, adminCSRF, true)
	assertHTTPStatus(t, enablePassenger, http.StatusOK)
	assertProviderAuthentication(t, fixture.provider, secondToken, false)
}

func TestAPIKeyResponseUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	api := &API{now: func() time.Time { return now }}
	expiresAt := now
	response := api.apiKeyResponse(domain.APIKey{KeyID: "key", ExpiresAt: &expiresAt})
	if response["status"] != "expired" {
		t.Fatalf("status = %#v, want expired", response["status"])
	}
}

func TestLoginRateLimitReturnsRetryable429(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	for attempt := 0; attempt < flowLoginBurst; attempt++ {
		response := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
			"username": "missing-user", "password": "incorrect-password",
		}, nil, "", true)
		assertHTTPStatus(t, response, http.StatusUnauthorized)
	}
	limited := fixture.request(t, http.MethodPost, "/carpool/api/v1/session", map[string]any{
		"username": "missing-user", "password": "incorrect-password",
	}, nil, "", true)
	assertHTTPStatus(t, limited, http.StatusTooManyRequests)
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("rate-limited response is missing Retry-After")
	}
	body := decodeResponseObject(t, limited)
	errorBody, ok := body["error"].(map[string]any)
	if !ok || errorBody["code"] != "rate_limited" || errorBody["request_id"] == "" {
		t.Fatalf("rate-limited response = %#v", body)
	}
}

func (f carpoolHTTPFlowFixture) request(t *testing.T, method, path string, body any, cookie *http.Cookie, csrf string, origin bool) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var errMarshal error
		encoded, errMarshal = json.Marshal(body)
		if errMarshal != nil {
			t.Fatalf("json.Marshal() error = %v", errMarshal)
		}
	}
	request := httptest.NewRequest(method, flowOrigin+path, bytes.NewReader(encoded))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin {
		request.Header.Set("Origin", flowOrigin)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if csrf != "" {
		request.Header.Set(csrfHeaderName, csrf)
	}
	response := httptest.NewRecorder()
	f.engine.ServeHTTP(response, request)
	return response
}

func assertHTTPStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, want, response.Body.String())
	}
}

func responseCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response does not contain cookie %q", name)
	return nil
}

func decodeResponseObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &value); errDecode != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", response.Body.String(), errDecode)
	}
	return value
}

func stringField(t *testing.T, value map[string]any, name string) string {
	t.Helper()
	field, ok := value[name].(string)
	if !ok || field == "" {
		t.Fatalf("field %q = %#v", name, value[name])
	}
	return field
}

func assertProviderAuthentication(t *testing.T, provider *carpoolaccess.Provider, token string, want bool) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	result, authErr := provider.Authenticate(request.Context(), request)
	if want && (authErr != nil || result == nil) {
		t.Fatalf("provider authentication failed: result=%#v error=%v", result, authErr)
	}
	if !want && authErr == nil {
		t.Fatalf("provider authentication unexpectedly succeeded: result=%#v", result)
	}
}
