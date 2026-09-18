package management

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

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
