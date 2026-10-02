package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/chatgpt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Refresh rotates the access token with the dynamically issued client id. Both the
// refresh token and the client id are required: the client id is bound to the refresh
// token at authorization time, so a credential stored without one cannot be renewed
// and is returned unchanged for the operator to reconnect.
func (e *ChatGPTExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("chatgpt executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "chatgpt executor: auth is nil"}
	}
	refreshToken := ""
	clientID := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok {
			refreshToken = strings.TrimSpace(v)
		}
		if v, ok := auth.Metadata["client_id"].(string); ok {
			clientID = strings.TrimSpace(v)
		}
	}
	if refreshToken == "" || clientID == "" {
		log.Debugf("chatgpt executor: skipping refresh (refresh_token=%t client_id=%t)", refreshToken != "", clientID != "")
		return auth, nil
	}

	credential, err := e.authService(e.cfg, auth.ProxyURL).RefreshAccessToken(ctx, refreshToken, clientID)
	if err != nil {
		return nil, err
	}
	if credential == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "chatgpt executor: refresh returned no credential"}
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	auth.Metadata["access_token"] = credential.AccessToken
	if credential.RefreshToken != "" {
		auth.Metadata["refresh_token"] = credential.RefreshToken
	}
	auth.Metadata["client_id"] = credential.ClientID
	if len(credential.Scopes) > 0 {
		auth.Metadata["scopes"] = credential.Scopes
	}
	auth.Metadata["expired"] = credential.ExpiresAt(now).Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)
	auth.Metadata["type"] = chatgpt.ProviderName
	return auth, nil
}

// PrepareRequest injects the bearer token into an outgoing HTTP request.
func (e *ChatGPTExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := chatGPTCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Del("Authorization")
	}
	applyChatGPTCustomHeaders(req, auth)
	return nil
}

// HttpRequest injects credentials into the supplied request and executes it.
func (e *ChatGPTExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("chatgpt executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}
