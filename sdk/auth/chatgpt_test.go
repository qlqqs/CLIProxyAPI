package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type fakeChatGPTService struct {
	credential  *chatgpt.Credential
	exchangeErr error

	gotState    string
	gotHostID   string
	gotHint     string
	gotCode     string
	gotVerifier string
	gotClientID string
}

func (f *fakeChatGPTService) GenerateAuthURL(state string, _ *chatgpt.PKCECodes, hostID, agentNameHint string) (string, error) {
	f.gotState = state
	f.gotHostID = hostID
	f.gotHint = agentNameHint
	return chatgpt.AuthorizeURL + "?client_id=dynamic_agent_client&state=" + state, nil
}

func (f *fakeChatGPTService) ExchangeAuthorizationCode(_ context.Context, code, verifier, clientID string) (*chatgpt.Credential, error) {
	f.gotCode = code
	f.gotVerifier = verifier
	f.gotClientID = clientID
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return f.credential, nil
}

func fakeAccessToken(t *testing.T, subject string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"sub": subject})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newFakeAuthenticator(t *testing.T, service *fakeChatGPTService) *ChatGPTAuthenticator {
	t.Helper()
	authenticator := NewChatGPTAuthenticator()
	authenticator.newAuthService = func(*config.Config, string) chatGPTAuthService { return service }
	return authenticator
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{AuthDir: t.TempDir()}
}

func TestChatGPTAuthenticatorProviderAndRefreshLead(t *testing.T) {
	authenticator := NewChatGPTAuthenticator()
	if got := authenticator.Provider(); got != chatgpt.ProviderName {
		t.Fatalf("Provider = %q", got)
	}
	lead := authenticator.RefreshLead()
	if lead == nil || *lead <= 0 {
		t.Fatalf("RefreshLead = %v", lead)
	}
}

func TestChatGPTStartLoginProducesPiAuthorizeURL(t *testing.T) {
	cfg := testConfig(t)
	service := &fakeChatGPTService{}
	authenticator := newFakeAuthenticator(t, service)

	session, err := authenticator.StartLogin(cfg, "", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if session.State == "" || session.CodeVerifier == "" {
		t.Fatalf("incomplete session: %+v", session)
	}
	if !strings.HasPrefix(session.AuthorizeURL, chatgpt.AuthorizeURL+"?") {
		t.Fatalf("authorize URL = %q", session.AuthorizeURL)
	}
	if service.gotState != session.State {
		t.Fatalf("state passed to service = %q, want %q", service.gotState, session.State)
	}

	// The device id is created inside the auth dir and reused on later logins.
	deviceIDPath := filepath.Join(cfg.AuthDir, chatgptDeviceIDFileName)
	raw, err := os.ReadFile(deviceIDPath)
	if err != nil {
		t.Fatalf("device id file: %v", err)
	}
	first := strings.TrimSpace(string(raw))
	if got := strings.TrimPrefix(service.gotHostID, "urn:uuid:"); got != first {
		t.Fatalf("agent host id = %q, want urn:uuid:%s", service.gotHostID, first)
	}
	if service.gotHint != "" {
		t.Fatalf("agent name hint = %q, want empty (package default)", service.gotHint)
	}

	if _, err = authenticator.StartLogin(cfg, "", "", ""); err != nil {
		t.Fatalf("StartLogin (second): %v", err)
	}
	raw, err = os.ReadFile(deviceIDPath)
	if err != nil {
		t.Fatalf("device id file (second): %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != first {
		t.Fatalf("device id changed: %q -> %q", first, got)
	}
}

func TestChatGPTStartLoginHonoursMetadataOverrides(t *testing.T) {
	cfg := testConfig(t)
	service := &fakeChatGPTService{}
	authenticator := newFakeAuthenticator(t, service)

	deviceID := "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	if _, err := authenticator.StartLogin(cfg, "CustomAgent", deviceID, ""); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if service.gotHint != "CustomAgent" {
		t.Fatalf("agent name hint = %q", service.gotHint)
	}
	if service.gotHostID != "urn:uuid:"+deviceID {
		t.Fatalf("agent host id = %q", service.gotHostID)
	}
	// An explicit device id must not create the dotfile.
	if _, err := os.Stat(filepath.Join(cfg.AuthDir, chatgptDeviceIDFileName)); !os.IsNotExist(err) {
		t.Fatalf("device id file should not exist, err = %v", err)
	}
}

func TestChatGPTCompleteLoginBuildsAuthRecord(t *testing.T) {
	cfg := testConfig(t)
	service := &fakeChatGPTService{credential: &chatgpt.Credential{
		AccessToken:  fakeAccessToken(t, "user-123"),
		RefreshToken: "refresh",
		ClientID:     "oaiapp_issued",
		Scopes:       []string{"openid", chatgpt.DirectTokenScope},
		ExpiresIn:    3600,
	}}
	authenticator := newFakeAuthenticator(t, service)

	session, err := authenticator.StartLogin(cfg, "", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	pasted := chatgpt.RedirectURI + "?code=the-code&state=" + session.State + "&client_id=oaiapp_issued"
	record, err := authenticator.CompleteLogin(context.Background(), cfg, session, pasted)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}

	if record.Provider != chatgpt.ProviderName {
		t.Fatalf("provider = %q", record.Provider)
	}
	if !strings.HasPrefix(record.FileName, chatgpt.ProviderName+"-") || !strings.HasSuffix(record.FileName, ".json") {
		t.Fatalf("file name = %q", record.FileName)
	}
	if record.ID != record.FileName {
		t.Fatalf("id %q != file name %q", record.ID, record.FileName)
	}
	if record.Metadata["access_token"] != service.credential.AccessToken {
		t.Fatalf("metadata access_token = %v", record.Metadata["access_token"])
	}
	if record.Metadata["client_id"] != "oaiapp_issued" {
		t.Fatalf("metadata client_id = %v", record.Metadata["client_id"])
	}
	if record.Metadata["type"] != chatgpt.ProviderName {
		t.Fatalf("metadata type = %v", record.Metadata["type"])
	}
	if record.Attributes["base_url"] != chatgpt.DefaultBaseURL {
		t.Fatalf("base_url = %q", record.Attributes["base_url"])
	}
	if service.gotCode != "the-code" || service.gotVerifier != session.CodeVerifier || service.gotClientID != "oaiapp_issued" {
		t.Fatalf("exchange args = %q/%q/%q", service.gotCode, service.gotVerifier, service.gotClientID)
	}

	storage, ok := record.Storage.(*chatgpt.ChatGPTTokenStorage)
	if !ok {
		t.Fatalf("storage type = %T", record.Storage)
	}
	if storage.ClientID != "oaiapp_issued" || storage.AccessToken != service.credential.AccessToken {
		t.Fatalf("unexpected storage: %+v", storage)
	}
	if storage.Expire == "" || storage.LastRefresh == "" {
		t.Fatalf("timestamps not set: %+v", storage)
	}
}

func TestChatGPTCompleteLoginRejectsMismatchedState(t *testing.T) {
	cfg := testConfig(t)
	authenticator := newFakeAuthenticator(t, &fakeChatGPTService{})

	session, err := authenticator.StartLogin(cfg, "", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	pasted := chatgpt.RedirectURI + "?code=the-code&state=someone-elses-state&client_id=oaiapp_issued"
	_, err = authenticator.CompleteLogin(context.Background(), cfg, session, pasted)
	if !errors.Is(err, chatgpt.ErrStateMismatch) {
		t.Fatalf("error = %v, want %v", err, chatgpt.ErrStateMismatch)
	}
}

func TestChatGPTCompleteLoginRejectsCredentialWithoutClientID(t *testing.T) {
	cfg := testConfig(t)
	service := &fakeChatGPTService{credential: &chatgpt.Credential{AccessToken: "a", RefreshToken: "r", ExpiresIn: 60}}
	authenticator := newFakeAuthenticator(t, service)

	session, err := authenticator.StartLogin(cfg, "", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	pasted := chatgpt.RedirectURI + "?code=c&state=" + session.State + "&client_id=oaiapp_issued"
	_, err = authenticator.CompleteLogin(context.Background(), cfg, session, pasted)
	if !errors.Is(err, chatgpt.ErrMissingClientID) {
		t.Fatalf("error = %v, want %v", err, chatgpt.ErrMissingClientID)
	}
}

func TestChatGPTCompleteLoginPropagatesExchangeFailure(t *testing.T) {
	cfg := testConfig(t)
	service := &fakeChatGPTService{exchangeErr: errors.New("boom")}
	authenticator := newFakeAuthenticator(t, service)

	session, err := authenticator.StartLogin(cfg, "", "", "")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	pasted := chatgpt.RedirectURI + "?code=c&state=" + session.State + "&client_id=oaiapp_issued"
	if _, err = authenticator.CompleteLogin(context.Background(), cfg, session, pasted); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
}

func TestChatGPTLoginReadsPastedURLThroughPrompt(t *testing.T) {
	cfg := testConfig(t)
	service := &fakeChatGPTService{credential: &chatgpt.Credential{
		AccessToken:  fakeAccessToken(t, "user-123"),
		RefreshToken: "refresh",
		ClientID:     "oaiapp_issued",
		ExpiresIn:    600,
	}}
	authenticator := newFakeAuthenticator(t, service)

	var prompted bool
	record, err := authenticator.Login(context.Background(), cfg, &LoginOptions{
		Prompt: func(prompt string) (string, error) {
			prompted = true
			if !strings.Contains(strings.ToLower(prompt), "callback") {
				t.Fatalf("unexpected prompt: %q", prompt)
			}
			return chatgpt.RedirectURI + "?code=c&state=" + service.gotState + "&client_id=oaiapp_issued", nil
		},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !prompted {
		t.Fatal("prompt was not used")
	}
	if record == nil || record.Provider != chatgpt.ProviderName {
		t.Fatalf("unexpected record: %+v", record)
	}
}

func TestChatGPTLoginRequiresPrompt(t *testing.T) {
	cfg := testConfig(t)
	authenticator := newFakeAuthenticator(t, &fakeChatGPTService{})
	if _, err := authenticator.Login(context.Background(), cfg, &LoginOptions{}); err == nil {
		t.Fatal("expected error when no prompt is available")
	}
}

func TestChatGPTLoginRequiresConfig(t *testing.T) {
	authenticator := NewChatGPTAuthenticator()
	if _, err := authenticator.StartLogin(nil, "", "", ""); err == nil {
		t.Fatal("expected error for nil config")
	}
	if _, err := authenticator.CompleteLogin(context.Background(), nil, &ChatGPTLoginSession{}, ""); err == nil {
		t.Fatal("expected error for nil config")
	}
	if _, err := authenticator.CompleteLogin(context.Background(), &config.Config{}, nil, ""); err == nil {
		t.Fatal("expected error for nil session")
	}
}
