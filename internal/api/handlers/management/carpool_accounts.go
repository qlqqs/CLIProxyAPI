package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// CarpoolAccountTestResult contains only bounded, non-sensitive execution metadata.
type CarpoolAccountTestResult struct {
	Model         string `json:"model"`
	DurationMS    int64  `json:"duration_ms"`
	ResponseBytes int    `json:"response_bytes"`
}

// CarpoolAccountTestError identifies a safe failure category for the Carpool facade.
type CarpoolAccountTestError struct {
	Status int
	Code   string
}

func (e *CarpoolAccountTestError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code
}

// TestCarpoolAccountConnection executes a minimal Responses request against one
// exact credential. The response body and upstream error text never leave this boundary.
func (h *Handler) TestCarpoolAccountConnection(ctx context.Context, name, index, model, prompt string) (CarpoolAccountTestResult, error) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusServiceUnavailable, Code: "accounts_unavailable"}
	}

	var target *coreauth.Auth
	for _, auth := range manager.List() {
		if auth == nil || (auth.ID != name && auth.FileName != name) || (index != "" && lockedAuthIndex(auth) != index) {
			continue
		}
		if target != nil {
			return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusConflict, Code: "account_ambiguous"}
		}
		target = auth
	}
	if target == nil {
		return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusNotFound, Code: "account_not_found"}
	}
	if target.Provider != "codex" && target.Provider != "openai" || coreauth.IsPluginVirtualAuth(target) {
		return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusForbidden, Code: "account_not_testable"}
	}
	if target.Disabled || target.Status == coreauth.StatusDisabled {
		return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusForbidden, Code: "account_disabled"}
	}

	payload, errMarshal := json.Marshal(map[string]any{
		"model":             model,
		"stream":            false,
		"store":             false,
		"max_output_tokens": 16,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": prompt}},
		}},
	})
	if errMarshal != nil {
		return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusInternalServerError, Code: "test_failed"}
	}

	started := time.Now()
	response, errExecute := manager.Execute(ctx, []string{target.Provider}, coreexecutor.Request{
		Model: model, Payload: payload, Format: sdktranslator.FormatOpenAIResponse,
	}, coreexecutor.Options{
		CredentialScope: coreexecutor.NewCredentialScope(target.ID),
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		ResponseFormat:  sdktranslator.FormatOpenAIResponse,
		OriginalRequest: payload,
		Metadata:        map[string]any{coreexecutor.PinnedAuthMetadataKey: target.ID},
	})
	duration := time.Since(started).Milliseconds()
	if errExecute != nil {
		if errors.Is(errExecute, context.Canceled) || errors.Is(errExecute, context.DeadlineExceeded) {
			return CarpoolAccountTestResult{}, errExecute
		}
		return CarpoolAccountTestResult{}, classifyCarpoolAccountTestError(errExecute)
	}
	trimmed := bytes.TrimSpace(response.Payload)
	var responseObject struct {
		ID     json.RawMessage `json:"id"`
		Object json.RawMessage `json:"object"`
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) || json.Unmarshal(trimmed, &responseObject) != nil ||
		len(responseObject.Error) != 0 || (len(responseObject.ID) == 0 && len(responseObject.Object) == 0 && len(responseObject.Output) == 0) {
		return CarpoolAccountTestResult{}, &CarpoolAccountTestError{Status: http.StatusBadGateway, Code: "invalid_upstream_response"}
	}
	return CarpoolAccountTestResult{Model: model, DurationMS: duration, ResponseBytes: len(response.Payload)}, nil
}

func classifyCarpoolAccountTestError(err error) error {
	status := 0
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	switch {
	case status == http.StatusUnauthorized:
		return &CarpoolAccountTestError{Status: http.StatusUnauthorized, Code: "authorization_expired"}
	case status == http.StatusForbidden:
		return &CarpoolAccountTestError{Status: http.StatusForbidden, Code: "upstream_forbidden"}
	case status == http.StatusTooManyRequests:
		return &CarpoolAccountTestError{Status: http.StatusTooManyRequests, Code: "rate_limited"}
	case status >= 500 && status <= 599:
		return &CarpoolAccountTestError{Status: http.StatusBadGateway, Code: "upstream_unavailable"}
	default:
		return &CarpoolAccountTestError{Status: http.StatusBadGateway, Code: "test_failed"}
	}
}

// ValidateCarpoolAccountTarget checks the unfiltered catalog, including hidden
// disabled credentials, before the restricted carpool facade delegates a write.
// Imports intentionally never replace existing files or catalog entries.
func (h *Handler) ValidateCarpoolAccountTarget(name, index string, importing bool) int {
	h.mu.Lock()
	manager := h.authManager
	dir := h.cfg.AuthDir
	h.mu.Unlock()
	if manager == nil {
		return http.StatusServiceUnavailable
	}
	matches := 0
	for _, auth := range manager.List() {
		if auth == nil || (auth.ID != name && auth.FileName != name) {
			continue
		}
		if importing {
			return http.StatusConflict
		}
		if auth.Provider != "codex" && auth.Provider != "openai" {
			return http.StatusForbidden
		}
		if coreauth.IsPluginVirtualAuth(auth) {
			return http.StatusForbidden
		}
		if index != "" && lockedAuthIndex(auth) != index {
			continue
		}
		matches++
	}
	if importing {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return http.StatusConflict
		} else if !os.IsNotExist(err) {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	}
	if matches == 0 {
		return http.StatusNotFound
	}
	if matches != 1 {
		return http.StatusConflict
	}
	return http.StatusOK
}

// CarpoolHiddenAccountEntries supplies safe identity fields for runtime entries
// omitted by the legacy list (notably disabled configuration-backed keys).
func (h *Handler) CarpoolHiddenAccountEntries() []gin.H {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	entries := make([]gin.H, 0)
	if manager == nil {
		return entries
	}
	for _, auth := range manager.List() {
		if auth == nil || (auth.Provider != "codex" && auth.Provider != "openai") || h.buildAuthFileEntry(auth) != nil {
			continue
		}
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = auth.ID
		}
		entries = append(entries, gin.H{"name": name, "provider": auth.Provider, "auth_index": lockedAuthIndex(auth), "disabled": auth.Disabled, "status": auth.Status, "created_at": auth.CreatedAt, "updated_at": auth.UpdatedAt})
	}
	return entries
}

type createOnlyAuthFileContextKey struct{}

// WithCreateOnlyAuthFile opts the existing upload handler into atomic creation.
// Legacy management clients retain their existing overwrite behavior.
func WithCreateOnlyAuthFile(ctx context.Context) context.Context {
	return context.WithValue(ctx, createOnlyAuthFileContextKey{}, true)
}

func createOnlyAuthFile(ctx context.Context) bool {
	enabled, _ := ctx.Value(createOnlyAuthFileContextKey{}).(bool)
	return enabled
}

type persistentAuthStatusContextKey struct{}

// WithPersistentAuthStatus requests acknowledged storage for file status changes.
// Config-backed changes retain their own config persistence/reload contract.
func WithPersistentAuthStatus(ctx context.Context) context.Context {
	return context.WithValue(ctx, persistentAuthStatusContextKey{}, true)
}

func persistentAuthStatus(ctx context.Context) bool {
	enabled, _ := ctx.Value(persistentAuthStatusContextKey{}).(bool)
	return enabled
}
