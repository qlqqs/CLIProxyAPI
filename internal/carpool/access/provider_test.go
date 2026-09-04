package access

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

type providerRepository struct {
	key       domain.APIKey
	user      domain.User
	keyErr    error
	userErr   error
	auditErr  error
	auditSink *[]domain.AuditEvent
}

func (r providerRepository) GetAPIKey(context.Context, string) (domain.APIKey, error) {
	return r.key, r.keyErr
}

func (r providerRepository) GetUser(context.Context, string) (domain.User, error) {
	return r.user, r.userErr
}

func (r providerRepository) InsertAuditEvent(_ context.Context, event domain.AuditEvent) (domain.AuditEvent, error) {
	if r.auditSink != nil {
		*r.auditSink = append(*r.auditSink, event)
	}
	return event, r.auditErr
}

func TestProviderAuthenticatesActivePassengerKey(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	secret, errSecret := carpoolservice.NewUserAPIKeySecret(nil)
	if errSecret != nil {
		t.Fatalf("NewUserAPIKeySecret() error = %v", errSecret)
	}
	repository := providerRepository{
		key:  domain.APIKey{KeyID: secret.KeyID, UserID: "user-1", SecretDigest: secret.Digest},
		user: domain.User{ID: "user-1", Role: domain.UserRolePassenger, Status: domain.UserStatusActive},
	}
	provider := NewProvider(repository, func() time.Time { return now })
	request, errRequest := http.NewRequest(http.MethodPost, "https://proxy.test/v1/chat/completions", nil)
	if errRequest != nil {
		t.Fatalf("NewRequest() error = %v", errRequest)
	}
	request.Header.Set("Authorization", "Bearer "+secret.Token)

	result, authErr := provider.Authenticate(context.Background(), request)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if result.Provider != ProviderName || result.Principal == secret.Token {
		t.Fatalf("Authenticate() result = %#v", result)
	}
	if result.Metadata[MetadataUserID] != "user-1" || result.Metadata[MetadataAPIKeyID] != secret.KeyID {
		t.Fatalf("Authenticate() metadata = %#v", result.Metadata)
	}
}

func TestProviderRejectsRevokedExpiredDisabledAndWrongDigest(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	secret, errSecret := carpoolservice.NewUserAPIKeySecret(nil)
	if errSecret != nil {
		t.Fatalf("NewUserAPIKeySecret() error = %v", errSecret)
	}
	revokedAt := now.Add(-time.Minute)
	expiredAt := now
	tests := []struct {
		name string
		key  domain.APIKey
		user domain.User
	}{
		{name: "revoked", key: domain.APIKey{KeyID: secret.KeyID, UserID: "user-1", SecretDigest: secret.Digest, RevokedAt: &revokedAt}, user: domain.User{ID: "user-1", Role: domain.UserRolePassenger, Status: domain.UserStatusActive}},
		{name: "expired", key: domain.APIKey{KeyID: secret.KeyID, UserID: "user-1", SecretDigest: secret.Digest, ExpiresAt: &expiredAt}, user: domain.User{ID: "user-1", Role: domain.UserRolePassenger, Status: domain.UserStatusActive}},
		{name: "disabled", key: domain.APIKey{KeyID: secret.KeyID, UserID: "user-1", SecretDigest: secret.Digest}, user: domain.User{ID: "user-1", Role: domain.UserRolePassenger, Status: domain.UserStatusDisabled}},
		{name: "administrator", key: domain.APIKey{KeyID: secret.KeyID, UserID: "user-1", SecretDigest: secret.Digest}, user: domain.User{ID: "user-1", Role: domain.UserRoleAdmin, Status: domain.UserStatusActive}},
		{name: "wrong digest", key: domain.APIKey{KeyID: secret.KeyID, UserID: "user-1", SecretDigest: make([]byte, 32)}, user: domain.User{ID: "user-1", Role: domain.UserRolePassenger, Status: domain.UserStatusActive}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			audits := make([]domain.AuditEvent, 0, 1)
			provider := NewProvider(providerRepository{key: test.key, user: test.user, auditSink: &audits}, func() time.Time { return now })
			request, _ := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models?key="+url.QueryEscape(secret.Token), nil)
			_, authErr := provider.Authenticate(context.Background(), request)
			if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
				t.Fatalf("Authenticate() error = %#v, want invalid credential", authErr)
			}
			if len(audits) != 1 {
				t.Fatalf("authentication audit count = %d, want 1", len(audits))
			}
			audit := audits[0]
			if audit.Action != "authentication_reject" || audit.Result != "rejected" || audit.ActorRef != CredentialFingerprint(secret.Token) {
				t.Fatalf("authentication audit = %#v", audit)
			}
			if strings.Contains(audit.ActorRef, secret.Token) || strings.Contains(string(audit.MetadataJSON), secret.Token) {
				t.Fatalf("authentication audit exposes credential: %#v", audit)
			}
		})
	}
}

func TestProviderAuditsMalformedMissingUserAndUnknownKeys(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	secret, errSecret := carpoolservice.NewUserAPIKeySecret(nil)
	if errSecret != nil {
		t.Fatalf("NewUserAPIKeySecret() error = %v", errSecret)
	}
	tests := []struct {
		name       string
		credential string
		repository providerRepository
		wantReason string
	}{
		{name: "malformed", credential: "cpk_v1_invalid", wantReason: "malformed_api_key"},
		{name: "unknown key", credential: secret.Token, repository: providerRepository{keyErr: domain.ErrNotFound}, wantReason: "api_key_not_found"},
		{name: "missing user", credential: secret.Token, repository: providerRepository{
			key: domain.APIKey{KeyID: secret.KeyID, UserID: "missing-user", SecretDigest: secret.Digest}, userErr: domain.ErrNotFound,
		}, wantReason: "user_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			audits := make([]domain.AuditEvent, 0, 1)
			test.repository.auditSink = &audits
			provider := NewProvider(test.repository, func() time.Time { return now })
			request, _ := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
			request.Header.Set("Authorization", "Bearer "+test.credential)
			_, authErr := provider.Authenticate(context.Background(), request)
			if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
				t.Fatalf("Authenticate() error = %#v, want invalid credential", authErr)
			}
			if len(audits) != 1 || audits[0].ReasonCode != test.wantReason || audits[0].OccurredAt != now {
				t.Fatalf("authentication audits = %#v, want reason %q", audits, test.wantReason)
			}
		})
	}
}

func TestProviderAuditFailureDoesNotChangeAuthenticationRejection(t *testing.T) {
	provider := NewProvider(providerRepository{auditErr: errors.New("audit unavailable")}, time.Now)
	request, _ := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
	request.Header.Set("Authorization", "Bearer cpk_v1_invalid")
	_, authErr := provider.Authenticate(context.Background(), request)
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		t.Fatalf("Authenticate() error = %#v, want invalid credential", authErr)
	}
}

func TestProviderAuthenticationErrorClassification(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models", nil)
	provider := NewProvider(providerRepository{}, time.Now)
	if _, authErr := provider.Authenticate(context.Background(), request); !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials) {
		t.Fatalf("missing credential error = %#v", authErr)
	}

	request.Header.Set("Authorization", "Bearer global-key")
	if _, authErr := provider.Authenticate(context.Background(), request); !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNotHandled) {
		t.Fatalf("global credential error = %#v", authErr)
	}

	request.Header.Set("Authorization", "Bearer cpk_v1_invalid")
	if _, authErr := provider.Authenticate(context.Background(), request); !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		t.Fatalf("malformed carpool credential error = %#v", authErr)
	}

	request.Header.Set("Authorization", "Bearer cpk_v1_invalid")
	provider = NewProvider(providerRepository{keyErr: errors.New("database unavailable")}, time.Now)
	// A malformed token is rejected before repository access.
	if _, authErr := provider.Authenticate(context.Background(), request); !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		t.Fatalf("malformed carpool credential error = %#v", authErr)
	}
}

func TestUniqueProxyCredentialDeduplicatesAndRejectsConflicts(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "https://proxy.test/v1/models?key=shared&auth_token=shared", nil)
	request.Header.Set("Authorization", "Bearer shared")
	request.Header.Set("X-Api-Key", "shared")
	request.Header.Set("X-Goog-Api-Key", "shared")
	credential, present, conflict := UniqueProxyCredential(request)
	if credential != "shared" || !present || conflict {
		t.Fatalf("UniqueProxyCredential() = %q, %t, %t", credential, present, conflict)
	}

	request.Header.Set("X-Api-Key", "different")
	credential, present, conflict = UniqueProxyCredential(request)
	if credential != "" || !present || !conflict {
		t.Fatalf("conflicting UniqueProxyCredential() = %q, %t, %t", credential, present, conflict)
	}
}

func TestCredentialFingerprintDoesNotExposeCredential(t *testing.T) {
	credential := "cpk_v1_secret-value"
	fingerprint := CredentialFingerprint(credential)
	if fingerprint == "" || fingerprint == credential || len(fingerprint) != 16 {
		t.Fatalf("CredentialFingerprint() = %q", fingerprint)
	}
}
