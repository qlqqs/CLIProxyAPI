package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/pricing"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type authorizationContextKey struct{}

// AccountSnapshot freezes one allowed upstream account assignment for a request.
type AccountSnapshot struct {
	AuthID       string
	AssignmentID string
	AccountRef   string
	SafeLabel    string
	Provider     string
}

// AuthorizationSnapshot is the immutable carpool identity and credential upper bound.
type AuthorizationSnapshot struct {
	requestID    string
	userID       string
	apiKeyID     string
	carID        string
	membershipID string
	callerScope  string
	accounts     map[string]AccountSnapshot
	prices       *pricing.Snapshot
}

// NewAuthorizationSnapshot copies all values so later assignment changes cannot expand a request.
func NewAuthorizationSnapshot(requestID, userID, apiKeyID, carID, membershipID, callerScope string, accounts map[string]AccountSnapshot, prices ...*pricing.Snapshot) *AuthorizationSnapshot {
	copied := make(map[string]AccountSnapshot, len(accounts))
	for authID, account := range accounts {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		account.AuthID = authID
		copied[authID] = account
	}
	var frozen *pricing.Snapshot
	if len(prices) > 0 {
		frozen = prices[0]
	}
	return &AuthorizationSnapshot{
		prices:       frozen,
		requestID:    strings.TrimSpace(requestID),
		userID:       strings.TrimSpace(userID),
		apiKeyID:     strings.TrimSpace(apiKeyID),
		carID:        strings.TrimSpace(carID),
		membershipID: strings.TrimSpace(membershipID),
		callerScope:  strings.TrimSpace(callerScope),
		accounts:     copied,
	}
}

func (s *AuthorizationSnapshot) RequestID() string {
	if s == nil {
		return ""
	}
	return s.requestID
}

func (s *AuthorizationSnapshot) UserID() string {
	if s == nil {
		return ""
	}
	return s.userID
}

func (s *AuthorizationSnapshot) APIKeyID() string {
	if s == nil {
		return ""
	}
	return s.apiKeyID
}

func (s *AuthorizationSnapshot) CarID() string {
	if s == nil {
		return ""
	}
	return s.carID
}

func (s *AuthorizationSnapshot) MembershipID() string {
	if s == nil {
		return ""
	}
	return s.membershipID
}

func (s *AuthorizationSnapshot) CallerScope() string {
	if s == nil {
		return ""
	}
	return s.callerScope
}

// AuthIDs returns a stable copy of the allowed credential identifiers.
func (s *AuthorizationSnapshot) AuthIDs() []string {
	if s == nil {
		return nil
	}
	ids := make([]string, 0, len(s.accounts))
	for authID := range s.accounts {
		ids = append(ids, authID)
	}
	sort.Strings(ids)
	return ids
}

// Account returns a copy of the frozen assignment for an allowed AuthID.
func (s *AuthorizationSnapshot) Account(authID string) (AccountSnapshot, bool) {
	if s == nil {
		return AccountSnapshot{}, false
	}
	account, ok := s.accounts[strings.TrimSpace(authID)]
	return account, ok
}

// WithAuthorization adds an immutable carpool snapshot to a request context.
func WithAuthorization(ctx context.Context, snapshot *AuthorizationSnapshot) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if snapshot == nil {
		return ctx
	}
	if _, exists := AuthorizationFromContext(ctx); exists {
		return ctx
	}
	ctx = context.WithValue(ctx, authorizationContextKey{}, snapshot)
	if snapshot.prices != nil {
		ctx = executor.WithRequestValidator(ctx, func(_ context.Context, provider string, req executor.Request) error {
			model := thinking.ParseSuffix(req.Model).ModelName
			if _, ok := snapshot.prices.Lookup(model); !ok {
				return &executor.RequestValidationError{Code: "model_price_not_configured", Message: "Model price is not configured", HTTPStatus: 422}
			}
			return nil
		})
	}
	return ctx
}

// AuthorizationFromContext returns the request's carpool authorization snapshot.
func AuthorizationFromContext(ctx context.Context) (*AuthorizationSnapshot, bool) {
	if ctx == nil {
		return nil, false
	}
	snapshot, ok := ctx.Value(authorizationContextKey{}).(*AuthorizationSnapshot)
	return snapshot, ok && snapshot != nil
}

// Prices returns the read-only catalog captured at authorization.
func (s *AuthorizationSnapshot) Prices() *pricing.Snapshot {
	if s == nil {
		return nil
	}
	return s.prices
}
