package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	ErrUnauthenticated = errors.New("carpool: unauthenticated")
	ErrForbidden       = errors.New("carpool: forbidden")
	ErrRateLimited     = errors.New("carpool: rate limited")
)

// LoginRateLimitError carries a safe retry delay for an exhausted login bucket.
type LoginRateLimitError struct {
	RetryAfter time.Duration
}

func (e *LoginRateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *LoginRateLimitError) Unwrap() error { return ErrRateLimited }

const (
	defaultListLimit      = 50
	maximumListLimit      = 200
	lastSeenTouchPeriod   = time.Minute
	defaultStatusMaxAge   = 5 * time.Minute
	loginAuditActorType   = "anonymous"
	sessionAuditActorType = "user"
)

// Repository is the persistence contract used by the carpool business layer.
type Repository interface {
	BootstrapAdmin(context.Context, domain.User, *domain.AuditEvent) (domain.User, error)
	CreateUser(context.Context, domain.User, ...*domain.AuditEvent) (domain.User, error)
	GetUser(context.Context, string) (domain.User, error)
	GetUserByNormalizedUsername(context.Context, string) (domain.User, error)
	GetUserByRef(context.Context, string) (domain.User, error)
	ListUsers(context.Context, string, int) ([]domain.User, error)
	CountUsers(context.Context) (int64, error)
	UpdateUser(context.Context, domain.User, *domain.AuditEvent) (domain.User, error)

	CreateSession(context.Context, domain.Session, ...*domain.AuditEvent) (domain.Session, error)
	GetSessionByDigest(context.Context, []byte) (domain.Session, error)
	RevokeSession(context.Context, string, string, time.Time, ...*domain.AuditEvent) error
	TouchSessionLastSeen(context.Context, string, time.Time, time.Duration) (bool, error)

	CreateAPIKey(context.Context, domain.APIKey, ...*domain.AuditEvent) (domain.APIKey, error)
	GetAPIKey(context.Context, string) (domain.APIKey, error)
	ListAPIKeysForUser(context.Context, string, string, int) ([]domain.APIKey, error)
	CountAPIKeysForUser(context.Context, string) (int64, error)
	RevokeAPIKey(context.Context, string, string, time.Time, ...*domain.AuditEvent) error
	RevokeAPIKeysForUser(context.Context, string, string, time.Time, ...*domain.AuditEvent) (int64, error)
	TouchAPIKeyLastUsed(context.Context, string, time.Time, time.Duration) (bool, error)

	CreateCar(context.Context, domain.Car, ...*domain.AuditEvent) (domain.Car, error)
	GetCar(context.Context, string) (domain.Car, error)
	GetCarByRef(context.Context, string) (domain.Car, error)
	ListCars(context.Context, string, int) ([]domain.Car, error)
	CountCars(context.Context) (int64, error)
	UpdateCar(context.Context, domain.Car, *domain.AuditEvent) (domain.Car, error)

	CurrentMembership(context.Context, string) (domain.Membership, error)
	ListCurrentMembershipsByCar(context.Context, string) ([]domain.Membership, error)
	MoveMembership(context.Context, domain.MembershipMove) (domain.Membership, error)
	EndMembership(context.Context, string, string, time.Time, *domain.AuditEvent) error

	CurrentAuthAssignment(context.Context, string) (domain.AuthAssignment, error)
	GetAuthAssignmentByRef(context.Context, string) (domain.AuthAssignment, error)
	ListCurrentAuthAssignmentsByCar(context.Context, string) ([]domain.AuthAssignment, error)
	MoveAuthAssignment(context.Context, domain.AuthAssignmentMove) (domain.AuthAssignment, error)
	EndAuthAssignment(context.Context, string, string, time.Time, *domain.AuditEvent) error

	AuthorizeAndBeginProxyRequest(context.Context, domain.ProxyAuthorization) (domain.AuthorizationSnapshot, error)
	MemberUsageByCar(context.Context, string, time.Time, time.Time) ([]domain.MemberUsageAggregate, error)
	AccountUsageByCar(context.Context, string, time.Time, time.Time) ([]domain.AccountUsageAggregate, error)
	AdminUsage(context.Context, domain.AdminUsageFilter) ([]domain.AdminUsageAggregate, error)
	InsertAuditEvent(context.Context, domain.AuditEvent) (domain.AuditEvent, error)
	ListAuditEvents(context.Context, time.Time, string, int) ([]domain.AuditEvent, error)
}

// AuthCatalog is the safe runtime view needed for assignments and status projection.
type AuthCatalog interface {
	List() []*coreauth.Auth
	GetByID(string) (*coreauth.Auth, bool)
}

// ControlConfig defines stable business behavior for one process lifetime.
type ControlConfig struct {
	SessionAbsoluteTTL time.Duration
	SessionIdleTTL     time.Duration
	ReportLocation     *time.Location
	UsageRetention     time.Duration
	HomeEnabled        bool
	Now                func() time.Time
	Random             io.Reader
	PasswordHasher     PasswordHasher
	LoginLimiter       *LoginLimiter
	CandidateRefKey    []byte
}

// Control coordinates carpool identity, assignment, authorization, and reporting.
type Control struct {
	repository        Repository
	authCatalog       AuthCatalog
	absoluteTTL       time.Duration
	idleTTL           time.Duration
	reportLocation    *time.Location
	usageRetention    time.Duration
	homeEnabled       bool
	now               func() time.Time
	random            io.Reader
	hasher            PasswordHasher
	loginLimiter      *LoginLimiter
	dummyPasswordHash string
	candidateRefKey   []byte
}

// SessionIdentity is a validated browser session and its current user.
type SessionIdentity struct {
	Session domain.Session
	User    domain.User
}

// LoginResult contains the only values needed to set a browser session.
type LoginResult struct {
	Identity SessionIdentity
	Token    string
}

// CreateUserResult carries a generated password that must be shown once.
type CreateUserResult struct {
	User              domain.User
	TemporaryPassword string
}

// CreateAPIKeyResult carries a generated API key that must be shown once.
type CreateAPIKeyResult struct {
	APIKey domain.APIKey
	Token  string
}

// CarSummary joins one car with current relationship counts.
type CarSummary struct {
	Car          domain.Car
	MemberCount  int
	AccountCount int
}

// CarUpdate contains only fields explicitly supplied by an administrator.
type CarUpdate struct {
	Name         *string
	Description  *string
	SeatLimit    *int
	SeatLimitSet bool
	Status       *domain.CarStatus
}

// MemberView joins the immutable reporting identity with aggregate usage.
type MemberView struct {
	Membership domain.Membership
	Usage      domain.MemberUsageAggregate
	Left       bool
}

// AccountView is a safe assignment, health, and usage projection.
type AccountView struct {
	Assignment domain.AuthAssignment
	Health     carpoolruntime.AccountHealth
	Usage      domain.AccountUsageAggregate
}

// AccountCandidate is the safe browser projection of a runtime credential.
type AccountCandidate struct {
	CandidateRef         string
	Provider             string
	Health               carpoolruntime.AccountHealth
	Assigned             bool
	AssignedToCurrentCar bool
}

// Page is a stable public cursor page with an exact collection total.
type Page[T any] struct {
	Items      []T
	NextCursor string
	Total      int64
}

// AdminUsageQuery contains public filters accepted by the administrator API.
type AdminUsageQuery struct {
	Period     string
	From       *time.Time
	To         *time.Time
	CarRef     string
	UserRef    string
	AccountRef string
	GroupBy    domain.AdminUsageGroup
}

// AdminUsageReport is a resolved administrator usage report.
type AdminUsageReport struct {
	Period          ReportPeriod
	RetentionCutoff time.Time
	GroupBy         domain.AdminUsageGroup
	Items           []domain.AdminUsageAggregate
}

// AuditListQuery supports the opaque cursor and the legacy tuple cursor.
type AuditListQuery struct {
	Cursor   string
	Before   time.Time
	BeforeID string
	Limit    int
}

// NewControl constructs the business layer and validates its fixed dependencies.
func NewControl(repository Repository, authCatalog AuthCatalog, cfg ControlConfig) (*Control, error) {
	if repository == nil {
		return nil, fmt.Errorf("carpool control: repository is required")
	}
	if cfg.SessionAbsoluteTTL <= 0 || cfg.SessionIdleTTL <= 0 || cfg.SessionIdleTTL > cfg.SessionAbsoluteTTL {
		return nil, fmt.Errorf("carpool control: invalid session lifetimes")
	}
	if cfg.ReportLocation == nil {
		cfg.ReportLocation = time.UTC
	}
	if cfg.UsageRetention <= 0 {
		return nil, fmt.Errorf("carpool control: usage retention must be positive")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	if cfg.PasswordHasher.Params == (PasswordParams{}) {
		cfg.PasswordHasher = PasswordHasher{Params: DefaultPasswordParams(), Rand: cfg.Random}
	} else if cfg.PasswordHasher.Rand == nil {
		cfg.PasswordHasher.Rand = cfg.Random
	}
	if cfg.LoginLimiter == nil {
		cfg.LoginLimiter = NewLoginLimiter(LoginLimiterConfig{Now: cfg.Now})
	}
	dummyPasswordHash, errHash := cfg.PasswordHasher.Hash("carpool-dummy-password-never-valid")
	if errHash != nil {
		return nil, fmt.Errorf("carpool control: prepare password verifier: %w", errHash)
	}
	candidateRefKey := append([]byte(nil), cfg.CandidateRefKey...)
	if len(candidateRefKey) == 0 {
		candidateRefKey = make([]byte, sha256.Size)
		if _, errRandom := io.ReadFull(rand.Reader, candidateRefKey); errRandom != nil {
			return nil, fmt.Errorf("carpool control: prepare account candidate references: %w", errRandom)
		}
	}
	if len(candidateRefKey) < sha256.Size {
		return nil, fmt.Errorf("carpool control: account candidate reference key must be at least %d bytes", sha256.Size)
	}
	return &Control{
		repository:        repository,
		authCatalog:       authCatalog,
		absoluteTTL:       cfg.SessionAbsoluteTTL,
		idleTTL:           cfg.SessionIdleTTL,
		reportLocation:    cfg.ReportLocation,
		usageRetention:    cfg.UsageRetention,
		homeEnabled:       cfg.HomeEnabled,
		now:               cfg.Now,
		random:            cfg.Random,
		hasher:            cfg.PasswordHasher,
		loginLimiter:      cfg.LoginLimiter,
		dummyPasswordHash: dummyPasswordHash,
		candidateRefKey:   candidateRefKey,
	}, nil
}

// BootstrapAdmin creates the first administrator without a default credential.
func (c *Control) BootstrapAdmin(ctx context.Context, username, displayName, password string) (domain.User, error) {
	normalized, errUsername := NormalizeUsername(username)
	if errUsername != nil {
		return domain.User{}, errUsername
	}
	displayName, _, errDisplay := NormalizeDisplayName(displayName)
	if errDisplay != nil {
		return domain.User{}, errDisplay
	}
	passwordHash, errHash := c.hasher.Hash(password)
	if errHash != nil {
		return domain.User{}, errHash
	}
	userRef, errRef := NewPublicRef(c.random, "usr")
	if errRef != nil {
		return domain.User{}, errRef
	}
	now := c.currentTime()
	user := domain.User{UserRef: userRef, Username: strings.TrimSpace(username), UsernameNormalized: normalized, DefaultDisplayName: displayName, Role: domain.UserRoleAdmin, Status: domain.UserStatusActive, PasswordHash: passwordHash, PasswordVersion: 1, CreatedAt: now, UpdatedAt: now}
	audit := c.audit("bootstrap", "system", "bootstrap_admin", "user", userRef, "succeeded", "")
	return c.repository.BootstrapAdmin(ctx, user, &audit)
}

// Login verifies a browser credential and creates a revocable server-side session.
func (c *Control) Login(ctx context.Context, username, password, clientAddress string) (LoginResult, error) {
	normalized, errNormalize := NormalizeUsername(username)
	if errNormalize != nil {
		normalized = strings.ToLower(strings.TrimSpace(username))
	}
	limitKey := LoginLimitKey(normalized, clientAddress)
	loginTargetRef := "login_" + limitKey
	if allowed, retryAfter := c.loginLimiter.Allow(limitKey); !allowed {
		c.recordAudit(ctx, c.audit(loginAuditActorType, "rate-limited", "login", "session", loginTargetRef, "rejected", "rate_limited"))
		return LoginResult{}, &LoginRateLimitError{RetryAfter: retryAfter}
	}

	user, errUser := c.repository.GetUserByNormalizedUsername(ctx, normalized)
	hash := c.dummyPasswordHash
	if errUser == nil {
		hash = user.PasswordHash
	} else if !errors.Is(errUser, domain.ErrNotFound) {
		return LoginResult{}, fmt.Errorf("carpool control: read login user: %w", errUser)
	}
	verified, errVerify := c.hasher.Verify(hash, password)
	if errVerify != nil {
		return LoginResult{}, fmt.Errorf("carpool control: verify password: %w", errVerify)
	}
	if errUser != nil || !verified || user.Status != domain.UserStatusActive {
		c.recordAudit(ctx, c.audit(loginAuditActorType, "credential", "login", "session", loginTargetRef, "rejected", "invalid_credentials"))
		return LoginResult{}, ErrUnauthenticated
	}

	secret, errSecret := NewSessionSecret(c.random)
	if errSecret != nil {
		return LoginResult{}, errSecret
	}
	now := c.currentTime()
	sessionInput := domain.Session{ID: uuid.NewString(), UserID: user.ID, TokenDigest: secret.Digest, CSRFToken: secret.CSRFToken, PasswordVersion: user.PasswordVersion, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(c.absoluteTTL)}
	audit := c.audit(sessionAuditActorType, user.UserRef, "login", "session", sessionInput.ID, "succeeded", "")
	session, errSession := c.repository.CreateSession(ctx, sessionInput, &audit)
	if errSession != nil {
		return LoginResult{}, fmt.Errorf("carpool control: create session: %w", errSession)
	}
	c.loginLimiter.Reset(limitKey)
	return LoginResult{Identity: SessionIdentity{Session: session, User: user}, Token: secret.Token}, nil
}

// AuthenticateSession validates revocation, expiry, idle time, password version, and user state.
func (c *Control) AuthenticateSession(ctx context.Context, token string) (SessionIdentity, error) {
	digest, errDigest := SessionTokenDigest(token)
	if errDigest != nil {
		return SessionIdentity{}, ErrUnauthenticated
	}
	session, errSession := c.repository.GetSessionByDigest(ctx, digest)
	if errSession != nil {
		if errors.Is(errSession, domain.ErrNotFound) {
			return SessionIdentity{}, ErrUnauthenticated
		}
		return SessionIdentity{}, fmt.Errorf("carpool control: read session: %w", errSession)
	}
	now := c.currentTime()
	if session.RevokedAt != nil || !now.Before(session.ExpiresAt) || !now.Before(session.LastSeenAt.Add(c.idleTTL)) {
		return SessionIdentity{}, ErrUnauthenticated
	}
	user, errUser := c.repository.GetUser(ctx, session.UserID)
	if errUser != nil {
		if errors.Is(errUser, domain.ErrNotFound) {
			return SessionIdentity{}, ErrUnauthenticated
		}
		return SessionIdentity{}, fmt.Errorf("carpool control: read session user: %w", errUser)
	}
	if user.Status != domain.UserStatusActive || user.PasswordVersion != session.PasswordVersion {
		return SessionIdentity{}, ErrUnauthenticated
	}
	if _, errTouch := c.repository.TouchSessionLastSeen(ctx, session.ID, now, lastSeenTouchPeriod); errTouch != nil {
		return SessionIdentity{}, fmt.Errorf("carpool control: touch session: %w", errTouch)
	}
	return SessionIdentity{Session: session, User: user}, nil
}

func (c *Control) Logout(ctx context.Context, identity SessionIdentity) error {
	now := c.currentTime()
	audit := c.audit(sessionAuditActorType, identity.User.UserRef, "logout", "session", identity.Session.ID, "succeeded", "")
	errRevoke := c.repository.RevokeSession(ctx, identity.Session.ID, "logout", now, &audit)
	if errRevoke != nil && !errors.Is(errRevoke, domain.ErrNotFound) {
		return fmt.Errorf("carpool control: revoke session: %w", errRevoke)
	}
	return nil
}

func (c *Control) ChangePassword(ctx context.Context, identity SessionIdentity, currentPassword, newPassword string) error {
	verified, errVerify := c.hasher.Verify(identity.User.PasswordHash, currentPassword)
	if errVerify != nil || !verified {
		return ErrUnauthenticated
	}
	hash, errHash := c.hasher.Hash(newPassword)
	if errHash != nil {
		return errHash
	}
	user := identity.User
	user.PasswordHash = hash
	user.PasswordVersion++
	user.MustChangePassword = false
	user.UpdatedAt = c.currentTime()
	audit := c.audit(sessionAuditActorType, user.UserRef, "change_password", "user", user.UserRef, "succeeded", "")
	if _, errUpdate := c.repository.UpdateUser(ctx, user, &audit); errUpdate != nil {
		return fmt.Errorf("carpool control: change password: %w", errUpdate)
	}
	return nil
}

func (c *Control) CreateUser(ctx context.Context, actor domain.User, username, displayName string, role domain.UserRole) (CreateUserResult, error) {
	if actor.Role != domain.UserRoleAdmin {
		return CreateUserResult{}, ErrForbidden
	}
	if !role.Valid() {
		return CreateUserResult{}, domain.ErrInvalid
	}
	normalized, errUsername := NormalizeUsername(username)
	if errUsername != nil {
		return CreateUserResult{}, errUsername
	}
	displayName, _, errDisplay := NormalizeDisplayName(displayName)
	if errDisplay != nil {
		return CreateUserResult{}, errDisplay
	}
	temporaryPassword, errPassword := NewTemporaryPassword(c.random)
	if errPassword != nil {
		return CreateUserResult{}, errPassword
	}
	hash, errHash := c.hasher.Hash(temporaryPassword)
	if errHash != nil {
		return CreateUserResult{}, errHash
	}
	userRef, errRef := NewPublicRef(c.random, "usr")
	if errRef != nil {
		return CreateUserResult{}, errRef
	}
	now := c.currentTime()
	userInput := domain.User{UserRef: userRef, Username: strings.TrimSpace(username), UsernameNormalized: normalized, DefaultDisplayName: displayName, Role: role, Status: domain.UserStatusActive, PasswordHash: hash, PasswordVersion: 1, MustChangePassword: true, CreatedAt: now, UpdatedAt: now}
	audit := c.audit(sessionAuditActorType, actor.UserRef, "create_user", "user", userRef, "succeeded", "")
	user, errCreate := c.repository.CreateUser(ctx, userInput, &audit)
	if errCreate != nil {
		return CreateUserResult{}, fmt.Errorf("carpool control: create user: %w", errCreate)
	}
	return CreateUserResult{User: user, TemporaryPassword: temporaryPassword}, nil
}

func (c *Control) ListUsers(ctx context.Context, actor domain.User, cursor string, limit int) (Page[domain.User], error) {
	if actor.Role != domain.UserRoleAdmin {
		return Page[domain.User]{}, ErrForbidden
	}
	afterRef, errCursor := decodePageCursor(cursor, "users")
	if errCursor != nil {
		return Page[domain.User]{}, errCursor
	}
	if afterRef != "" {
		if errRef := validatePublicRef(afterRef, "usr"); errRef != nil {
			return Page[domain.User]{}, errRef
		}
	}
	limit = normalizePageLimit(limit)
	users, errUsers := c.repository.ListUsers(ctx, afterRef, limit+1)
	if errUsers != nil {
		return Page[domain.User]{}, errUsers
	}
	total, errTotal := c.repository.CountUsers(ctx)
	if errTotal != nil {
		return Page[domain.User]{}, errTotal
	}
	page := Page[domain.User]{Items: users, Total: total}
	if len(users) > limit {
		page.Items = users[:limit]
		page.NextCursor = encodePageCursor("users", page.Items[len(page.Items)-1].UserRef)
	}
	return page, nil
}

func (c *Control) UserByRef(ctx context.Context, actor domain.User, userRef string) (domain.User, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.User{}, ErrForbidden
	}
	if errRef := validatePublicRef(userRef, "usr"); errRef != nil {
		return domain.User{}, errRef
	}
	return c.repository.GetUserByRef(ctx, userRef)
}

// UpdateUser changes mutable user fields without allowing role changes.
func (c *Control) UpdateUser(ctx context.Context, actor domain.User, userRef string, defaultDisplayName *string, status *domain.UserStatus) (domain.User, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.User{}, ErrForbidden
	}
	if defaultDisplayName == nil && status == nil {
		return domain.User{}, domain.ErrInvalid
	}
	if errRef := validatePublicRef(userRef, "usr"); errRef != nil {
		return domain.User{}, errRef
	}
	user, errUser := c.repository.GetUserByRef(ctx, userRef)
	if errUser != nil {
		return domain.User{}, errUser
	}
	if defaultDisplayName != nil {
		displayName, _, errDisplay := NormalizeDisplayName(*defaultDisplayName)
		if errDisplay != nil {
			return domain.User{}, errDisplay
		}
		user.DefaultDisplayName = displayName
	}
	if status != nil {
		if !status.Valid() {
			return domain.User{}, domain.ErrInvalid
		}
		if user.ID == actor.ID && *status == domain.UserStatusDisabled {
			return domain.User{}, domain.ErrConflict
		}
		user.Status = *status
	}
	user.UpdatedAt = c.currentTime()
	audit := c.audit(sessionAuditActorType, actor.UserRef, "update_user", "user", user.UserRef, "succeeded", "")
	return c.repository.UpdateUser(ctx, user, &audit)
}

func (c *Control) SetUserStatus(ctx context.Context, actor domain.User, userRef string, status domain.UserStatus) (domain.User, error) {
	return c.UpdateUser(ctx, actor, userRef, nil, &status)
}

func (c *Control) ResetPassword(ctx context.Context, actor domain.User, userRef string) (string, error) {
	if actor.Role != domain.UserRoleAdmin {
		return "", ErrForbidden
	}
	user, errUser := c.repository.GetUserByRef(ctx, userRef)
	if errUser != nil {
		return "", errUser
	}
	temporaryPassword, errPassword := NewTemporaryPassword(c.random)
	if errPassword != nil {
		return "", errPassword
	}
	hash, errHash := c.hasher.Hash(temporaryPassword)
	if errHash != nil {
		return "", errHash
	}
	user.PasswordHash = hash
	user.PasswordVersion++
	user.MustChangePassword = true
	user.UpdatedAt = c.currentTime()
	audit := c.audit(sessionAuditActorType, actor.UserRef, "reset_password", "user", user.UserRef, "succeeded", "")
	if _, errUpdate := c.repository.UpdateUser(ctx, user, &audit); errUpdate != nil {
		return "", errUpdate
	}
	return temporaryPassword, nil
}

func (c *Control) CreateAPIKey(ctx context.Context, user domain.User, name string, expiresAt *time.Time) (CreateAPIKeyResult, error) {
	if user.Role != domain.UserRolePassenger || user.Status != domain.UserStatusActive {
		return CreateAPIKeyResult{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return CreateAPIKeyResult{}, domain.ErrInvalid
	}
	now := c.currentTime()
	if expiresAt != nil {
		expiry := expiresAt.UTC()
		if !expiry.After(now) {
			return CreateAPIKeyResult{}, domain.ErrInvalid
		}
		expiresAt = &expiry
	}
	secret, errSecret := NewUserAPIKeySecret(c.random)
	if errSecret != nil {
		return CreateAPIKeyResult{}, errSecret
	}
	audit := c.audit(sessionAuditActorType, user.UserRef, "create_api_key", "api_key", secret.KeyID, "succeeded", "")
	key, errCreate := c.repository.CreateAPIKey(ctx, domain.APIKey{KeyID: secret.KeyID, UserID: user.ID, Name: name, SecretDigest: secret.Digest, CreatedAt: now, ExpiresAt: expiresAt}, &audit)
	if errCreate != nil {
		return CreateAPIKeyResult{}, errCreate
	}
	return CreateAPIKeyResult{APIKey: key, Token: secret.Token}, nil
}

func (c *Control) ListAPIKeys(ctx context.Context, actor, owner domain.User, cursor string, limit int) (Page[domain.APIKey], error) {
	if actor.Role != domain.UserRoleAdmin && actor.ID != owner.ID {
		return Page[domain.APIKey]{}, ErrForbidden
	}
	afterKeyID, errCursor := decodePageCursor(cursor, "api_keys")
	if errCursor != nil {
		return Page[domain.APIKey]{}, errCursor
	}
	if afterKeyID != "" && !validURLSecret(afterKeyID, 12) {
		return Page[domain.APIKey]{}, domain.ErrInvalid
	}
	limit = normalizePageLimit(limit)
	keys, errKeys := c.repository.ListAPIKeysForUser(ctx, owner.ID, afterKeyID, limit+1)
	if errKeys != nil {
		return Page[domain.APIKey]{}, errKeys
	}
	total, errTotal := c.repository.CountAPIKeysForUser(ctx, owner.ID)
	if errTotal != nil {
		return Page[domain.APIKey]{}, errTotal
	}
	page := Page[domain.APIKey]{Items: keys, Total: total}
	if len(keys) > limit {
		page.Items = keys[:limit]
		page.NextCursor = encodePageCursor("api_keys", page.Items[len(page.Items)-1].KeyID)
	}
	return page, nil
}

func (c *Control) RevokeAPIKey(ctx context.Context, actor, owner domain.User, keyID string) error {
	if actor.Role != domain.UserRoleAdmin && actor.ID != owner.ID {
		return ErrForbidden
	}
	if !validURLSecret(strings.TrimSpace(keyID), 12) {
		return domain.ErrInvalid
	}
	key, errKey := c.repository.GetAPIKey(ctx, keyID)
	if errKey != nil {
		return errKey
	}
	if key.UserID != owner.ID {
		return domain.ErrNotFound
	}
	now := c.currentTime()
	audit := c.audit(sessionAuditActorType, actor.UserRef, "revoke_api_key", "api_key", keyID, "succeeded", "")
	if errRevoke := c.repository.RevokeAPIKey(ctx, keyID, "revoked", now, &audit); errRevoke != nil {
		return errRevoke
	}
	return nil
}

func (c *Control) RevokeAllAPIKeys(ctx context.Context, actor, owner domain.User) error {
	if actor.Role != domain.UserRoleAdmin && actor.ID != owner.ID {
		return ErrForbidden
	}
	now := c.currentTime()
	audit := c.audit(sessionAuditActorType, actor.UserRef, "revoke_all_api_keys", "user", owner.UserRef, "succeeded", "")
	if _, errRevoke := c.repository.RevokeAPIKeysForUser(ctx, owner.ID, "revoked_all", now, &audit); errRevoke != nil {
		return errRevoke
	}
	return nil
}

func (c *Control) CreateCar(ctx context.Context, actor domain.User, name, description string, seatLimit *int) (domain.Car, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.Car{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || len(description) > 500 || (seatLimit != nil && *seatLimit <= 0) {
		return domain.Car{}, domain.ErrInvalid
	}
	carRef, errRef := NewPublicRef(c.random, "car")
	if errRef != nil {
		return domain.Car{}, errRef
	}
	now := c.currentTime()
	carInput := domain.Car{CarRef: carRef, Name: name, Description: strings.TrimSpace(description), SeatLimit: seatLimit, Status: domain.CarStatusActive, CreatedAt: now, UpdatedAt: now}
	audit := c.audit(sessionAuditActorType, actor.UserRef, "create_car", "car", carRef, "succeeded", "")
	car, errCreate := c.repository.CreateCar(ctx, carInput, &audit)
	if errCreate != nil {
		return domain.Car{}, errCreate
	}
	return car, nil
}

func (c *Control) ListCars(ctx context.Context, actor domain.User, cursor string, limit int) (Page[CarSummary], error) {
	if actor.Role != domain.UserRoleAdmin {
		return Page[CarSummary]{}, ErrForbidden
	}
	afterRef, errCursor := decodePageCursor(cursor, "cars")
	if errCursor != nil {
		return Page[CarSummary]{}, errCursor
	}
	if afterRef != "" {
		if errRef := validatePublicRef(afterRef, "car"); errRef != nil {
			return Page[CarSummary]{}, errRef
		}
	}
	limit = normalizePageLimit(limit)
	cars, errCars := c.repository.ListCars(ctx, afterRef, limit+1)
	if errCars != nil {
		return Page[CarSummary]{}, errCars
	}
	total, errTotal := c.repository.CountCars(ctx)
	if errTotal != nil {
		return Page[CarSummary]{}, errTotal
	}
	hasMore := len(cars) > limit
	if hasMore {
		cars = cars[:limit]
	}
	page := Page[CarSummary]{Items: make([]CarSummary, 0, len(cars)), Total: total}
	for _, car := range cars {
		members, errMembers := c.repository.ListCurrentMembershipsByCar(ctx, car.ID)
		if errMembers != nil {
			return Page[CarSummary]{}, errMembers
		}
		accounts, errAccounts := c.repository.ListCurrentAuthAssignmentsByCar(ctx, car.ID)
		if errAccounts != nil {
			return Page[CarSummary]{}, errAccounts
		}
		page.Items = append(page.Items, CarSummary{Car: car, MemberCount: len(members), AccountCount: len(accounts)})
	}
	if hasMore {
		page.NextCursor = encodePageCursor("cars", page.Items[len(page.Items)-1].Car.CarRef)
	}
	return page, nil
}

func (c *Control) UpdateCar(ctx context.Context, actor domain.User, carRef string, update CarUpdate) (domain.Car, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.Car{}, ErrForbidden
	}
	if update.Name == nil && update.Description == nil && !update.SeatLimitSet && update.Status == nil {
		return domain.Car{}, domain.ErrInvalid
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return domain.Car{}, errCar
	}
	if update.Name != nil {
		car.Name = strings.TrimSpace(*update.Name)
	}
	if update.Description != nil {
		car.Description = strings.TrimSpace(*update.Description)
	}
	if update.SeatLimitSet {
		car.SeatLimit = update.SeatLimit
	}
	if update.Status != nil {
		car.Status = *update.Status
	}
	if car.Name == "" || len(car.Name) > 80 || len(car.Description) > 500 || !car.Status.Valid() || (car.SeatLimit != nil && *car.SeatLimit <= 0) {
		return domain.Car{}, domain.ErrInvalid
	}
	car.UpdatedAt = c.currentTime()
	audit := c.audit(sessionAuditActorType, actor.UserRef, "update_car", "car", car.CarRef, "succeeded", "")
	return c.repository.UpdateCar(ctx, car, &audit)
}

func (c *Control) MoveMember(ctx context.Context, actor domain.User, carRef, userRef, displayName string) (domain.Membership, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.Membership{}, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return domain.Membership{}, errCar
	}
	user, errUser := c.repository.GetUserByRef(ctx, userRef)
	if errUser != nil {
		return domain.Membership{}, errUser
	}
	displayName, displayKey, errDisplay := NormalizeDisplayName(displayName)
	if errDisplay != nil {
		return domain.Membership{}, errDisplay
	}
	memberRef, errRef := NewPublicRef(c.random, "mem")
	if errRef != nil {
		return domain.Membership{}, errRef
	}
	expected := ""
	current, errCurrent := c.repository.CurrentMembership(ctx, user.ID)
	if errCurrent == nil {
		expected = current.ID
	} else if !errors.Is(errCurrent, domain.ErrNotFound) {
		return domain.Membership{}, errCurrent
	}
	audit := c.audit(sessionAuditActorType, actor.UserRef, "move_member", "user", user.UserRef, "succeeded", "")
	return c.repository.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{MemberRef: memberRef, UserID: user.ID, CarID: car.ID, DisplayName: displayName, DisplayNameKey: displayKey, StartedAt: c.currentTime(), CreatedByUserID: actor.ID}, ExpectedCurrentID: expected, EndCurrentReason: "moved", Audit: &audit})
}

func (c *Control) RemoveMember(ctx context.Context, actor domain.User, carRef, memberRef string) error {
	if actor.Role != domain.UserRoleAdmin {
		return ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return errCar
	}
	members, errMembers := c.repository.ListCurrentMembershipsByCar(ctx, car.ID)
	if errMembers != nil {
		return errMembers
	}
	for _, member := range members {
		if member.MemberRef != memberRef {
			continue
		}
		audit := c.audit(sessionAuditActorType, actor.UserRef, "remove_member", "membership", member.MemberRef, "succeeded", "")
		return c.repository.EndMembership(ctx, member.ID, "removed", c.currentTime(), &audit)
	}
	return domain.ErrNotFound
}

func (c *Control) ListMembers(ctx context.Context, actor domain.User, carRef string) ([]domain.Membership, error) {
	if actor.Role != domain.UserRoleAdmin {
		return nil, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return nil, errCar
	}
	return c.repository.ListCurrentMembershipsByCar(ctx, car.ID)
}

func (c *Control) MoveAccount(ctx context.Context, actor domain.User, carRef, authID, safeLabel string) (domain.AuthAssignment, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.AuthAssignment{}, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return domain.AuthAssignment{}, errCar
	}
	auth, ok := c.authCatalog.GetByID(strings.TrimSpace(authID))
	if !ok || auth == nil {
		return domain.AuthAssignment{}, domain.ErrNotFound
	}
	safeLabel, safeLabelKey, errLabel := NormalizeDisplayName(safeLabel)
	if errLabel != nil {
		return domain.AuthAssignment{}, errLabel
	}
	accountRef, errRef := NewPublicRef(c.random, "acct")
	if errRef != nil {
		return domain.AuthAssignment{}, errRef
	}
	expected := ""
	current, errCurrent := c.repository.CurrentAuthAssignment(ctx, auth.ID)
	if errCurrent == nil {
		expected = current.ID
	} else if !errors.Is(errCurrent, domain.ErrNotFound) {
		return domain.AuthAssignment{}, errCurrent
	}
	audit := c.audit(sessionAuditActorType, actor.UserRef, "move_account", "account", accountRef, "succeeded", "")
	return c.repository.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AccountRef: accountRef, CarID: car.ID, AuthID: auth.ID, SafeLabel: safeLabel, SafeLabelKey: safeLabelKey, ProviderSnapshot: auth.Provider, StartedAt: c.currentTime(), CreatedByUserID: actor.ID}, ExpectedCurrentID: expected, EndCurrentReason: "moved", Audit: &audit})
}

// MoveAccountCandidate assigns the runtime credential represented by an opaque browser reference.
func (c *Control) MoveAccountCandidate(ctx context.Context, actor domain.User, carRef, candidateRef, safeLabel string) (domain.AuthAssignment, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.AuthAssignment{}, ErrForbidden
	}
	auth, ok := c.authByCandidateRef(candidateRef)
	if !ok {
		return domain.AuthAssignment{}, domain.ErrNotFound
	}
	return c.MoveAccount(ctx, actor, carRef, auth.ID, safeLabel)
}

func (c *Control) RemoveAccount(ctx context.Context, actor domain.User, carRef, accountRef string) error {
	if actor.Role != domain.UserRoleAdmin {
		return ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return errCar
	}
	accounts, errAccounts := c.repository.ListCurrentAuthAssignmentsByCar(ctx, car.ID)
	if errAccounts != nil {
		return errAccounts
	}
	for _, account := range accounts {
		if account.AccountRef != accountRef {
			continue
		}
		audit := c.audit(sessionAuditActorType, actor.UserRef, "remove_account", "account", account.AccountRef, "succeeded", "")
		return c.repository.EndAuthAssignment(ctx, account.ID, "removed", c.currentTime(), &audit)
	}
	return domain.ErrNotFound
}

func (c *Control) ListAccounts(ctx context.Context, actor domain.User, carRef string) ([]domain.AuthAssignment, error) {
	if actor.Role != domain.UserRoleAdmin {
		return nil, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return nil, errCar
	}
	return c.repository.ListCurrentAuthAssignmentsByCar(ctx, car.ID)
}

// ListAccountCandidates returns opaque runtime credential references for the assignment UI.
func (c *Control) ListAccountCandidates(ctx context.Context, actor domain.User, carRef string) ([]AccountCandidate, error) {
	if actor.Role != domain.UserRoleAdmin {
		return nil, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return nil, errCar
	}
	if c.authCatalog == nil {
		return []AccountCandidate{}, nil
	}
	now := c.currentTime()
	auths := c.authCatalog.List()
	result := make([]AccountCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		candidate := AccountCandidate{
			CandidateRef: c.candidateRefForAuthID(auth.ID),
			Provider:     strings.TrimSpace(auth.Provider),
			Health:       carpoolruntime.ProjectAccountHealth(auth, now, defaultStatusMaxAge),
		}
		assignment, errAssignment := c.repository.CurrentAuthAssignment(ctx, auth.ID)
		switch {
		case errAssignment == nil:
			candidate.Assigned = true
			candidate.AssignedToCurrentCar = assignment.CarID == car.ID
		case errors.Is(errAssignment, domain.ErrNotFound):
		default:
			return nil, errAssignment
		}
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].CandidateRef < result[j].CandidateRef
	})
	return result, nil
}

func (c *Control) PassengerCar(ctx context.Context, user domain.User) (domain.Car, domain.Membership, error) {
	if user.Role != domain.UserRolePassenger {
		return domain.Car{}, domain.Membership{}, ErrForbidden
	}
	membership, errMembership := c.repository.CurrentMembership(ctx, user.ID)
	if errMembership != nil {
		return domain.Car{}, domain.Membership{}, errMembership
	}
	car, errCar := c.repository.GetCar(ctx, membership.CarID)
	if errCar != nil {
		return domain.Car{}, domain.Membership{}, errCar
	}
	if car.Status != domain.CarStatusActive {
		return domain.Car{}, domain.Membership{}, ErrForbidden
	}
	return car, membership, nil
}

func (c *Control) PassengerCarSummary(ctx context.Context, user domain.User) (CarSummary, domain.Membership, error) {
	car, membership, errCar := c.PassengerCar(ctx, user)
	if errCar != nil {
		return CarSummary{}, domain.Membership{}, errCar
	}
	members, errMembers := c.repository.ListCurrentMembershipsByCar(ctx, car.ID)
	if errMembers != nil {
		return CarSummary{}, domain.Membership{}, errMembers
	}
	accounts, errAccounts := c.repository.ListCurrentAuthAssignmentsByCar(ctx, car.ID)
	if errAccounts != nil {
		return CarSummary{}, domain.Membership{}, errAccounts
	}
	availableAccounts := 0
	for _, account := range accounts {
		if c.authCatalog == nil {
			continue
		}
		auth, ok := c.authCatalog.GetByID(account.AuthID)
		if ok && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled {
			availableAccounts++
		}
	}
	return CarSummary{Car: car, MemberCount: len(members), AccountCount: availableAccounts}, membership, nil
}

func (c *Control) PassengerMemberUsage(ctx context.Context, user domain.User, periodName string) (ReportPeriod, []domain.MemberUsageAggregate, error) {
	car, membership, errCar := c.PassengerCar(ctx, user)
	if errCar != nil {
		return ReportPeriod{}, nil, errCar
	}
	_ = membership
	period, errPeriod := ResolveReportPeriod(periodName, c.currentTime(), c.reportLocation)
	if errPeriod != nil {
		return ReportPeriod{}, nil, errPeriod
	}
	usage, errUsage := c.repository.MemberUsageByCar(ctx, car.ID, period.From, period.To)
	return period, usage, errUsage
}

func (c *Control) PassengerMembers(ctx context.Context, user domain.User, periodName string) (ReportPeriod, []MemberView, error) {
	car, _, errCar := c.PassengerCar(ctx, user)
	if errCar != nil {
		return ReportPeriod{}, nil, errCar
	}
	period, errPeriod := ResolveReportPeriod(periodName, c.currentTime(), c.reportLocation)
	if errPeriod != nil {
		return ReportPeriod{}, nil, errPeriod
	}
	members, errMembers := c.repository.ListCurrentMembershipsByCar(ctx, car.ID)
	if errMembers != nil {
		return ReportPeriod{}, nil, errMembers
	}
	aggregates, errUsage := c.repository.MemberUsageByCar(ctx, car.ID, period.From, period.To)
	if errUsage != nil {
		return ReportPeriod{}, nil, errUsage
	}
	views := make([]MemberView, 0, len(members)+len(aggregates))
	seen := make(map[string]struct{}, len(members))
	usageByMembership := make(map[string]domain.MemberUsageAggregate, len(aggregates))
	for _, aggregate := range aggregates {
		usageByMembership[aggregate.MembershipID] = aggregate
	}
	for _, member := range members {
		aggregate, ok := usageByMembership[member.ID]
		if !ok {
			aggregate = domain.MemberUsageAggregate{MembershipID: member.ID, MemberRef: member.MemberRef, DisplayName: member.DisplayName}
		}
		views = append(views, MemberView{Membership: member, Usage: aggregate})
		seen[member.ID] = struct{}{}
	}
	for _, aggregate := range aggregates {
		if _, ok := seen[aggregate.MembershipID]; ok {
			continue
		}
		views = append(views, MemberView{Usage: aggregate, Left: true})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Usage.DisplayName < views[j].Usage.DisplayName })
	return period, views, nil
}

func (c *Control) PassengerAccounts(ctx context.Context, user domain.User, periodName string) (ReportPeriod, []AccountView, error) {
	car, _, errCar := c.PassengerCar(ctx, user)
	if errCar != nil {
		return ReportPeriod{}, nil, errCar
	}
	period, errPeriod := ResolveReportPeriod(periodName, c.currentTime(), c.reportLocation)
	if errPeriod != nil {
		return ReportPeriod{}, nil, errPeriod
	}
	assignments, errAssignments := c.repository.ListCurrentAuthAssignmentsByCar(ctx, car.ID)
	if errAssignments != nil {
		return ReportPeriod{}, nil, errAssignments
	}
	aggregates, errUsage := c.repository.AccountUsageByCar(ctx, car.ID, period.From, period.To)
	if errUsage != nil {
		return ReportPeriod{}, nil, errUsage
	}
	usageByAssignment := make(map[string]domain.AccountUsageAggregate, len(aggregates))
	for _, aggregate := range aggregates {
		usageByAssignment[aggregate.AssignmentID] = aggregate
	}
	now := c.currentTime()
	views := make([]AccountView, 0, len(assignments))
	for _, assignment := range assignments {
		var auth *coreauth.Auth
		if c.authCatalog != nil {
			auth, _ = c.authCatalog.GetByID(assignment.AuthID)
		}
		views = append(views, AccountView{Assignment: assignment, Health: carpoolruntime.ProjectAccountHealth(auth, now, defaultStatusMaxAge), Usage: usageByAssignment[assignment.ID]})
	}
	return period, views, nil
}

func (c *Control) AdminUsage(ctx context.Context, actor domain.User, query AdminUsageQuery) (AdminUsageReport, error) {
	if actor.Role != domain.UserRoleAdmin {
		return AdminUsageReport{}, ErrForbidden
	}
	now := c.currentTime()
	retentionCutoff := now.Add(-c.usageRetention)
	var period ReportPeriod
	var errPeriod error
	switch {
	case query.From != nil || query.To != nil:
		if query.From == nil || query.To == nil || strings.TrimSpace(query.Period) != "" {
			return AdminUsageReport{}, domain.ErrInvalid
		}
		period, errPeriod = ValidateCustomReportPeriod(*query.From, *query.To, retentionCutoff)
	default:
		periodName := strings.TrimSpace(query.Period)
		if periodName == "" {
			periodName = "today"
		}
		period, errPeriod = ResolveReportPeriod(periodName, now, c.reportLocation)
		if errPeriod == nil && period.From.Before(retentionCutoff) {
			period.From = retentionCutoff
		}
	}
	if errPeriod != nil {
		return AdminUsageReport{}, errPeriod
	}
	groupBy := query.GroupBy
	if groupBy == "" {
		groupBy = domain.AdminUsageGroupUser
	}
	if !groupBy.Valid() {
		return AdminUsageReport{}, domain.ErrInvalid
	}
	filter := domain.AdminUsageFilter{From: period.From, To: period.To, GroupBy: groupBy}
	if query.CarRef = strings.TrimSpace(query.CarRef); query.CarRef != "" {
		if errRef := validatePublicRef(query.CarRef, "car"); errRef != nil {
			return AdminUsageReport{}, errRef
		}
		car, errCar := c.repository.GetCarByRef(ctx, query.CarRef)
		if errCar != nil {
			return AdminUsageReport{}, errCar
		}
		filter.CarID = car.ID
	}
	if query.UserRef = strings.TrimSpace(query.UserRef); query.UserRef != "" {
		if errRef := validatePublicRef(query.UserRef, "usr"); errRef != nil {
			return AdminUsageReport{}, errRef
		}
		user, errUser := c.repository.GetUserByRef(ctx, query.UserRef)
		if errUser != nil {
			return AdminUsageReport{}, errUser
		}
		filter.UserID = user.ID
	}
	if query.AccountRef = strings.TrimSpace(query.AccountRef); query.AccountRef != "" {
		if errRef := validatePublicRef(query.AccountRef, "acct"); errRef != nil {
			return AdminUsageReport{}, errRef
		}
		assignment, errAssignment := c.repository.GetAuthAssignmentByRef(ctx, query.AccountRef)
		if errAssignment != nil {
			return AdminUsageReport{}, errAssignment
		}
		filter.AssignmentID = assignment.ID
	}
	items, errUsage := c.repository.AdminUsage(ctx, filter)
	if errUsage != nil {
		return AdminUsageReport{}, errUsage
	}
	return AdminUsageReport{Period: period, RetentionCutoff: retentionCutoff, GroupBy: groupBy, Items: items}, nil
}

func (c *Control) ListAuditEvents(ctx context.Context, actor domain.User, query AuditListQuery) (Page[domain.AuditEvent], error) {
	if actor.Role != domain.UserRoleAdmin {
		return Page[domain.AuditEvent]{}, ErrForbidden
	}
	before, beforeID := query.Before.UTC(), strings.TrimSpace(query.BeforeID)
	if strings.TrimSpace(query.Cursor) != "" {
		if !query.Before.IsZero() || beforeID != "" {
			return Page[domain.AuditEvent]{}, domain.ErrInvalid
		}
		var errCursor error
		before, beforeID, errCursor = decodeAuditCursor(query.Cursor)
		if errCursor != nil {
			return Page[domain.AuditEvent]{}, errCursor
		}
	} else if query.Before.IsZero() != (beforeID == "") {
		return Page[domain.AuditEvent]{}, domain.ErrInvalid
	}
	limit := normalizePageLimit(query.Limit)
	events, errEvents := c.repository.ListAuditEvents(ctx, before, beforeID, limit+1)
	if errEvents != nil {
		return Page[domain.AuditEvent]{}, errEvents
	}
	page := Page[domain.AuditEvent]{Items: events}
	if len(events) > limit {
		page.Items = events[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeAuditCursor(last.OccurredAt, last.ID)
	}
	return page, nil
}

// AuthorizeProxy freezes the user's current vehicle and runtime account upper bound.
func (c *Control) AuthorizeProxy(ctx context.Context, userID, apiKeyID, callerScope, method, path string, upgrade bool) (*carpoolruntime.AuthorizationSnapshot, error) {
	policy := carpoolruntime.CarpoolProxyRoutePolicy(method, path, upgrade)
	reasonCode := policy.ReasonCode
	if c.homeEnabled {
		reasonCode = "home_not_supported"
	}
	runtimeAuthIDs := make([]string, 0)
	if reasonCode == "" && c.authCatalog != nil {
		for _, auth := range c.authCatalog.List() {
			if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == coreauth.StatusDisabled {
				continue
			}
			runtimeAuthIDs = append(runtimeAuthIDs, auth.ID)
		}
	}
	requestID := uuid.NewString()
	snapshot, errAuthorize := c.repository.AuthorizeAndBeginProxyRequest(ctx, domain.ProxyAuthorization{RequestID: requestID, UserID: userID, APIKeyID: apiKeyID, SourceFormat: policy.SourceFormat, StartedAt: c.currentTime(), RuntimeAuthIDs: runtimeAuthIDs, PreflightReasonCode: reasonCode})
	if errAuthorize != nil {
		return nil, errAuthorize
	}
	accounts := make(map[string]carpoolruntime.AccountSnapshot, len(snapshot.Scopes))
	for _, scope := range snapshot.Scopes {
		accounts[scope.AuthID] = carpoolruntime.AccountSnapshot{AuthID: scope.AuthID, AssignmentID: scope.AssignmentID, AccountRef: scope.AccountRefSnapshot, SafeLabel: scope.SafeLabelSnapshot, Provider: scope.ProviderSnapshot}
	}
	_, _ = c.repository.TouchAPIKeyLastUsed(ctx, apiKeyID, c.currentTime(), lastSeenTouchPeriod)
	return carpoolruntime.NewAuthorizationSnapshot(snapshot.Request.RequestID, snapshot.User.ID, snapshot.APIKey.KeyID, snapshot.Car.ID, snapshot.Membership.ID, callerScope, accounts), nil
}

func (c *Control) ReportLocationName() string { return c.reportLocation.String() }

func (c *Control) listAllUsers(ctx context.Context) ([]domain.User, error) {
	var result []domain.User
	after := ""
	for {
		page, errPage := c.repository.ListUsers(ctx, after, maximumListLimit)
		if errPage != nil {
			return nil, errPage
		}
		result = append(result, page...)
		if len(page) < maximumListLimit {
			return result, nil
		}
		after = page[len(page)-1].UserRef
	}
}

func (c *Control) listAllCars(ctx context.Context) ([]domain.Car, error) {
	var result []domain.Car
	after := ""
	for {
		page, errPage := c.repository.ListCars(ctx, after, maximumListLimit)
		if errPage != nil {
			return nil, errPage
		}
		result = append(result, page...)
		if len(page) < maximumListLimit {
			return result, nil
		}
		after = page[len(page)-1].CarRef
	}
}

func (c *Control) carByRef(ctx context.Context, carRef string) (domain.Car, error) {
	if errRef := validatePublicRef(carRef, "car"); errRef != nil {
		return domain.Car{}, errRef
	}
	return c.repository.GetCarByRef(ctx, carRef)
}

func (c *Control) audit(actorType, actorRef, action, targetType, targetRef, result, reason string) domain.AuditEvent {
	return domain.AuditEvent{OccurredAt: c.currentTime(), ActorType: actorType, ActorRef: actorRef, Action: action, TargetType: targetType, TargetRef: targetRef, Result: result, ReasonCode: reason, MetadataJSON: json.RawMessage("{}")}
}

func (c *Control) recordAudit(ctx context.Context, event domain.AuditEvent) {
	if _, errAudit := c.repository.InsertAuditEvent(ctx, event); errAudit != nil {
		log.WithFields(log.Fields{
			"action": event.Action,
			"reason": "audit_persist_failed",
		}).Warn("carpool audit event was not persisted")
	}
}

// RecordProxyCredentialConflict stores a bounded authentication rejection without credential material.
func (c *Control) RecordProxyCredentialConflict(ctx context.Context, credentialFingerprint string) {
	if c == nil {
		return
	}
	credentialFingerprint = strings.TrimSpace(credentialFingerprint)
	if credentialFingerprint == "" {
		credentialFingerprint = "unknown"
	}
	c.recordAudit(ctx, c.audit(
		"credential",
		credentialFingerprint,
		"authentication_reject",
		"proxy_credentials",
		"proxy",
		"rejected",
		"conflicting_credentials",
	))
}

func (c *Control) currentTime() time.Time { return c.now().UTC().Truncate(time.Microsecond) }

func (c *Control) candidateRefForAuthID(authID string) string {
	mac := hmac.New(sha256.New, c.candidateRefKey)
	_, _ = mac.Write([]byte(strings.TrimSpace(authID)))
	return "cand_v1_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c *Control) authByCandidateRef(candidateRef string) (*coreauth.Auth, bool) {
	candidateRef = strings.TrimSpace(candidateRef)
	if c.authCatalog == nil || !strings.HasPrefix(candidateRef, "cand_v1_") {
		return nil, false
	}
	for _, auth := range c.authCatalog.List() {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		expected := c.candidateRefForAuthID(auth.ID)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(candidateRef)) == 1 {
			return auth, true
		}
	}
	return nil, false
}

func normalizePageLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maximumListLimit {
		return maximumListLimit
	}
	return limit
}
