package chatgpt

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// Error values returned by this package. Callers can match them with errors.Is.
var (
	// ErrMissingCode indicates the authorization code is absent.
	ErrMissingCode = errors.New("chatgpt: missing authorization code")
	// ErrMissingState indicates the OAuth state is absent.
	ErrMissingState = errors.New("chatgpt: missing oauth state")
	// ErrStateMismatch indicates the callback state does not match the request.
	ErrStateMismatch = errors.New("chatgpt: oauth state mismatch")
	// ErrMissingClientID indicates the dynamic client registration callback did
	// not carry the client id issued for this login.
	ErrMissingClientID = errors.New("chatgpt: registration callback did not contain an issued client ID")
	// ErrCallbackURLShape indicates the pasted URL is not this flow's callback.
	ErrCallbackURLShape = errors.New("chatgpt: pasted callback URL must match " + RedirectURI)
	// ErrAuthorizationFailed indicates the authorization server returned an error.
	ErrAuthorizationFailed = errors.New("chatgpt: authorization failed")
	// ErrDeviceIDRequired indicates the installation device id is missing/invalid.
	ErrDeviceIDRequired = errors.New("chatgpt: sign in with ChatGPT requires a device ID (UUID) for this installation")
	// ErrRefreshTokenRequired indicates the stored credential has no refresh token.
	ErrRefreshTokenRequired = errors.New("chatgpt: refresh token is required")
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// AuthorizationResult carries the values read from the callback URL.
type AuthorizationResult struct {
	Code     string
	ClientID string
}

// Credential is the validated outcome of a token request.
type Credential struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Scopes       []string
	// ClientID is the dynamically issued client id that must be replayed on refresh.
	ClientID string
	// ExpiresIn is the lifetime of AccessToken in seconds.
	ExpiresIn int
}

// ExpiresAt returns the absolute expiry, already reduced by ExpiryMargin.
func (c *Credential) ExpiresAt(now time.Time) time.Time {
	if c == nil {
		return now
	}
	return now.Add(time.Duration(c.ExpiresIn)*time.Second - ExpiryMargin)
}

// ChatGPTAuth performs the "Sign in with ChatGPT" OAuth flow.
type ChatGPTAuth struct {
	httpClient *http.Client
	// authorizeURL and tokenURL default to the production endpoints. They are
	// fields so tests can point the client at a local server.
	authorizeURL string
	tokenURL     string
}

// NewChatGPTAuth builds the auth service using proxy settings from cfg.
func NewChatGPTAuth(cfg *config.Config) *ChatGPTAuth {
	return NewChatGPTAuthWithProxyURL(cfg, "")
}

// NewChatGPTAuthWithProxyURL builds the auth service; proxyURL wins over cfg.
func NewChatGPTAuthWithProxyURL(cfg *config.Config, proxyURL string) *ChatGPTAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &ChatGPTAuth{
		httpClient:   util.SetProxy(&sdkCfg, &http.Client{}),
		authorizeURL: AuthorizeURL,
		tokenURL:     TokenURL,
	}
}

func (o *ChatGPTAuth) authorizationEndpoint() string {
	if o != nil && strings.TrimSpace(o.authorizeURL) != "" {
		return o.authorizeURL
	}
	return AuthorizeURL
}

func (o *ChatGPTAuth) tokenEndpoint() string {
	if o != nil && strings.TrimSpace(o.tokenURL) != "" {
		return o.tokenURL
	}
	return TokenURL
}

// RandomValue mirrors pi's randomValue(): 32 random bytes as unpadded base64url.
// It is used for both the OAuth state and the nonce.
func RandomValue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("chatgpt: failed to generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// AgentHostID converts an installation UUID into the urn:uuid identifier that the
// authorization server expects in ext_agent_host_id.
func AgentHostID(deviceID string) (string, error) {
	trimmed := strings.TrimSpace(deviceID)
	if !uuidPattern.MatchString(trimmed) {
		return "", ErrDeviceIDRequired
	}
	return "urn:uuid:" + strings.ToLower(trimmed), nil
}

// GenerateAuthURL builds the authorization URL. Query parameters are emitted in the
// same order as pi produces them, so upstream receives a byte-identical request.
func (o *ChatGPTAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes, hostID, agentNameHint string) (string, error) {
	if pkceCodes == nil {
		return "", errors.New("chatgpt: PKCE codes are required")
	}
	trimmedState := strings.TrimSpace(state)
	if trimmedState == "" {
		return "", ErrMissingState
	}
	trimmedHostID := strings.TrimSpace(hostID)
	if trimmedHostID == "" {
		return "", ErrDeviceIDRequired
	}
	hint := strings.TrimSpace(agentNameHint)
	if hint == "" {
		hint = DefaultAgentNameHint
	}
	nonce, err := RandomValue()
	if err != nil {
		return "", err
	}

	query := encodeOrdered(
		[2]string{"client_id", DynamicClientID},
		[2]string{"agent_name_hint", hint},
		[2]string{"ext_agent_host_id", trimmedHostID},
		[2]string{"response_type", "code"},
		[2]string{"redirect_uri", RedirectURI},
		[2]string{"resource", Resource},
		[2]string{"scope", Scope},
		[2]string{"state", trimmedState},
		[2]string{"code_challenge", pkceCodes.CodeChallenge},
		[2]string{"code_challenge_method", "S256"},
		[2]string{"nonce", nonce},
	)
	return o.authorizationEndpoint() + "?" + query, nil
}

// ParseManualCallback parses the callback URL a user pastes back after signing in.
// The URL must be this flow's redirect URI, otherwise the helper rejects it before
// the state check so that unrelated pastes fail with an actionable message.
func ParseManualCallback(input, expectedState string) (*AuthorizationResult, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, ErrCallbackURLShape
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, ErrCallbackURLShape
	}
	expected, err := url.Parse(RedirectURI)
	if err != nil {
		return nil, ErrCallbackURLShape
	}
	if !strings.EqualFold(parsed.Scheme, expected.Scheme) ||
		!strings.EqualFold(parsed.Host, expected.Host) ||
		parsed.EscapedPath() != expected.EscapedPath() {
		return nil, ErrCallbackURLShape
	}
	if errParam := strings.TrimSpace(parsed.Query().Get("error")); errParam != "" {
		return nil, fmt.Errorf("%w: %s", ErrAuthorizationFailed, errParam)
	}
	return authorizationResultFromCallback(parsed, strings.TrimSpace(expectedState))
}

func authorizationResultFromCallback(parsed *url.URL, expectedState string) (*AuthorizationResult, error) {
	query := parsed.Query()
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return nil, ErrMissingCode
	}
	state := strings.TrimSpace(query.Get("state"))
	if state == "" {
		return nil, ErrMissingState
	}
	if state != expectedState {
		return nil, ErrStateMismatch
	}
	clientID := strings.TrimSpace(query.Get("client_id"))
	if clientID == "" {
		return nil, ErrMissingClientID
	}
	return &AuthorizationResult{Code: code, ClientID: clientID}, nil
}

// ExchangeAuthorizationCode trades the authorization code for credentials.
func (o *ChatGPTAuth) ExchangeAuthorizationCode(ctx context.Context, code, verifier, clientID string) (*Credential, error) {
	trimmedCode := strings.TrimSpace(code)
	if trimmedCode == "" {
		return nil, ErrMissingCode
	}
	trimmedVerifier := strings.TrimSpace(verifier)
	if trimmedVerifier == "" {
		return nil, errors.New("chatgpt: code verifier is required")
	}
	trimmedClientID := strings.TrimSpace(clientID)
	if trimmedClientID == "" {
		return nil, ErrMissingClientID
	}

	form := encodeOrdered(
		[2]string{"grant_type", "authorization_code"},
		[2]string{"client_id", trimmedClientID},
		[2]string{"code", trimmedCode},
		[2]string{"code_verifier", trimmedVerifier},
		[2]string{"redirect_uri", RedirectURI},
		[2]string{"resource", Resource},
	)
	tokenResp, err := o.requestToken(ctx, form)
	if err != nil {
		return nil, err
	}
	// pi keeps the presence check of the ID token as part of the token-response
	// contract even though the value is never used to identify the user.
	if strings.TrimSpace(tokenResp.IDToken) == "" {
		return nil, errors.New("chatgpt: token response did not contain an ID token")
	}
	return credentialFromTokenResponse(tokenResp, trimmedClientID)
}

// RefreshAccessToken rotates an access token using the stored refresh token. The
// dynamically issued client id must be supplied; refresh fails without it.
func (o *ChatGPTAuth) RefreshAccessToken(ctx context.Context, refreshToken, clientID string) (*Credential, error) {
	trimmedClientID := strings.TrimSpace(clientID)
	if trimmedClientID == "" {
		return nil, ErrMissingClientID
	}
	trimmedRefresh := strings.TrimSpace(refreshToken)
	if trimmedRefresh == "" {
		return nil, ErrRefreshTokenRequired
	}

	form := encodeOrdered(
		[2]string{"grant_type", "refresh_token"},
		[2]string{"client_id", trimmedClientID},
		[2]string{"refresh_token", trimmedRefresh},
		[2]string{"resource", Resource},
	)
	tokenResp, err := o.requestToken(ctx, form)
	if err != nil {
		return nil, err
	}
	return credentialFromTokenResponse(tokenResp, trimmedClientID)
}

type tokenResponse struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	IDToken      string   `json:"id_token"`
	TokenType    string   `json:"token_type"`
	Scope        string   `json:"scope"`
	ExpiresIn    *float64 `json:"expires_in"`
}

func (o *ChatGPTAuth) requestToken(ctx context.Context, form string) (*tokenResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenEndpoint(), strings.NewReader(form))
	if err != nil {
		return nil, fmt.Errorf("chatgpt: failed to create token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := o.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: token request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: failed to read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chatgpt: OpenAI OAuth token request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("chatgpt: failed to parse token response: %w", err)
	}
	return &parsed, nil
}

func credentialFromTokenResponse(tokenResp *tokenResponse, clientID string) (*Credential, error) {
	if tokenResp == nil {
		return nil, errors.New("chatgpt: token response is nil")
	}
	access := strings.TrimSpace(tokenResp.AccessToken)
	if access == "" {
		return nil, errors.New("chatgpt: token response has invalid access_token")
	}
	refresh := strings.TrimSpace(tokenResp.RefreshToken)
	if refresh == "" {
		return nil, errors.New("chatgpt: token response has invalid refresh_token")
	}
	scope := strings.TrimSpace(tokenResp.Scope)
	if scope == "" {
		return nil, errors.New("chatgpt: token response has invalid scope")
	}
	if tokenResp.ExpiresIn == nil || math.IsNaN(*tokenResp.ExpiresIn) || math.IsInf(*tokenResp.ExpiresIn, 0) || *tokenResp.ExpiresIn <= 0 {
		return nil, errors.New("chatgpt: token response has invalid expires_in")
	}
	scopes := strings.Fields(scope)
	if !slices.Contains(scopes, DirectTokenScope) {
		return nil, fmt.Errorf("chatgpt: OpenAI OAuth grant did not include %s", DirectTokenScope)
	}
	return &Credential{
		AccessToken:  access,
		RefreshToken: refresh,
		IDToken:      strings.TrimSpace(tokenResp.IDToken),
		Scopes:       scopes,
		ClientID:     clientID,
		ExpiresIn:    int(*tokenResp.ExpiresIn),
	}, nil
}

// encodeOrdered renders form data with the given field order. url.Values.Encode()
// sorts keys, which would change the request bytes relative to pi.
func encodeOrdered(pairs ...[2]string) string {
	var builder strings.Builder
	for i, pair := range pairs {
		if i > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(url.QueryEscape(pair[0]))
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(pair[1]))
	}
	return builder.String()
}
