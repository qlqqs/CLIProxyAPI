package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// chatgptLoginSessionTTL bounds how long a started login may be completed.
	chatgptLoginSessionTTL = 30 * time.Minute
	// chatgptMaxPastedCallbackLength bounds the accepted paste size.
	chatgptMaxPastedCallbackLength = 4096
)

// chatgptLoginDriver is the subset of the chatgpt authenticator the HTTP handlers
// need. It exists so tests can drive the endpoints without touching the network.
type chatgptLoginDriver interface {
	StartLogin(cfg *config.Config, agentNameHint, deviceID, proxyURL string) (*sdkAuth.ChatGPTLoginSession, error)
	CompleteLogin(ctx context.Context, cfg *config.Config, session *sdkAuth.ChatGPTLoginSession, pastedCallbackURL string) (*coreauth.Auth, error)
}

var newChatGPTLoginDriver = func() chatgptLoginDriver {
	return sdkAuth.NewChatGPTAuthenticator()
}

type chatgptPendingLogin struct {
	session   *sdkAuth.ChatGPTLoginSession
	createdAt time.Time
}

// chatgptLoginStore keeps the server-side half of a pending login (the PKCE verifier
// and the expected state) between the "auth url" call and the pasted callback.
//
// This flow deliberately does not register with the shared OAuth session store: the
// generic /oauth-callback endpoint persists only code and state, and the callback for
// this provider also carries the client id that the authorization server issues
// dynamically, without which the credential could never be refreshed.
type chatgptLoginStore struct {
	mu       sync.Mutex
	sessions map[string]chatgptPendingLogin
}

var chatgptPendingLogins = &chatgptLoginStore{sessions: make(map[string]chatgptPendingLogin)}

func (s *chatgptLoginStore) put(session *sdkAuth.ChatGPTLoginSession) {
	if s == nil || session == nil {
		return
	}
	state := strings.TrimSpace(session.State)
	if state == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(time.Now())
	s.sessions[state] = chatgptPendingLogin{session: session, createdAt: time.Now()}
}

// take returns and consumes the pending login for state, making each paste single-use.
func (s *chatgptLoginStore) take(state string) (*sdkAuth.ChatGPTLoginSession, bool) {
	trimmed := strings.TrimSpace(state)
	if s == nil || trimmed == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(time.Now())
	entry, ok := s.sessions[trimmed]
	if !ok {
		return nil, false
	}
	delete(s.sessions, trimmed)
	return entry.session, true
}

func (s *chatgptLoginStore) purgeLocked(now time.Time) {
	for state, entry := range s.sessions {
		if now.Sub(entry.createdAt) > chatgptLoginSessionTTL {
			delete(s.sessions, state)
		}
	}
}

type chatgptCallbackRequest struct {
	State       string `json:"state"`
	RedirectURL string `json:"redirect_url"`
}

// RequestChatGPTToken starts a "Sign in with ChatGPT" login and returns the
// authorization URL to open. The flow has no loopback listener, so the browser's
// callback will fail to load; the operator copies that URL and posts it to
// PostChatGPTCallback.
func (h *Handler) RequestChatGPTToken(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "handler not initialized"})
		return
	}

	proxyURL := strings.TrimSpace(c.Query("proxy_url"))
	if _, errProxy := proxyutil.Parse(proxyURL); errProxy != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"status": "error", "error": "invalid proxy URL"})
		return
	}

	driver := newChatGPTLoginDriver()
	session, err := driver.StartLogin(h.cfg, c.Query("agent_name_hint"), c.Query("device_id"), proxyURL)
	if err != nil {
		log.WithError(err).Error("failed to start chatgpt login")
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to start chatgpt login"})
		return
	}

	chatgptPendingLogins.put(session)

	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"url":          session.AuthorizeURL,
		"state":        session.State,
		"redirect_uri": chatgpt.RedirectURI,
		"provider":     chatgpt.ProviderName,
		"instructions": "Open the url and sign in. The browser cannot reach " + chatgpt.RedirectURI +
			" afterwards: copy the full address from the address bar and POST it as {\"state\":\"...\",\"redirect_url\":\"...\"} to /v0/management/chatgpt-auth-callback.",
	})
}

// PostChatGPTCallback completes a pending login from the pasted callback URL.
func (h *Handler) PostChatGPTCallback(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "handler not initialized"})
		return
	}

	var req chatgptCallbackRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}

	redirectURL := strings.TrimSpace(req.RedirectURL)
	if redirectURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "redirect_url is required"})
		return
	}
	if len(redirectURL) > chatgptMaxPastedCallbackLength {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "redirect_url is too long"})
		return
	}

	state := strings.TrimSpace(req.State)
	if state == "" {
		state = chatgptStateFromRedirectURL(redirectURL)
	}
	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}

	session, ok := chatgptPendingLogins.take(state)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}

	ctx := PopulateAuthContext(context.Background(), c)
	driver := newChatGPTLoginDriver()
	record, err := driver.CompleteLogin(ctx, h.cfg, session, redirectURL)
	if err != nil {
		status, message := chatgptCallbackErrorStatus(err)
		log.WithError(err).Warn("chatgpt login callback failed")
		c.JSON(status, gin.H{"status": "error", "error": message})
		return
	}

	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		log.WithError(errSave).Error("failed to save chatgpt credentials")
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save credentials"})
		return
	}

	log.Infof("ChatGPT authentication successful; credentials saved to %s", savedPath)
	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"provider":  record.Provider,
		"file_name": record.FileName,
	})
}

func chatgptStateFromRedirectURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("state"))
}

// chatgptCallbackErrorStatus maps flow errors onto HTTP responses. Invalid pastes are
// client errors; anything else (token endpoint failures, transport errors) is a
// gateway error so operators can tell the two apart.
func chatgptCallbackErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, chatgpt.ErrStateMismatch):
		return http.StatusBadRequest, "oauth state mismatch"
	case errors.Is(err, chatgpt.ErrCallbackURLShape):
		return http.StatusBadRequest, "redirect_url is not a " + chatgpt.RedirectURI + " callback URL"
	case errors.Is(err, chatgpt.ErrMissingCode):
		return http.StatusBadRequest, "callback URL did not contain an authorization code"
	case errors.Is(err, chatgpt.ErrMissingState):
		return http.StatusBadRequest, "callback URL did not contain a state"
	case errors.Is(err, chatgpt.ErrMissingClientID):
		return http.StatusBadRequest, "callback URL did not contain the issued client id"
	case errors.Is(err, chatgpt.ErrAuthorizationFailed):
		return http.StatusBadRequest, "authorization was rejected"
	case errors.Is(err, chatgpt.ErrDeviceIDRequired):
		return http.StatusBadRequest, "a valid device id (UUID) is required"
	default:
		return http.StatusBadGateway, "token exchange failed"
	}
}
