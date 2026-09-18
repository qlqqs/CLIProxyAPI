package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRootResponsesRouteParity(t *testing.T) {
	server := newTestServer(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/responses"},
		{http.MethodPost, "/responses"},
		{http.MethodPost, "/responses/compact"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			registered := make(map[string]string)
			for _, info := range server.engine.Routes() {
				if info.Method == route.method {
					registered[info.Path] = info.Handler
				}
			}
			if registered[route.path] == "" || registered[route.path] != registered["/v1"+route.path] {
				t.Fatalf("root and versioned handlers differ: %q / %q", registered[route.path], registered["/v1"+route.path])
			}
			for _, key := range []string{"", "invalid-key", "test-key"} {
				var baseline *httptest.ResponseRecorder
				for _, prefix := range []string{"/v1", ""} {
					request := httptest.NewRequest(route.method, prefix+route.path, strings.NewReader(`{"model":`))
					if key != "" {
						request.Header.Set("Authorization", "Bearer "+key)
					}
					response := httptest.NewRecorder()
					server.engine.ServeHTTP(response, request)
					if key != "test-key" && response.Code != http.StatusUnauthorized {
						t.Fatalf("unauthenticated %s: status=%d", prefix+route.path, response.Code)
					}
					if key == "test-key" && response.Code != http.StatusBadRequest {
						t.Fatalf("authenticated %s did not reach handler: status=%d body=%s", prefix+route.path, response.Code, response.Body.String())
					}
					if baseline != nil && (response.Code != baseline.Code || response.Body.String() != baseline.Body.String()) {
						t.Fatalf("root/versioned response mismatch: %d %s vs %d %s", response.Code, response.Body.String(), baseline.Code, baseline.Body.String())
					}
					baseline = response
				}
			}
		})
	}
}

func TestRootResponsesExampleKeySafeMode(t *testing.T) {
	server := newTestServerWithOptions(t, WithExampleAPIKeySafeMode())
	cfg := *server.cfg
	cfg.APIKeys = []string{"your-api-key-1"}
	server.UpdateClients(&cfg)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/responses"},
		{http.MethodPost, "/responses"},
		{http.MethodPost, "/responses/compact"},
	} {
		request := httptest.NewRequest(route.method, route.path, nil)
		request.Header.Set("Authorization", "Bearer your-api-key-1")
		response := httptest.NewRecorder()
		server.engine.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || response.Header().Get("X-CPA-SAFE-MODE") != "example-api-key" {
			t.Fatalf("safe mode bypassed on %s %s: %d %s", route.method, route.path, response.Code, response.Body.String())
		}
	}
}
