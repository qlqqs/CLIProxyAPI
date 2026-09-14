package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ProxyCredentialGuard rejects ambiguous credentials before any access provider is evaluated.
func (a *API) ProxyCredentialGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if c.Request == nil || c.Request.URL == nil || !isProxyPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		_, _, conflict := carpoolaccess.UniqueProxyCredential(c.Request)
		if conflict {
			if a != nil && a.control != nil {
				a.control.RecordProxyCredentialConflict(c.Request.Context(), carpoolaccess.ProxyCredentialSetFingerprint(c.Request))
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Conflicting API credentials"})
			return
		}
		c.Next()
	}
}

// ScopedModelRequestCompletion completes carpool model queries that do not
// enter the common executor lifecycle.
func (a *API) ScopedModelRequestCompletion(observer func(context.Context, pluginapi.RequestCompletion)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		c.Next()
		if observer == nil || c.Request == nil || c.Request.URL == nil || c.Request.Method != http.MethodGet {
			return
		}
		requestCtx := c.Request.Context()
		snapshot, ok := carpoolruntime.AuthorizationFromContext(requestCtx)
		if !ok || snapshot.RequestID() == "" {
			return
		}
		policy := carpoolruntime.CarpoolProxyRoutePolicy(c.Request.Method, c.Request.URL.Path, false)
		if !policy.Allowed {
			return
		}

		statusCode := c.Writer.Status()
		outcome := pluginapi.RequestCompletionSucceeded
		if requestCtx.Err() != nil {
			outcome = pluginapi.RequestCompletionCanceled
			statusCode = 0
		} else if statusCode >= http.StatusBadRequest {
			outcome = pluginapi.RequestCompletionFailed
		}
		completedAt := time.Now().UTC()
		if a != nil && a.now != nil {
			completedAt = a.now().UTC()
		}
		observer(requestCtx, pluginapi.RequestCompletion{
			RequestID:    snapshot.RequestID(),
			SourceFormat: policy.SourceFormat,
			Outcome:      outcome,
			StatusCode:   statusCode,
			CompletedAt:  completedAt,
		})
	}
}

func isProxyPath(path string) bool {
	path = strings.TrimSpace(path)
	return path == "/v1" || strings.HasPrefix(path, "/v1/") ||
		path == "/v1beta" || strings.HasPrefix(path, "/v1beta/") ||
		path == "/openai/v1" || strings.HasPrefix(path, "/openai/v1/") ||
		path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/")
}

// AuthenticatedRequestHook atomically freezes carpool authorization after key validation.
func (a *API) AuthenticatedRequestHook(ctx context.Context, request *http.Request, result *sdkaccess.Result) *sdkaccess.AuthError {
	if a == nil || result == nil || result.Provider != carpoolaccess.ProviderName {
		return nil
	}
	if request == nil || request.URL == nil {
		return sdkaccess.NewForbiddenError("Carpool request is not authorized", nil)
	}
	userID := strings.TrimSpace(result.Metadata[carpoolaccess.MetadataUserID])
	apiKeyID := strings.TrimSpace(result.Metadata[carpoolaccess.MetadataAPIKeyID])
	upgrade := headerContainsToken(request.Header, "Connection", "upgrade") || strings.TrimSpace(request.Header.Get("Upgrade")) != ""
	snapshot, errAuthorize := a.control.AuthorizeProxy(ctx, userID, apiKeyID, result.Principal, request.Method, request.URL.Path, upgrade)
	if errAuthorize != nil {
		if errors.Is(errAuthorize, domain.ErrAccountingUnavailable) {
			return sdkaccess.NewAccountingUnavailableError()
		}
		if errors.Is(errAuthorize, domain.ErrAuthorizationRejected) || errors.Is(errAuthorize, domain.ErrNotFound) || errors.Is(errAuthorize, domain.ErrConflict) {
			return sdkaccess.NewForbiddenError("Carpool request is not authorized", errAuthorize)
		}
		return sdkaccess.NewInternalAuthError("Carpool authorization service unavailable", errAuthorize)
	}
	requestCtx := request.Context()
	requestCtx = carpoolruntime.WithAuthorization(requestCtx, snapshot)
	requestCtx = coreexecutor.WithCredentialScope(requestCtx, coreexecutor.NewCredentialScope(snapshot.AuthIDs()...))
	requestCtx = handlers.WithRequestLifecycleID(requestCtx, snapshot.RequestID())
	*request = *request.WithContext(requestCtx)
	return nil
}

func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
