package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type fakeChatGPTDriver struct {
	session  *sdkAuth.ChatGPTLoginSession
	startErr error
	loginErr error

	gotHint     string
	gotDeviceID string
	gotProxy    string
	gotPasted   string
}

func (f *fakeChatGPTDriver) StartLogin(_ *config.Config, agentNameHint, deviceID, proxyURL string) (*sdkAuth.ChatGPTLoginSession, error) {
	f.gotHint = agentNameHint
	f.gotDeviceID = deviceID
	f.gotProxy = proxyURL
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.session, nil
}

func (f *fakeChatGPTDriver) CompleteLogin(_ context.Context, _ *config.Config, _ *sdkAuth.ChatGPTLoginSession, pastedCallbackURL string) (*coreauth.Auth, error) {
	f.gotPasted = pastedCallbackURL
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	fileName := "chatgpt-0123456789ab.json"
	storage := &chatgpt.ChatGPTTokenStorage{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ClientID:     "oaiapp_issued",
		Scopes:       []string{"openid", chatgpt.DirectTokenScope},
		LastRefresh:  "2026-10-01T00:00:00Z",
		Expire:       "2026-10-01T01:00:00Z",
	}
	return &coreauth.Auth{
		ID:       fileName,
		Provider: chatgpt.ProviderName,
		FileName: fileName,
		Storage:  storage,
		Metadata: map[string]any{"type": chatgpt.ProviderName},
	}, nil
}

func withFakeChatGPTDriver(t *testing.T, driver chatgptLoginDriver) {
	t.Helper()
	previous := newChatGPTLoginDriver
	newChatGPTLoginDriver = func() chatgptLoginDriver { return driver }
	t.Cleanup(func() { newChatGPTLoginDriver = previous })
}

func newChatGPTRouter(t *testing.T, cfg *config.Config) *gin.Engine {
	t.Helper()
	handler := NewHandlerWithoutConfigFilePath(cfg, nil)
	router := gin.New()
	router.GET("/v0/management/chatgpt-auth-url", handler.RequestChatGPTToken)
	router.POST("/v0/management/chatgpt-auth-callback", handler.PostChatGPTCallback)
	return router
}

func TestRequestChatGPTTokenReturnsAuthorizeURL(t *testing.T) {
	driver := &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{
		AuthorizeURL: chatgpt.AuthorizeURL + "?client_id=dynamic_agent_client",
		State:        "state-1",
		CodeVerifier: "verifier-1",
	}}
	withFakeChatGPTDriver(t, driver)

	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})
	req := httptest.NewRequest(http.MethodGet, "/v0/management/chatgpt-auth-url?proxy_url=http://127.0.0.1:8080&agent_name_hint=Pi&device_id=af0a2098-3f5b-4a0e-9c1d-7e2b4c6d8f01", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["url"] != driver.session.AuthorizeURL {
		t.Fatalf("url = %v", payload["url"])
	}
	if payload["state"] != "state-1" || payload["provider"] != chatgpt.ProviderName {
		t.Fatalf("payload = %v", payload)
	}
	if payload["redirect_uri"] != chatgpt.RedirectURI {
		t.Fatalf("redirect_uri = %v", payload["redirect_uri"])
	}
	if !strings.Contains(payload["instructions"].(string), chatgpt.RedirectURI) {
		t.Fatalf("instructions = %v", payload["instructions"])
	}
	if driver.gotProxy != "http://127.0.0.1:8080" || driver.gotHint != "Pi" || driver.gotDeviceID == "" {
		t.Fatalf("driver args = %q/%q/%q", driver.gotProxy, driver.gotHint, driver.gotDeviceID)
	}

	session, ok := chatgptPendingLogins.take("state-1")
	if !ok || session == nil || session.CodeVerifier != "verifier-1" {
		t.Fatalf("pending login not stored: %+v ok=%v", session, ok)
	}
}

func TestRequestChatGPTTokenRejectsInvalidProxy(t *testing.T) {
	withFakeChatGPTDriver(t, &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{State: "s"}})
	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})

	req := httptest.NewRequest(http.MethodGet, "/v0/management/chatgpt-auth-url?proxy_url=://bad", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestRequestChatGPTTokenSurfacesStartFailure(t *testing.T) {
	withFakeChatGPTDriver(t, &fakeChatGPTDriver{startErr: errors.New("boom")})
	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/management/chatgpt-auth-url", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestPostChatGPTCallbackSavesCredential(t *testing.T) {
	authDir := t.TempDir()
	driver := &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{State: "state-2"}}
	withFakeChatGPTDriver(t, driver)
	chatgptPendingLogins.put(driver.session)

	router := newChatGPTRouter(t, &config.Config{AuthDir: authDir})
	pasted := chatgpt.RedirectURI + "?code=the-code&state=state-2&client_id=oaiapp_issued"
	body := `{"redirect_url":"` + pasted + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/chatgpt-auth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if driver.gotPasted != pasted {
		t.Fatalf("pasted = %q", driver.gotPasted)
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fileName, _ := payload["file_name"].(string)
	if fileName != "chatgpt-0123456789ab.json" {
		t.Fatalf("file_name = %v", payload["file_name"])
	}

	raw, err := os.ReadFile(filepath.Join(authDir, fileName))
	if err != nil {
		t.Fatalf("credential file: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal credential: %v", err)
	}
	if stored["type"] != chatgpt.ProviderName {
		t.Fatalf("stored type = %v", stored["type"])
	}
	if stored["client_id"] != "oaiapp_issued" || stored["access_token"] != "access-token" {
		t.Fatalf("stored credential = %v", stored)
	}

	// The paste is single-use: the pending login was consumed.
	if _, ok := chatgptPendingLogins.take("state-2"); ok {
		t.Fatal("pending login should have been consumed")
	}
}

func TestPostChatGPTCallbackRequiresState(t *testing.T) {
	withFakeChatGPTDriver(t, &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{State: "state-3"}})
	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})

	body := `{"redirect_url":"http://127.0.0.1:1455/auth/callback?code=c"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/chatgpt-auth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestPostChatGPTCallbackUnknownState(t *testing.T) {
	withFakeChatGPTDriver(t, &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{State: "state-4"}})
	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})

	body := `{"state":"never-started","redirect_url":"http://127.0.0.1:1455/auth/callback?code=c&state=never-started"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/chatgpt-auth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestPostChatGPTCallbackRejectsOversizedPaste(t *testing.T) {
	withFakeChatGPTDriver(t, &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{State: "state-5"}})
	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})

	body := `{"state":"state-5","redirect_url":"` + strings.Repeat("a", chatgptMaxPastedCallbackLength+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/chatgpt-auth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestPostChatGPTCallbackMapsFlowErrors(t *testing.T) {
	driver := &fakeChatGPTDriver{session: &sdkAuth.ChatGPTLoginSession{State: "state-6"}, loginErr: chatgpt.ErrStateMismatch}
	withFakeChatGPTDriver(t, driver)
	chatgptPendingLogins.put(driver.session)

	router := newChatGPTRouter(t, &config.Config{AuthDir: t.TempDir()})
	body := `{"state":"state-6","redirect_url":"http://127.0.0.1:1455/auth/callback?code=c&state=other"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/chatgpt-auth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "state mismatch") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestChatGPTCallbackErrorStatus(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
	}{
		{chatgpt.ErrStateMismatch, http.StatusBadRequest},
		{chatgpt.ErrCallbackURLShape, http.StatusBadRequest},
		{chatgpt.ErrMissingCode, http.StatusBadRequest},
		{chatgpt.ErrMissingState, http.StatusBadRequest},
		{chatgpt.ErrMissingClientID, http.StatusBadRequest},
		{chatgpt.ErrAuthorizationFailed, http.StatusBadRequest},
		{chatgpt.ErrDeviceIDRequired, http.StatusBadRequest},
		{errors.New("token endpoint exploded"), http.StatusBadGateway},
	}
	for _, test := range tests {
		status, message := chatgptCallbackErrorStatus(test.err)
		if status != test.wantStatus {
			t.Fatalf("status for %v = %d, want %d", test.err, status, test.wantStatus)
		}
		if strings.TrimSpace(message) == "" {
			t.Fatalf("empty message for %v", test.err)
		}
	}
}

func TestChatGPTLoginStorePurgesExpiredSessions(t *testing.T) {
	store := &chatgptLoginStore{sessions: make(map[string]chatgptPendingLogin)}
	store.sessions["stale"] = chatgptPendingLogin{
		session:   &sdkAuth.ChatGPTLoginSession{State: "stale"},
		createdAt: time.Now().Add(-2 * chatgptLoginSessionTTL),
	}
	store.put(&sdkAuth.ChatGPTLoginSession{State: "fresh"})

	if _, ok := store.take("stale"); ok {
		t.Fatal("expired session must be purged")
	}
	if _, ok := store.take("fresh"); !ok {
		t.Fatal("fresh session must survive")
	}
}

func TestChatGPTStateFromRedirectURL(t *testing.T) {
	if got := chatgptStateFromRedirectURL(chatgpt.RedirectURI + "?code=c&state=abc"); got != "abc" {
		t.Fatalf("state = %q", got)
	}
	if got := chatgptStateFromRedirectURL("not a url"); got != "" && strings.Contains(got, " ") {
		t.Fatalf("state = %q", got)
	}
	if got := chatgptStateFromRedirectURL(""); got != "" {
		t.Fatalf("state = %q", got)
	}
}
