package runtime

import (
	"context"
	"sort"
	"strings"
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
}

// NewAuthorizationSnapshot copies all values so later assignment changes cannot expand a request.
func NewAuthorizationSnapshot(requestID, userID, apiKeyID, carID, membershipID, callerScope string, accounts map[string]AccountSnapshot) *AuthorizationSnapshot {
	copied := make(map[string]AccountSnapshot, len(accounts))
	for authID, account := range accounts {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		account.AuthID = authID
		copied[authID] = account
	}
	return &AuthorizationSnapshot{
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
	return context.WithValue(ctx, authorizationContextKey{}, snapshot)
}

// AuthorizationFromContext returns the request's carpool authorization snapshot.
func AuthorizationFromContext(ctx context.Context) (*AuthorizationSnapshot, bool) {
	if ctx == nil {
		return nil, false
	}
	snapshot, ok := ctx.Value(authorizationContextKey{}).(*AuthorizationSnapshot)
	return snapshot, ok && snapshot != nil
}
