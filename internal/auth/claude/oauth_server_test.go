package claude

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOAuthCallbackRejectsUnrelatedRequestsWithoutEndingWaiter(t *testing.T) {
	const expectedState = "synthetic-expected-state"
	tests := []struct {
		name, query string
		method      string
		want        int
	}{
		{"missing error state", "error=access_denied", http.MethodGet, http.StatusBadRequest},
		{"wrong error state", "state=wrong&error=access_denied", http.MethodGet, http.StatusBadRequest},
		{"missing success state", "code=synthetic-code", http.MethodGet, http.StatusBadRequest},
		{"wrong success state", "state=wrong&code=synthetic-code", http.MethodGet, http.StatusBadRequest},
		{"duplicate state", "state=" + expectedState + "&state=wrong&code=synthetic-code", http.MethodGet, http.StatusBadRequest},
		{"missing code", "state=" + expectedState, http.MethodGet, http.StatusBadRequest},
		{"wrong method", "state=" + expectedState + "&error=access_denied", http.MethodPost, http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := NewOAuthServer(0, expectedState)
			type callbackResult struct {
				result *OAuthResult
				err    error
			}
			done := make(chan callbackResult, 1)
			go func() { result, err := server.WaitForCallback(5 * time.Second); done <- callbackResult{result, err} }()
			response := httptest.NewRecorder()
			server.handleCallback(response, httptest.NewRequest(tc.method, "/callback?"+tc.query, nil))
			if response.Code != tc.want {
				t.Errorf("unrelated callback status %d, want %d", response.Code, tc.want)
			}
			select {
			case result := <-done:
				t.Fatalf("unrelated callback ended waiter: %#v", result)
			default:
			}
			valid := httptest.NewRecorder()
			server.handleCallback(valid, httptest.NewRequest(http.MethodGet, "/callback?state="+expectedState+"&code=accepted-code", nil))
			if valid.Code != http.StatusFound || valid.Header().Get("Location") != "/success" {
				t.Errorf("valid callback status = %d", valid.Code)
			}
			select {
			case result := <-done:
				if result.err != nil || result.result == nil || result.result.State != expectedState || result.result.Code != "accepted-code" || result.result.Error != "" {
					t.Fatalf("valid callback did not finish waiter correctly: %#v", result)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("valid callback did not complete")
			}
		})
	}
}

func TestOAuthCallbackAcceptsMatchingStateError(t *testing.T) {
	server := NewOAuthServer(0, "expected-state")
	response := httptest.NewRecorder()
	server.handleCallback(response, httptest.NewRequest(http.MethodGet, "/callback?state=expected-state&error=access_denied", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d", response.Code)
	}
	result, err := server.WaitForCallback(time.Second)
	if err != nil || result.State != "expected-state" || result.Error != "access_denied" || result.Code != "" {
		t.Fatalf("matching state error was lost: result=%#v, err=%v", result, err)
	}
}

func TestOAuthCallbackRequiresConfiguredState(t *testing.T) {
	server := NewOAuthServer(0, "")
	if err := server.Start(); err == nil {
		_ = server.Stop(context.Background())
		t.Fatal("server started without a state")
	}
	response := httptest.NewRecorder()
	server.handleCallback(response, httptest.NewRequest(http.MethodGet, "/callback?error=access_denied", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d", response.Code)
	}
	select {
	case <-server.resultChan:
		t.Fatal("callback with unconfigured state was accepted")
	default:
	}
}

func TestOAuthServerBindsLoopbackAndServesSuccessfulFlow(t *testing.T) {
	server := NewOAuthServer(0, "expected-state")
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	host, _, err := net.SplitHostPort(server.server.Addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("listener address = %q", server.server.Addr)
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	response, err := client.Get("http://" + server.server.Addr + "/callback?state=expected-state&code=synthetic-code")
	if err != nil {
		t.Fatal(err)
	}
	body, errRead := io.ReadAll(response.Body)
	if errClose := response.Body.Close(); errClose != nil {
		t.Error(errClose)
	}
	if errRead != nil {
		t.Fatal(errRead)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `href="https://console.anthropic.com/"`) {
		t.Fatalf("success page unusable: status %d", response.StatusCode)
	}
	result, err := server.WaitForCallback(time.Second)
	if err != nil || result.Code != "synthetic-code" {
		t.Fatalf("callback = %#v, %v", result, err)
	}
}

func TestOAuthSuccessPageRejectsUntrustedPlatformURLs(t *testing.T) {
	server := NewOAuthServer(0, "expected-state")
	for _, raw := range []string{
		`"><script>window.reviewMarker=1</script><a href="`,
		"javascript:alert(1)", "data:text/html,test", "//evil.example/path", "https://evil.example/", "https://user@console.anthropic.com/", "http://console.anthropic.com/", "https://console.anthropic.com.evil.example/",
	} {
		for _, setupRequired := range []bool{false, true} {
			response := httptest.NewRecorder()
			query := url.Values{"platform_url": {raw}}
			if setupRequired {
				query.Set("setup_required", "true")
			}
			server.handleSuccess(response, httptest.NewRequest(http.MethodGet, "/success?"+query.Encode(), nil))
			body := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(body, `href="https://console.anthropic.com/"`) {
				t.Fatalf("invalid URL did not fall back safely: %q", raw)
			}
			if strings.Contains(body, "window.reviewMarker") || strings.Contains(body, "javascript:") || strings.Contains(body, "evil.example") {
				t.Fatalf("unsafe URL reached HTML: %q", raw)
			}
			if strings.Contains(body, "{{") {
				t.Fatal("unresolved success template placeholders")
			}
			if setupRequired && !strings.Contains(body, "Additional Setup Required") {
				t.Fatal("setup notice lost")
			}
		}
	}
	raw := `https://console.anthropic.com/settings?next=" onmouseover="alert(1)&a=b`
	page := server.generateSuccessHTML(true, raw)
	if strings.Contains(page, `next="`) || strings.Contains(page, ` onmouseover="`) || !strings.Contains(page, "https://console.anthropic.com/settings?next=") || !strings.Contains(page, "&amp;a=b") {
		t.Fatal("allowed platform URL was not HTML-escaped")
	}
}
