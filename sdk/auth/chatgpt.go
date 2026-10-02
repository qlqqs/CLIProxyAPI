package auth

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// ChatGPTAgentNameHintMetadataKey overrides the default agent name hint.
	ChatGPTAgentNameHintMetadataKey = "chatgpt_agent_name_hint"
	// ChatGPTDeviceIDMetadataKey overrides the installation device id.
	ChatGPTDeviceIDMetadataKey = "chatgpt_device_id"
	// ChatGPTProxyURLMetadataKey selects a per-account egress proxy.
	ChatGPTProxyURLMetadataKey = "proxy_url"
	// chatgptDeviceIDFileName stores the installation UUID inside the auth dir.
	// The auth watcher only reads *.json files, so a dotfile is inert, matching
	// the existing .oauth-<provider>-<state>.oauth convention.
	chatgptDeviceIDFileName = ".chatgpt-device-id"
)

// ChatGPTAuthenticator implements the "Sign in with ChatGPT" login flow.
//
// Unlike the codex authenticator this flow never listens on a local port. The
// authorization server issues the client id dynamically and returns it on the
// callback URL, and the operator pastes that URL back; keeping the loopback callback
// out of the picture means the flow also works on a headless host, and it avoids a
// second listener competing for port 1455 with the codex flow.
type ChatGPTAuthenticator struct {
	// newAuthService is a seam for tests; production builds chatgpt.ChatGPTAuth
	// with the per-account proxy URL when one was supplied.
	newAuthService func(*config.Config, string) chatGPTAuthService
}

// chatGPTAuthService is the subset of *chatgpt.ChatGPTAuth the authenticator needs.
type chatGPTAuthService interface {
	GenerateAuthURL(state string, pkceCodes *chatgpt.PKCECodes, hostID, agentNameHint string) (string, error)
	ExchangeAuthorizationCode(ctx context.Context, code, verifier, clientID string) (*chatgpt.Credential, error)
}

// NewChatGPTAuthenticator constructs a "Sign in with ChatGPT" authenticator.
func NewChatGPTAuthenticator() *ChatGPTAuthenticator {
	return &ChatGPTAuthenticator{
		newAuthService: func(cfg *config.Config, proxyURL string) chatGPTAuthService {
			return chatgpt.NewChatGPTAuthWithProxyURL(cfg, proxyURL)
		},
	}
}

// Provider returns the provider identifier for credentials created by this flow.
func (a *ChatGPTAuthenticator) Provider() string {
	return chatgpt.ProviderName
}

// RefreshLead reports how far ahead of expiry credentials should be refreshed.
func (a *ChatGPTAuthenticator) RefreshLead() *time.Duration {
	return new(24 * time.Hour)
}

// ChatGPTLoginSession is the server-side state of a pending login. CodeVerifier
// must stay on the server until the pasted callback URL arrives.
type ChatGPTLoginSession struct {
	AuthorizeURL string
	State        string
	CodeVerifier string
	// ProxyURL is the per-account proxy used for the token exchange and stored on
	// the credential so later refreshes and upstream calls use the same egress.
	ProxyURL string
}

// StartLogin creates the authorization URL for a new login.
func (a *ChatGPTAuthenticator) StartLogin(cfg *config.Config, agentNameHint, deviceID, proxyURL string) (*ChatGPTLoginSession, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	pkceCodes, err := chatgpt.GeneratePKCECodes()
	if err != nil {
		return nil, fmt.Errorf("chatgpt pkce generation failed: %w", err)
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, fmt.Errorf("chatgpt state generation failed: %w", err)
	}

	resolvedDeviceID := strings.TrimSpace(deviceID)
	if resolvedDeviceID == "" {
		resolvedDeviceID, err = chatgpt.LoadOrCreateDeviceID(filepath.Join(strings.TrimSpace(cfg.AuthDir), chatgptDeviceIDFileName))
		if err != nil {
			return nil, err
		}
	}
	hostID, err := chatgpt.AgentHostID(resolvedDeviceID)
	if err != nil {
		return nil, err
	}

	authSvc := a.authService(cfg, proxyURL)
	authorizeURL, err := authSvc.GenerateAuthURL(state, pkceCodes, hostID, agentNameHint)
	if err != nil {
		return nil, fmt.Errorf("chatgpt authorization url generation failed: %w", err)
	}

	return &ChatGPTLoginSession{
		AuthorizeURL: authorizeURL,
		State:        state,
		CodeVerifier: pkceCodes.CodeVerifier,
		ProxyURL:     strings.TrimSpace(proxyURL),
	}, nil
}

// CompleteLogin exchanges the pasted callback URL for credentials and builds the
// auth record. The pasted URL is validated against the session state, and the client
// id it carries is persisted because refresh requests require it.
func (a *ChatGPTAuthenticator) CompleteLogin(ctx context.Context, cfg *config.Config, session *ChatGPTLoginSession, pastedCallbackURL string) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if session == nil {
		return nil, errors.New("chatgpt: login session is required")
	}
	result, err := chatgpt.ParseManualCallback(pastedCallbackURL, session.State)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	log.Debug("chatgpt authorization code received; exchanging for tokens")
	credential, err := a.authService(cfg, session.ProxyURL).ExchangeAuthorizationCode(ctx, result.Code, session.CodeVerifier, result.ClientID)
	if err != nil {
		return nil, err
	}
	return buildChatGPTAuthRecord(credential, session.ProxyURL)
}

// Login runs the whole flow interactively: it prints the authorization URL, reads the
// pasted callback URL through opts.Prompt and persists the resulting credential.
func (a *ChatGPTAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	session, err := a.StartLogin(cfg, opts.Metadata[ChatGPTAgentNameHintMetadataKey], opts.Metadata[ChatGPTDeviceIDMetadataKey], opts.Metadata[ChatGPTProxyURLMetadataKey])
	if err != nil {
		return nil, err
	}

	fmt.Println("Sign in with ChatGPT")
	fmt.Printf("Visit the following URL to continue authentication:\n%s\n", session.AuthorizeURL)
	fmt.Printf("After signing in, the browser cannot reach %s. Copy the full address from the address bar and paste it below.\n", chatgpt.RedirectURI)

	if opts.Prompt == nil {
		return nil, errors.New("chatgpt: login requires a prompt to read the pasted callback URL")
	}
	pasted, err := opts.Prompt("Paste the full callback URL: ")
	if err != nil {
		return nil, err
	}

	record, err := a.CompleteLogin(ctx, cfg, session, pasted)
	if err != nil {
		return nil, err
	}
	fmt.Println("ChatGPT authentication successful")
	return record, nil
}

func (a *ChatGPTAuthenticator) authService(cfg *config.Config, proxyURL string) chatGPTAuthService {
	if a != nil && a.newAuthService != nil {
		return a.newAuthService(cfg, proxyURL)
	}
	return chatgpt.NewChatGPTAuthWithProxyURL(cfg, proxyURL)
}

func buildChatGPTAuthRecord(credential *chatgpt.Credential, proxyURL string) (*coreauth.Auth, error) {
	if credential == nil {
		return nil, errors.New("chatgpt: credential is nil")
	}
	if strings.TrimSpace(credential.ClientID) == "" {
		// Without the dynamically issued client id the credential cannot be refreshed.
		return nil, chatgpt.ErrMissingClientID
	}

	now := time.Now()
	storage := &chatgpt.ChatGPTTokenStorage{
		AccessToken:  credential.AccessToken,
		RefreshToken: credential.RefreshToken,
		ClientID:     credential.ClientID,
		Scopes:       credential.Scopes,
		Email:        chatgpt.EmailFromIDToken(credential.IDToken),
		LastRefresh:  now.Format(time.RFC3339),
		Expire:       credential.ExpiresAt(now).Format(time.RFC3339),
	}
	fileName := chatgpt.CredentialFileName(chatgpt.SubjectHash(credential.AccessToken))

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: chatgpt.ProviderName,
		FileName: fileName,
		Storage:  storage,
		// Metadata mirrors the persisted fields so an execution that starts before
		// the auth dir is rescanned still finds the credential.
		Metadata: map[string]any{
			"type":          chatgpt.ProviderName,
			"access_token":  credential.AccessToken,
			"refresh_token": credential.RefreshToken,
			"client_id":     credential.ClientID,
			"expired":       storage.Expire,
			"last_refresh":  storage.LastRefresh,
		},
		Attributes: map[string]string{
			"base_url": chatgpt.DefaultBaseURL,
		},
	}
	if resolvedProxy := strings.TrimSpace(proxyURL); resolvedProxy != "" {
		record.ProxyURL = resolvedProxy
		record.Metadata["proxy_url"] = resolvedProxy
	}
	return record, nil
}
