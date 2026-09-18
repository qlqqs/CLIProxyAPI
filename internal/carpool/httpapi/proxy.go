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
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ProxyCredentialGuard rejects ambiguous credentials before any access provider is evaluated.
func (a *API) ProxyCredentialGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if c != nil && c.Request != nil {
				if snapshot, ok := carpoolruntime.AuthorizationFromContext(c.Request.Context()); ok {
					snapshot.Release()
				}
			}
		}()
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

// ScopedFailedRequestCompletion covers billable HTTP failures before an executor
// completion callback. It never treats an arbitrary successful HTTP close as final.
func (a *API) ScopedFailedRequestCompletion(observer func(context.Context, pluginapi.RequestCompletion)) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if observer == nil || c.Request == nil || c.Request.URL == nil || c.Request.Method == http.MethodGet {
			return
		}
		ctx := c.Request.Context()
		snapshot, ok := carpoolruntime.AuthorizationFromContext(ctx)
		if !ok {
			return
		}
		policy := carpoolruntime.CarpoolProxyRoutePolicy(c.Request.Method, c.Request.URL.Path, false)
		if !policy.Allowed {
			return
		}
		status := c.Writer.Status()
		outcome := pluginapi.RequestCompletionRejected
		if ctx.Err() != nil {
			outcome = pluginapi.RequestCompletionCanceled
			status = 0
		} else if status < http.StatusBadRequest {
			return
		}
		completedAt := time.Now().UTC()
		if a != nil && a.now != nil {
			completedAt = a.now().UTC()
		}
		observer(ctx, pluginapi.RequestCompletion{RequestID: snapshot.RequestID(), SourceFormat: policy.SourceFormat, Outcome: outcome, StatusCode: status, CompletedAt: completedAt})
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
	return path == "/responses" || strings.HasPrefix(path, "/responses/") ||
		path == "/v1" || strings.HasPrefix(path, "/v1/") ||
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
		if errors.Is(errAuthorize, carpoolruntime.ErrConcurrencyQueueFull) {
			return &sdkaccess.AuthError{Code: "concurrency_queue_full", Message: "Concurrency queue is full", StatusCode: http.StatusTooManyRequests}
		}
		if errors.Is(errAuthorize, carpoolruntime.ErrConcurrencyClosed) {
			return &sdkaccess.AuthError{Code: "concurrency_closed", Message: "Concurrency admission is closed", StatusCode: http.StatusServiceUnavailable}
		}
		if errors.Is(errAuthorize, context.Canceled) {
			return sdkaccess.NewForbiddenError("Request canceled", nil)
		}
		var admission *carpoolservice.ProxyAdmissionError
		if errors.As(errAuthorize, &admission) && (admission.Reason == "five_hour_quota_exhausted" || admission.Reason == "weekly_quota_exhausted") {
			return &sdkaccess.AuthError{Code: sdkaccess.AuthErrorCode(admission.Reason), Message: "User quota limit reached", StatusCode: http.StatusForbidden}
		}
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
