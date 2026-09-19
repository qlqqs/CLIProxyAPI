package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	service "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestAccountManagement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	tokenStore := sdkauth.NewFileTokenStore()
	tokenStore.SetBaseDir(dir)
	manager := coreauth.NewManager(tokenStore, nil, nil)
	handler := management.NewHandler(&config.Config{AuthDir: dir}, "", manager)
	now := time.Now()
	api := &API{now: func() time.Time { return now }}
	api.originValidator, _ = NewOriginValidator(nil)
	api.SetAccountManagement(handler)
	identity := service.SessionIdentity{User: domain.User{Role: domain.UserRoleAdmin}, Session: domain.Session{ID: "owner", CSRFToken: "csrf"}}
	engine := gin.New()
	group := engine.Group("/admin", func(c *gin.Context) { c.Set(identityContextKey, identity) }, requireChangedPassword, requireAdmin)
	api.registerAccountManagement(group)
	request := func(method, path, body, contentType string, security bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://test/admin"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		if security {
			req.Header.Set("Origin", "http://test")
			req.Header.Set(csrfHeaderName, "csrf")
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	credential := `{"type":"codex","access_token":"SECRET_ACCESS","id_token":"SECRET_ID","account":"SECRET_ACCOUNT","note":"SECRET_NOTE"}`
	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"provider only", "good.json", `{"type":"codex"}`, 422},
		{"api key only", "good.json", `{"type":"codex","api_key":"secret"}`, 422},
		{"refresh only", "good.json", `{"type":"codex","refresh_token":"secret"}`, 422},
		{"openai api key", "good.json", `{"type":"openai","api_key":"secret"}`, 422},
		{"openai access token", "good.json", `{"type":"openai","access_token":"secret"}`, 422},
		{"other provider", "good.json", `{"type":"claude","access_token":"secret"}`, 422},
		{"invalid", "good.json", `[]`, 422},
		{"trailing", "good.json", credential + `{}`, 422},
		{"traversal", "..%2Fevil.json", credential, 422},
		{"oversized", "good.json", strings.Repeat("x", accountBodyLimit+1), 413},
		{"success", "good.json", credential, 200},
		{"overwrite", "good.json", credential, 409},
		{"uppercase extension", "upper.JSON", credential, 200},
		{"unicode spaces", "%E4%B8%AD%E6%96%87%20account.json", credential, 200},
		{"windows reserved", "CON.json", credential, 422},
		{"platform unsafe", "bad%3Aname.json", credential, 422},
		{"invalid proxy", "proxy-invalid.json&proxy_url=ftp%3A%2F%2Fproxy.example", credential, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertHTTPStatus(t, request("POST", "/auth-files?name="+tc.path, tc.body, "application/json", true), tc.want)
		})
	}
	live, okLive := manager.GetByID("good.json")
	if !okLive {
		t.Fatal("import not registered")
	}
	upstream := httptest.NewRequest("POST", "https://example.test/responses", nil)
	if errPrepare := executor.NewCodexExecutor(&config.Config{}).PrepareRequest(upstream, live); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	if upstream.Header.Get("Authorization") != "Bearer SECRET_ACCESS" {
		t.Fatal("imported credential is unusable by Codex executor")
	}
	// Hidden disabled providers must not be overwritten, even when absent from the original list.
	_, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "hidden.json", FileName: "hidden.json", Provider: "claude", Disabled: true, Status: coreauth.StatusDisabled})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	assertHTTPStatus(t, request("POST", "/auth-files?name=hidden.json", credential, "application/json", true), 409)
	assertHTTPStatus(t, request("PATCH", "/auth-files/status", `{"name":"hidden.json","disabled":false}`, "application/json", true), 403)
	list := request("GET", "/auth-files", "", "application/json", true)
	assertHTTPStatus(t, list, 200)
	for _, marker := range []string{"SECRET_", "account\"", "note\"", "path\"", "id_token", "status_message", "hidden.json"} {
		if strings.Contains(list.Body.String(), marker) {
			t.Fatalf("list leaked %q: %s", marker, list.Body)
		}
	}
	assertHTTPStatus(t, request("PATCH", "/auth-files/status", `{"name":"good.json","auth_index":"wrong","disabled":true}`, "application/json", true), 404)
	assertHTTPStatus(t, request("PATCH", "/auth-files/status", `{"name":"good.json","disabled":true}`, "application/json", true), 200)
	persisted, errRead := os.ReadFile(filepath.Join(dir, "good.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var metadata map[string]any
	if json.Unmarshal(persisted, &metadata) != nil || metadata["disabled"] != true {
		t.Fatalf("disable not persisted: %s", persisted)
	}
	for _, id := range []string{"duplicate-one", "duplicate-two"} {
		if _, errDuplicate := manager.Register(context.Background(), &coreauth.Auth{ID: id, FileName: "ambiguous.json", Provider: "codex", Status: coreauth.StatusActive}); errDuplicate != nil {
			t.Fatal(errDuplicate)
		}
	}
	assertHTTPStatus(t, request("PATCH", "/auth-files/status", `{"name":"ambiguous.json","disabled":true}`, "application/json", true), 409)
	// A failed store save must not publish the requested enabled runtime state.
	manager.SetStore(failingAccountStore{})
	assertHTTPStatus(t, request("PATCH", "/auth-files/status", `{"name":"good.json","disabled":false}`, "application/json", true), 500)
	failedLive, okFailed := manager.GetByID("good.json")
	if !okFailed || !failedLive.Disabled || failedLive.Metadata["disabled"] != true {
		t.Fatal("failed persistence changed runtime")
	}
	afterFailure, errAfter := os.ReadFile(filepath.Join(dir, "good.json"))
	if errAfter != nil || !bytes.Equal(afterFailure, persisted) {
		t.Fatal("failed persistence changed disk")
	}
	manager.SetStore(tokenStore)
	// Disabled runtime/config entries remain visible without exposing API keys.
	_, errRuntime := manager.Register(context.Background(), &coreauth.Auth{ID: "codex:apikey:test", Provider: "codex", Disabled: true, Status: coreauth.StatusDisabled, Attributes: map[string]string{"api_key": "SECRET_CONFIG_KEY", "source": "config:codex[test]"}})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	runtimeList := request("GET", "/auth-files", "", "application/json", false)
	assertHTTPStatus(t, runtimeList, 200)
	if !strings.Contains(runtimeList.Body.String(), "codex:apikey:test") || strings.Contains(runtimeList.Body.String(), "SECRET_CONFIG_KEY") {
		t.Fatalf("unsafe or missing runtime entry: %s", runtimeList.Body)
	}
	// A single bounded multipart JSON file is accepted; paths are not silently reduced to basenames.
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	part, errPart := writer.CreateFormFile("files", "multipart.json")
	if errPart != nil {
		t.Fatal(errPart)
	}
	if _, errWrite := part.Write([]byte(credential)); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errField := writer.WriteField("proxy_url", "socks5://proxy.example:1080"); errField != nil {
		t.Fatal(errField)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	assertHTTPStatus(t, request("POST", "/auth-files", multipartBody.String(), writer.FormDataContentType(), true), 200)
	multipartAuth, okMultipart := manager.GetByID("multipart.json")
	if !okMultipart || multipartAuth.ProxyURL != "socks5://proxy.example:1080" {
		t.Fatalf("multipart proxy URL = %q, found=%t", multipartAuth.ProxyURL, okMultipart)
	}
	for _, route := range []struct{ method, path, body string }{{"GET", "/auth-files", ""}, {"POST", "/auth-files?name=x.json", credential}, {"PATCH", "/auth-files/status", `{"name":"good.json","disabled":false}`}, {"POST", "/auth-files/test", `{"name":"good.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`}, {"POST", "/codex-auth-url", ""}, {"GET", "/get-auth-status?state=foreign", ""}, {"POST", "/oauth-callback", `{"state":"foreign","code":"secret"}`}} {
		if route.method != "GET" {
			assertHTTPStatus(t, request(route.method, route.path, route.body, "application/json", false), 403)
		}
		if route.method != "GET" {
			for _, missing := range []string{"Origin", csrfHeaderName} {
				req := httptest.NewRequest(route.method, "http://test/admin"+route.path, strings.NewReader(route.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", "http://test")
				req.Header.Set(csrfHeaderName, "csrf")
				req.Header.Del(missing)
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, req)
				assertHTTPStatus(t, rec, 403)
			}
		}
		identity.User.Role = domain.UserRolePassenger
		assertHTTPStatus(t, request(route.method, route.path, route.body, "application/json", true), 403)
		identity.User.Role = domain.UserRoleAdmin
		identity.User.MustChangePassword = true
		assertHTTPStatus(t, request(route.method, route.path, route.body, "application/json", true), 403)
		identity.User.MustChangePassword = false
	}
	for _, tc := range []struct {
		name  string
		files int
		want  int
	}{{"../escape.json", 1, 422}, {"multiple.json", 2, 422}, {"single-alias.json", 1, 200}} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for i := 0; i < tc.files; i++ {
			part, errPart := writer.CreateFormFile("file", tc.name)
			if errPart != nil {
				t.Fatal(errPart)
			}
			if _, errWrite := part.Write([]byte(credential)); errWrite != nil {
				t.Fatal(errWrite)
			}
		}
		if errClose := writer.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		assertHTTPStatus(t, request("POST", "/auth-files", body.String(), writer.FormDataContentType(), true), tc.want)
	}
	assertHTTPStatus(t, request("POST", "/codex-auth-url", `{"proxy_url":"ftp://proxy.example"}`, "application/json", true), 422)
	// The real original start handler creates a session-bound manual OAuth URL.
	start := request("POST", "/codex-auth-url", "{}", "application/json", true)
	assertHTTPStatus(t, start, 200)
	var started map[string]string
	if errDecode := json.Unmarshal(start.Body.Bytes(), &started); errDecode != nil {
		t.Fatal(errDecode)
	}
	if started["state"] == "" || !strings.HasPrefix(started["url"], "https://") {
		t.Fatalf("invalid OAuth start response: %s", start.Body)
	}
	assertHTTPStatus(t, request("GET", "/get-auth-status?state="+started["state"], "", "application/json", false), 200)
	management.CompleteOAuthSession(started["state"])
	completed := request("GET", "/get-auth-status?state="+started["state"], "", "application/json", false)
	if !strings.Contains(completed.Body.String(), `"ok"`) {
		t.Fatal("completion not reflected")
	}
	// Polling and callbacks cannot borrow another login's global state.
	management.RegisterOAuthSession("owned-state", "codex")
	t.Cleanup(func() { management.CompleteOAuthSession("owned-state") })
	api.accounts.states = map[string]ownedOAuth{"owned-state": {session: "owner", expires: now.Add(time.Minute)}}
	assertHTTPStatus(t, request("GET", "/get-auth-status?state=owned-state", "", "application/json", true), 200)
	assertHTTPStatus(t, request("GET", "/get-auth-status", "", "application/json", true), 404)
	identity.Session.ID = "other"
	assertHTTPStatus(t, request("GET", "/get-auth-status?state=owned-state", "", "application/json", true), 404)
	assertHTTPStatus(t, request("POST", "/oauth-callback", `{"state":"owned-state","code":"secret"}`, "application/json", true), 404)
	identity.Session.ID = "owner"
	assertHTTPStatus(t, request("POST", "/oauth-callback", `{"provider":"claude","state":"owned-state","code":"secret"}`, "application/json", true), 422)
	assertHTTPStatus(t, request("POST", "/oauth-callback", `{"provider":"codex","state":"owned-state","code":"secret"}`, "application/json", true), 200)
	poll := request("GET", "/get-auth-status?state=owned-state", "", "application/json", true)
	if !strings.Contains(poll.Body.String(), `"wait"`) {
		t.Fatalf("staging must not complete: %s", poll.Body)
	}
	management.SetOAuthSessionError("owned-state", "SECRET_UPSTREAM_ERROR")
	poll = request("GET", "/get-auth-status?state=owned-state", "", "application/json", true)
	if strings.Contains(poll.Body.String(), "SECRET_") {
		t.Fatal("upstream error leaked")
	}
	now = now.Add(2 * time.Minute)
	assertHTTPStatus(t, request("GET", "/get-auth-status?state=owned-state", "", "application/json", true), 404)
}

func TestAccountRoutesRequireSession(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	for _, route := range []struct{ method, path string }{{"GET", "auth-files"}, {"POST", "auth-files"}, {"PATCH", "auth-files/status"}, {"POST", "auth-files/test"}, {"POST", "codex-auth-url"}, {"GET", "get-auth-status"}, {"POST", "oauth-callback"}} {
		assertHTTPStatus(t, fixture.request(t, route.method, "/carpool/api/v1/admin/"+route.path, nil, nil, "", true), http.StatusUnauthorized)
	}
}

type failingAccountStore struct{}

func (failingAccountStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (failingAccountStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", errors.New("synthetic storage failure")
}
func (failingAccountStore) Delete(context.Context, string) error { return nil }

type accountTestExecutor struct {
	mu       sync.Mutex
	authID   string
	authIDs  []string
	request  coreexecutor.Request
	options  coreexecutor.Options
	response coreexecutor.Response
	err      error
	started  chan struct{}
}

func (*accountTestExecutor) Identifier() string { return "codex" }
func (e *accountTestExecutor) Execute(ctx context.Context, auth *coreauth.Auth, request coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	if auth != nil {
		e.authID = auth.ID
		e.authIDs = append(e.authIDs, auth.ID)
	}
	e.request = request
	e.options = options
	started := e.started
	response := e.response
	err := e.err
	e.mu.Unlock()
	if started != nil {
		close(started)
		<-ctx.Done()
		return coreexecutor.Response{}, ctx.Err()
	}
	return response, err
}
func (*accountTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("unexpected stream execution")
}
func (*accountTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*accountTestExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("unexpected count execution")
}
func (*accountTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected HTTP execution")
}

func TestAccountConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newFixture := func(t *testing.T, executeErr error) (*gin.Engine, *accountTestExecutor, *service.SessionIdentity, func(string, string, bool) *httptest.ResponseRecorder) {
		t.Helper()
		manager := coreauth.NewManager(nil, nil, nil)
		executor := &accountTestExecutor{response: coreexecutor.Response{Payload: []byte(`{"id":"resp_test","status":"completed","output":[],"error":null}`)}, err: executeErr}
		manager.RegisterExecutor(executor)
		for _, auth := range []*coreauth.Auth{
			{ID: "target-auth", FileName: "target.json", Provider: "codex", Status: coreauth.StatusActive},
			{ID: "other-auth", FileName: "other.json", Provider: "codex", Status: coreauth.StatusActive},
			{ID: "disabled-auth", FileName: "disabled.json", Provider: "codex", Status: coreauth.StatusDisabled, Disabled: true},
			{ID: "ambiguous-a", FileName: "ambiguous.json", Provider: "codex", Status: coreauth.StatusActive},
			{ID: "ambiguous-b", FileName: "ambiguous.json", Provider: "codex", Status: coreauth.StatusActive},
		} {
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
		}
		pluginAuth := &coreauth.Auth{ID: "plugin-auth", FileName: "plugin.json", Provider: "codex", Status: coreauth.StatusActive}
		coreauth.MarkPluginVirtualAuth(pluginAuth, "source.json", 0)
		if _, errRegister := manager.Register(context.Background(), pluginAuth); errRegister != nil {
			t.Fatal(errRegister)
		}
		for _, authID := range []string{"target-auth", "other-auth", "disabled-auth", "ambiguous-a", "ambiguous-b", "plugin-auth"} {
			registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4", Object: "model", OwnedBy: "openai"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		}
		handler := management.NewHandler(&config.Config{}, "", manager)
		api := &API{}
		api.originValidator, _ = NewOriginValidator(nil)
		api.SetAccountManagement(handler)
		identity := &service.SessionIdentity{User: domain.User{Role: domain.UserRoleAdmin}, Session: domain.Session{ID: "owner", CSRFToken: "csrf"}}
		engine := gin.New()
		group := engine.Group("/admin", func(c *gin.Context) { c.Set(identityContextKey, *identity) }, requireChangedPassword, requireAdmin)
		api.registerAccountManagement(group)
		request := func(body, csrf string, origin bool) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "http://test/admin/auth-files/test", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if origin {
				req.Header.Set("Origin", "http://test")
			}
			if csrf != "" {
				req.Header.Set(csrfHeaderName, csrf)
			}
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			return rec
		}
		return engine, executor, identity, request
	}

	t.Run("locks exact account and returns safe metadata", func(t *testing.T) {
		_, executor, _, request := newFixture(t, nil)
		// An empty index is valid when the filename already identifies one runtime auth.
		body := `{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"请只回复 OK。"}`
		response := request(body, "csrf", true)
		assertHTTPStatus(t, response, http.StatusOK)
		if strings.Contains(response.Body.String(), "resp_test") || strings.Contains(response.Body.String(), "OK") {
			t.Fatalf("response leaked upstream content: %s", response.Body)
		}
		var result map[string]any
		if errDecode := json.Unmarshal(response.Body.Bytes(), &result); errDecode != nil {
			t.Fatal(errDecode)
		}
		if result["status"] != "ok" || result["model"] != "gpt-5.4" || result["response_bytes"].(float64) <= 0 {
			t.Fatalf("unexpected result: %v", result)
		}
		executor.mu.Lock()
		defer executor.mu.Unlock()
		if executor.authID != "target-auth" || executor.request.Model != "gpt-5.4" || executor.request.Format != sdktranslator.FormatOpenAIResponse {
			t.Fatalf("unexpected execution target: auth=%q request=%+v", executor.authID, executor.request)
		}
		if !executor.options.CredentialScope.Allows("target-auth") || executor.options.CredentialScope.Allows("other-auth") || executor.options.Metadata[coreexecutor.PinnedAuthMetadataKey] != "target-auth" {
			t.Fatalf("execution was not pinned: %+v", executor.options)
		}
		if executor.options.SourceFormat != sdktranslator.FormatOpenAIResponse || executor.options.ResponseFormat != sdktranslator.FormatOpenAIResponse {
			t.Fatalf("unexpected formats: %+v", executor.options)
		}
	})

	t.Run("rejects invalid disabled missing and ambiguous targets", func(t *testing.T) {
		_, _, _, request := newFixture(t, nil)
		cases := []struct {
			body string
			want int
		}{
			{`{"name":"disabled.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`, http.StatusForbidden},
			{`{"name":"missing.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`, http.StatusNotFound},
			{`{"name":"ambiguous.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`, http.StatusConflict},
			{`{"name":"plugin.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`, http.StatusForbidden},
			{`{"name":"target.json","auth_index":"wrong","model":"gpt-5.4","prompt":"OK"}`, http.StatusNotFound},
			{`{"name":"target.json","auth_index":"","model":"","prompt":"OK"}`, http.StatusUnprocessableEntity},
			{`{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":null}`, http.StatusUnprocessableEntity},
			{`{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"OK","token":"secret"}`, http.StatusUnprocessableEntity},
			{`{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}` + strings.Repeat(" ", accountTestBodyLimit), http.StatusUnprocessableEntity},
		}
		for _, tc := range cases {
			assertHTTPStatus(t, request(tc.body, "csrf", true), tc.want)
		}
	})

	t.Run("rejects error-shaped successful payload", func(t *testing.T) {
		// Replace the default response through the executor returned by the fixture.
		_, executor, _, request := newFixture(t, nil)
		executor.response = coreexecutor.Response{Payload: []byte(`{"error":{"message":"SECRET_TOKEN raw upstream body"}}`)}
		response := request(`{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`, "csrf", true)
		assertHTTPStatus(t, response, http.StatusBadGateway)
		if strings.Contains(response.Body.String(), "SECRET_TOKEN") || strings.Contains(response.Body.String(), "raw upstream") {
			t.Fatalf("unsafe response: %s", response.Body)
		}
	})

	t.Run("requires origin csrf admin and changed password", func(t *testing.T) {
		_, _, identity, request := newFixture(t, nil)
		body := `{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`
		assertHTTPStatus(t, request(body, "csrf", false), http.StatusForbidden)
		assertHTTPStatus(t, request(body, "", true), http.StatusForbidden)
		identity.User.Role = domain.UserRolePassenger
		assertHTTPStatus(t, request(body, "csrf", true), http.StatusForbidden)
		identity.User.Role = domain.UserRoleAdmin
		identity.User.MustChangePassword = true
		assertHTTPStatus(t, request(body, "csrf", true), http.StatusForbidden)
	})

	t.Run("propagates client cancellation", func(t *testing.T) {
		engine, executor, _, _ := newFixture(t, nil)
		executor.mu.Lock()
		executor.started = make(chan struct{})
		started := executor.started
		executor.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodPost, "http://test/admin/auth-files/test", strings.NewReader(`{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://test")
		req.Header.Set(csrfHeaderName, "csrf")
		done := make(chan struct{})
		go func() {
			engine.ServeHTTP(httptest.NewRecorder(), req)
			close(done)
		}()
		<-started
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("canceled request did not return")
		}
	})

	t.Run("classifies upstream errors without leaking them", func(t *testing.T) {
		for _, tc := range []struct {
			upstreamStatus int
			responseStatus int
			message        string
		}{
			{http.StatusUnauthorized, http.StatusUnauthorized, "账号授权已失效"},
			{http.StatusForbidden, http.StatusForbidden, "上游拒绝访问"},
			{http.StatusTooManyRequests, http.StatusTooManyRequests, "额度或速率受限"},
			{http.StatusServiceUnavailable, http.StatusBadGateway, "上游暂不可用"},
		} {
			_, executor, _, request := newFixture(t, &coreauth.Error{HTTPStatus: tc.upstreamStatus, Message: "SECRET_TOKEN raw upstream body"})
			response := request(`{"name":"target.json","auth_index":"","model":"gpt-5.4","prompt":"OK"}`, "csrf", true)
			assertHTTPStatus(t, response, tc.responseStatus)
			if strings.Contains(response.Body.String(), "SECRET_TOKEN") || strings.Contains(response.Body.String(), "raw upstream") {
				t.Fatalf("unsafe error response: %s", response.Body)
			}
			if !strings.Contains(response.Body.String(), tc.message) {
				t.Fatalf("missing safe classification %q: %s", tc.message, response.Body)
			}
			executor.mu.Lock()
			for _, authID := range executor.authIDs {
				if authID != "target-auth" {
					executor.mu.Unlock()
					t.Fatalf("execution fell back to %q: %v", authID, executor.authIDs)
				}
			}
			executor.mu.Unlock()
		}
	})
}
