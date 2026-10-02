package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDeviceID = "af0a2098-3f5b-4a0e-9c1d-7e2b4c6d8f01"

func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	encode := func(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
	return encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".sig"
}

func TestGenerateAuthURLFieldOrderMatchesPi(t *testing.T) {
	auth := &ChatGPTAuth{authorizeURL: AuthorizeURL}
	hostID, err := AgentHostID(testDeviceID)
	if err != nil {
		t.Fatalf("AgentHostID: %v", err)
	}
	pkce := &PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge-value"}

	raw, err := auth.GenerateAuthURL("state-value", pkce, hostID, DefaultAgentNameHint)
	if err != nil {
		t.Fatalf("GenerateAuthURL: %v", err)
	}
	if !strings.HasPrefix(raw, AuthorizeURL+"?") {
		t.Fatalf("unexpected endpoint: %s", raw)
	}

	pairs := strings.Split(strings.TrimPrefix(raw, AuthorizeURL+"?"), "&")
	keys := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		key, _, _ := strings.Cut(pair, "=")
		keys = append(keys, key)
	}
	wantOrder := []string{
		"client_id",
		"agent_name_hint",
		"ext_agent_host_id",
		"response_type",
		"redirect_uri",
		"resource",
		"scope",
		"state",
		"code_challenge",
		"code_challenge_method",
		"nonce",
	}
	if strings.Join(keys, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("field order mismatch:\n got %v\nwant %v", keys, wantOrder)
	}

	wantPrefix := "client_id=dynamic_agent_client" +
		"&agent_name_hint=Pi" +
		"&ext_agent_host_id=urn%3Auuid%3A" + testDeviceID +
		"&response_type=code" +
		"&redirect_uri=http%3A%2F%2F127.0.0.1%3A1455%2Fauth%2Fcallback" +
		"&resource=https%3A%2F%2Fapi.openai.com%2Fv1" +
		"&scope=openid+profile+email+offline_access+resource.invoke+chatgpt.tokens.use.direct" +
		"&state=state-value" +
		"&code_challenge=challenge-value" +
		"&code_challenge_method=S256" +
		"&nonce="
	if !strings.HasPrefix(raw, AuthorizeURL+"?"+wantPrefix) {
		t.Fatalf("authorization URL mismatch:\n got %s\nwant prefix %s", raw, wantPrefix)
	}
	nonce := strings.TrimPrefix(raw, AuthorizeURL+"?"+wantPrefix)
	// pi's randomValue(): 32 bytes as unpadded base64url.
	if len(nonce) != 43 {
		t.Fatalf("nonce length = %d, want 43 (%q)", len(nonce), nonce)
	}
	if strings.ContainsAny(nonce, "=+/") {
		t.Fatalf("nonce must be unpadded url-safe base64: %q", nonce)
	}
}

func TestGenerateAuthURLRequiresPKCE(t *testing.T) {
	auth := &ChatGPTAuth{authorizeURL: AuthorizeURL}
	if _, err := auth.GenerateAuthURL("state", nil, "urn:uuid:x", "Pi"); err == nil {
		t.Fatal("expected error for missing PKCE codes")
	}
}

func TestParseManualCallback(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{
			name:  "valid",
			input: "http://127.0.0.1:1455/auth/callback?code=abc&state=st&client_id=oaiapp_123",
		},
		{
			name:    "wrong host",
			input:   "http://localhost:1455/auth/callback?code=abc&state=st&client_id=oaiapp_123",
			wantErr: ErrCallbackURLShape,
		},
		{
			name:    "wrong path",
			input:   "http://127.0.0.1:1455/other?code=abc&state=st&client_id=oaiapp_123",
			wantErr: ErrCallbackURLShape,
		},
		{
			name:    "not a url",
			input:   "abc123",
			wantErr: ErrCallbackURLShape,
		},
		{
			name:    "empty",
			input:   "   ",
			wantErr: ErrCallbackURLShape,
		},
		{
			name:    "state mismatch",
			input:   "http://127.0.0.1:1455/auth/callback?code=abc&state=other&client_id=oaiapp_123",
			wantErr: ErrStateMismatch,
		},
		{
			name:    "missing code",
			input:   "http://127.0.0.1:1455/auth/callback?state=st&client_id=oaiapp_123",
			wantErr: ErrMissingCode,
		},
		{
			name:    "missing client id",
			input:   "http://127.0.0.1:1455/auth/callback?code=abc&state=st",
			wantErr: ErrMissingClientID,
		},
		{
			name:    "authorization error",
			input:   "http://127.0.0.1:1455/auth/callback?error=access_denied&state=st",
			wantErr: ErrAuthorizationFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ParseManualCallback(test.input, "st")
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.Code != "abc" || result.ClientID != "oaiapp_123" {
					t.Fatalf("unexpected result: %+v", result)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestParseManualCallbackRejectsEmptyStateExpectation(t *testing.T) {
	_, err := ParseManualCallback("http://127.0.0.1:1455/auth/callback?code=abc&state=st&client_id=x", "  ")
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("error = %v, want %v", err, ErrStateMismatch)
	}
}

func newTestTokenServer(t *testing.T, response string, status int, capture *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			body := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				if _, err := r.Body.Read(body); err != nil && err.Error() != "EOF" {
					t.Errorf("read body: %v", err)
				}
			}
			*capture = string(body)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestExchangeAuthorizationCodeSendsPiFormFieldOrder(t *testing.T) {
	idToken := fakeJWT(t, map[string]any{"email": "user@example.com"})
	response, err := json.Marshal(map[string]any{
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"id_token":      idToken,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"scope":         "openid profile email offline_access " + DirectTokenScope,
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var captured string
	server := newTestTokenServer(t, string(response), http.StatusOK, &captured)

	auth := &ChatGPTAuth{httpClient: server.Client(), tokenURL: server.URL}
	credential, err := auth.ExchangeAuthorizationCode(context.Background(), "the-code", "the-verifier", "oaiapp_issued")
	if err != nil {
		t.Fatalf("ExchangeAuthorizationCode: %v", err)
	}

	wantBody := "grant_type=authorization_code" +
		"&client_id=oaiapp_issued" +
		"&code=the-code" +
		"&code_verifier=the-verifier" +
		"&redirect_uri=http%3A%2F%2F127.0.0.1%3A1455%2Fauth%2Fcallback" +
		"&resource=https%3A%2F%2Fapi.openai.com%2Fv1"
	if captured != wantBody {
		t.Fatalf("request body mismatch:\n got %s\nwant %s", captured, wantBody)
	}
	if credential.AccessToken != "access-token" || credential.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected credential: %+v", credential)
	}
	if credential.ClientID != "oaiapp_issued" {
		t.Fatalf("client id = %q", credential.ClientID)
	}
	if len(credential.Scopes) != 5 {
		t.Fatalf("scopes = %v", credential.Scopes)
	}
	if got := credential.ExpiresAt(time.Unix(0, 0)); !got.Equal(time.Unix(3600, 0).Add(-ExpiryMargin)) {
		t.Fatalf("ExpiresAt = %s", got)
	}
	if got := EmailFromIDToken(credential.IDToken); got != "user@example.com" {
		t.Fatalf("email = %q", got)
	}
}

func TestRefreshAccessTokenSendsPiFormFieldOrder(t *testing.T) {
	response := `{"access_token":"a2","refresh_token":"r2","id_token":"` + fakeJWT(t, map[string]any{"sub": "s"}) + `","expires_in":1800,"scope":"openid ` + DirectTokenScope + `"}`
	var captured string
	server := newTestTokenServer(t, response, http.StatusOK, &captured)

	auth := &ChatGPTAuth{httpClient: server.Client(), tokenURL: server.URL}
	credential, err := auth.RefreshAccessToken(context.Background(), "r1", "oaiapp_issued")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}

	wantBody := "grant_type=refresh_token" +
		"&client_id=oaiapp_issued" +
		"&refresh_token=r1" +
		"&resource=https%3A%2F%2Fapi.openai.com%2Fv1"
	if captured != wantBody {
		t.Fatalf("request body mismatch:\n got %s\nwant %s", captured, wantBody)
	}
	if credential.AccessToken != "a2" || credential.ExpiresIn != 1800 {
		t.Fatalf("unexpected credential: %+v", credential)
	}
}

func TestRefreshAccessTokenRequiresIssuedClientID(t *testing.T) {
	auth := &ChatGPTAuth{}
	if _, err := auth.RefreshAccessToken(context.Background(), "r1", "  "); !errors.Is(err, ErrMissingClientID) {
		t.Fatalf("error = %v, want %v", err, ErrMissingClientID)
	}
	if _, err := auth.RefreshAccessToken(context.Background(), "", "client"); !errors.Is(err, ErrRefreshTokenRequired) {
		t.Fatalf("error = %v, want %v", err, ErrRefreshTokenRequired)
	}
}

func TestExchangeTokenResponseValidation(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  string
	}{
		{
			name:     "missing id token",
			response: `{"access_token":"a","refresh_token":"r","expires_in":600,"scope":"openid ` + DirectTokenScope + `"}`,
			wantErr:  "did not contain an ID token",
		},
		{
			name:     "missing direct scope",
			response: `{"access_token":"a","refresh_token":"r","id_token":"i","expires_in":600,"scope":"openid profile email"}`,
			wantErr:  DirectTokenScope,
		},
		{
			name:     "zero expires_in",
			response: `{"access_token":"a","refresh_token":"r","id_token":"i","expires_in":0,"scope":"` + DirectTokenScope + `"}`,
			wantErr:  "invalid expires_in",
		},
		{
			name:     "missing refresh token",
			response: `{"access_token":"a","id_token":"i","expires_in":600,"scope":"` + DirectTokenScope + `"}`,
			wantErr:  "invalid refresh_token",
		},
		{
			name:     "missing scope",
			response: `{"access_token":"a","refresh_token":"r","id_token":"i","expires_in":600}`,
			wantErr:  "invalid scope",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newTestTokenServer(t, test.response, http.StatusOK, nil)
			auth := &ChatGPTAuth{httpClient: server.Client(), tokenURL: server.URL}
			_, err := auth.ExchangeAuthorizationCode(context.Background(), "code", "verifier", "client")
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestTokenEndpointErrorIncludesStatusAndBody(t *testing.T) {
	server := newTestTokenServer(t, `{"error":"invalid_grant"}`, http.StatusBadRequest, nil)
	auth := &ChatGPTAuth{httpClient: server.Client(), tokenURL: server.URL}
	_, err := auth.ExchangeAuthorizationCode(context.Background(), "code", "verifier", "client")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentHostID(t *testing.T) {
	got, err := AgentHostID(strings.ToUpper(testDeviceID))
	if err != nil {
		t.Fatalf("AgentHostID: %v", err)
	}
	if got != "urn:uuid:"+testDeviceID {
		t.Fatalf("AgentHostID = %q", got)
	}
	if _, err := AgentHostID("not-a-uuid"); !errors.Is(err, ErrDeviceIDRequired) {
		t.Fatalf("error = %v, want %v", err, ErrDeviceIDRequired)
	}
}

func TestLoadOrCreateDeviceIDIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "device-id")
	first, err := LoadOrCreateDeviceID(path)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceID: %v", err)
	}
	if !uuidPattern.MatchString(first) {
		t.Fatalf("generated id is not a uuid: %q", first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	second, err := LoadOrCreateDeviceID(path)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceID (second): %v", err)
	}
	if second != first {
		t.Fatalf("device id changed: %q -> %q", first, second)
	}
}

func TestSubjectHashAndCredentialFileName(t *testing.T) {
	token := fakeJWT(t, map[string]any{"sub": "user-123"})
	hash := SubjectHash(token)
	if hash == "" || len(hash) != 12 {
		t.Fatalf("SubjectHash = %q", hash)
	}
	if SubjectHash(token) != hash {
		t.Fatal("SubjectHash must be stable")
	}
	if SubjectHash(fakeJWT(t, map[string]any{"sub": "user-456"})) == hash {
		t.Fatal("different subjects must hash differently")
	}
	if SubjectHash("garbage") != "" {
		t.Fatal("unparsable token must yield empty hash")
	}
	if got := CredentialFileName(hash); got != "chatgpt-"+hash+".json" {
		t.Fatalf("CredentialFileName = %q", got)
	}
	if got := CredentialFileName(""); got != "chatgpt-unknown.json" {
		t.Fatalf("CredentialFileName(empty) = %q", got)
	}
}

func TestTokenStorageRoundTrip(t *testing.T) {
	subject := fakeJWT(t, map[string]any{"sub": "user-123"})
	storage := &ChatGPTTokenStorage{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ClientID:     "oaiapp_issued",
		Scopes:       []string{"openid", DirectTokenScope},
		LastRefresh:  "2026-10-01T00:00:00Z",
		Expire:       "2026-10-01T01:00:00Z",
	}
	storage.SetMetadata(map[string]any{"disabled": false})

	path := filepath.Join(t.TempDir(), CredentialFileName(SubjectHash(subject)))
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["type"] != ProviderName {
		t.Fatalf("type = %v", decoded["type"])
	}
	for key, want := range map[string]any{
		"access_token":  "access",
		"refresh_token": "refresh",
		"client_id":     "oaiapp_issued",
		"last_refresh":  "2026-10-01T00:00:00Z",
		"expired":       "2026-10-01T01:00:00Z",
		"disabled":      false,
	} {
		if got := decoded[key]; got != want {
			t.Fatalf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestGeneratePKCECodes(t *testing.T) {
	first, err := GeneratePKCECodes()
	if err != nil {
		t.Fatalf("GeneratePKCECodes: %v", err)
	}
	second, err := GeneratePKCECodes()
	if err != nil {
		t.Fatalf("GeneratePKCECodes: %v", err)
	}
	if first.CodeVerifier == second.CodeVerifier {
		t.Fatal("verifiers must differ")
	}
	if len(first.CodeVerifier) != 128 {
		t.Fatalf("verifier length = %d", len(first.CodeVerifier))
	}
	// S256 challenge is the base64url SHA-256 of the verifier.
	if got, want := generateCodeChallenge(first.CodeVerifier), first.CodeChallenge; got != want {
		t.Fatalf("challenge = %q, want %q", want, got)
	}
}
