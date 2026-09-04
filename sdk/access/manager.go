package access

import (
	"context"
	"net/http"
	"sync"
)

// Manager coordinates authentication providers.
type Manager struct {
	mu                       sync.RWMutex
	providers                []Provider
	authenticatedRequestHook func(context.Context, *http.Request, *Result) *AuthError
}

// NewManager constructs an empty manager.
func NewManager() *Manager {
	return &Manager{}
}

// SetProviders replaces the active provider list.
func (m *Manager) SetProviders(providers []Provider) {
	if m == nil {
		return
	}
	cloned := make([]Provider, len(providers))
	copy(cloned, providers)
	m.mu.Lock()
	m.providers = cloned
	m.mu.Unlock()
}

// Providers returns a snapshot of the active providers.
func (m *Manager) Providers() []Provider {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make([]Provider, len(m.providers))
	copy(snapshot, m.providers)
	return snapshot
}

// SetAuthenticatedRequestHook installs a hook that runs after provider authentication succeeds.
// The hook may attach immutable request context or reject authorization before the handler runs.
func (m *Manager) SetAuthenticatedRequestHook(hook func(context.Context, *http.Request, *Result) *AuthError) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.authenticatedRequestHook = hook
	m.mu.Unlock()
}

func (m *Manager) requestHook() func(context.Context, *http.Request, *Result) *AuthError {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.authenticatedRequestHook
}

// Authenticate evaluates providers until one succeeds.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError) {
	if m == nil {
		return nil, nil
	}
	providers := m.Providers()
	if len(providers) == 0 {
		return nil, nil
	}

	var (
		missing bool
		invalid bool
	)

	for _, provider := range providers {
		if provider == nil {
			continue
		}
		res, authErr := provider.Authenticate(ctx, r)
		if authErr == nil {
			if hook := m.requestHook(); hook != nil && res != nil {
				if hookErr := hook(ctx, r, res); hookErr != nil {
					return nil, hookErr
				}
			}
			return res, nil
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNotHandled) {
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNoCredentials) {
			missing = true
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeInvalidCredential) {
			invalid = true
			continue
		}
		return nil, authErr
	}

	if invalid {
		return nil, NewInvalidCredentialError()
	}
	if missing {
		return nil, NewNoCredentialsError()
	}
	return nil, NewNoCredentialsError()
}
