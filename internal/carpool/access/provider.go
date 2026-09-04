package access

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	log "github.com/sirupsen/logrus"
)

const (
	ProviderType = "carpool-user-key"
	ProviderName = "carpool-user-key"

	MetadataUserID   = "carpool_user_id"
	MetadataAPIKeyID = "carpool_api_key_id"
	MetadataRole     = "carpool_role"
)

// Repository is the credential state needed to authenticate a carpool user key.
type Repository interface {
	GetAPIKey(context.Context, string) (domain.APIKey, error)
	GetUser(context.Context, string) (domain.User, error)
	InsertAuditEvent(context.Context, domain.AuditEvent) (domain.AuditEvent, error)
}

// Provider authenticates versioned carpool user API keys.
type Provider struct {
	repository Repository
	now        func() time.Time
}

// NewProvider creates a carpool user-key access provider.
func NewProvider(repository Repository, now func() time.Time) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{repository: repository, now: now}
}

func (p *Provider) Identifier() string {
	return ProviderName
}

func (p *Provider) Authenticate(ctx context.Context, request *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil || p.repository == nil {
		return nil, sdkaccess.NewInternalAuthError("Authentication service unavailable", nil)
	}
	credential, present, conflict := UniqueProxyCredential(request)
	if conflict {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	if !present {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	if !strings.HasPrefix(credential, carpoolservice.UserAPIKeyPrefix) {
		return nil, sdkaccess.NewNotHandledError()
	}

	keyID, digest, errParse := carpoolservice.ParseUserAPIKey(credential)
	if errParse != nil {
		return p.rejectCredential(ctx, credential, "malformed_api_key")
	}
	key, errKey := p.repository.GetAPIKey(ctx, keyID)
	if errKey != nil {
		if errors.Is(errKey, domain.ErrNotFound) {
			return p.rejectCredential(ctx, credential, "api_key_not_found")
		}
		return nil, sdkaccess.NewInternalAuthError("Authentication service unavailable", errKey)
	}
	now := p.now().UTC()
	switch {
	case key.RevokedAt != nil:
		return p.rejectCredential(ctx, credential, "api_key_revoked")
	case key.ExpiresAt != nil && !now.Before(*key.ExpiresAt):
		return p.rejectCredential(ctx, credential, "api_key_expired")
	case !carpoolservice.SecretDigestEqual(key.SecretDigest, digest):
		return p.rejectCredential(ctx, credential, "api_key_secret_mismatch")
	}
	user, errUser := p.repository.GetUser(ctx, key.UserID)
	if errUser != nil {
		if errors.Is(errUser, domain.ErrNotFound) {
			return p.rejectCredential(ctx, credential, "user_not_found")
		}
		return nil, sdkaccess.NewInternalAuthError("Authentication service unavailable", errUser)
	}
	if user.Status != domain.UserStatusActive {
		return p.rejectCredential(ctx, credential, "user_disabled")
	}
	if user.Role != domain.UserRolePassenger {
		return p.rejectCredential(ctx, credential, "role_not_passenger")
	}

	return &sdkaccess.Result{
		Provider:  ProviderName,
		Principal: callerScope(user.ID, key.KeyID),
		Metadata: map[string]string{
			MetadataUserID:   user.ID,
			MetadataAPIKeyID: key.KeyID,
			MetadataRole:     string(user.Role),
		},
	}, nil
}

func (p *Provider) rejectCredential(ctx context.Context, credential, reasonCode string) (*sdkaccess.Result, *sdkaccess.AuthError) {
	event := domain.AuditEvent{
		OccurredAt: p.now().UTC(),
		ActorType:  "credential",
		ActorRef:   CredentialFingerprint(credential),
		Action:     "authentication_reject",
		TargetType: "access_provider",
		TargetRef:  ProviderName,
		Result:     "rejected",
		ReasonCode: reasonCode,
	}
	if _, errAudit := p.repository.InsertAuditEvent(ctx, event); errAudit != nil {
		log.WithFields(log.Fields{
			"action": "authentication_reject",
			"reason": "audit_persist_failed",
		}).Warn("carpool authentication audit event was not persisted")
	}
	return nil, sdkaccess.NewInvalidCredentialError()
}

// UniqueProxyCredential returns the single distinct credential supplied in all
// supported proxy credential positions. Empty positions are ignored.
func UniqueProxyCredential(request *http.Request) (credential string, present, conflict bool) {
	for _, candidate := range proxyCredentialCandidates(request) {
		if candidate == "" {
			continue
		}
		if !present {
			credential = candidate
			present = true
			continue
		}
		if candidate != credential {
			return "", true, true
		}
	}
	return credential, present, false
}

// ProxyCredentialSetFingerprint identifies a supplied credential set without exposing its values.
func ProxyCredentialSetFingerprint(request *http.Request) string {
	distinct := make(map[string]struct{})
	for _, credential := range proxyCredentialCandidates(request) {
		if credential != "" {
			distinct[credential] = struct{}{}
		}
	}
	digests := make([]string, 0, len(distinct))
	for credential := range distinct {
		digest := sha256.Sum256([]byte(credential))
		digests = append(digests, hex.EncodeToString(digest[:]))
	}
	sort.Strings(digests)
	return CredentialFingerprint(strings.Join(digests, "\x00"))
}

// CredentialFingerprint creates a bounded irreversible value for security audit events.
func CredentialFingerprint(credential string) string {
	digest := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(digest[:8])
}

func extractAuthorization(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.Fields(value)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return value
}

func proxyCredentialCandidates(request *http.Request) []string {
	if request == nil {
		return nil
	}
	candidates := []string{
		extractAuthorization(request.Header.Get("Authorization")),
		strings.TrimSpace(request.Header.Get("X-Api-Key")),
		strings.TrimSpace(request.Header.Get("X-Goog-Api-Key")),
	}
	if request.URL != nil {
		query := request.URL.Query()
		candidates = append(candidates,
			strings.TrimSpace(query.Get("key")),
			strings.TrimSpace(query.Get("auth_token")),
		)
	}
	return candidates
}

func callerScope(userID, keyID string) string {
	digest := sha256.Sum256([]byte("carpool-user-key\x00" + userID + "\x00" + keyID))
	return "carpool:" + hex.EncodeToString(digest[:16])
}
