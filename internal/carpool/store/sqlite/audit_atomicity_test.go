package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestAtomicAuditFailureRollsBackStateChanges(t *testing.T) {
	now := time.Date(2026, time.September, 4, 18, 0, 0, 0, time.UTC)

	t.Run("create user", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		_, errCreate := store.CreateUser(t.Context(), testUser("rollback-user", domain.UserRolePassenger), invalidAtomicAudit(now))
		assertInvalidAuditError(t, errCreate)

		count, errCount := store.CountUsers(t.Context())
		if errCount != nil || count != 0 {
			t.Fatalf("CountUsers() = (%d, %v), want (0, nil)", count, errCount)
		}
		assertNoAuditEvents(t, store)
	})

	t.Run("create session", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		passenger := seedAtomicAuditPassenger(t, store)
		digest := []byte("create-session-rollback")
		_, errCreate := store.CreateSession(t.Context(), domain.Session{
			ID: "session-create-rollback", UserID: passenger.ID, TokenDigest: digest,
			CSRFToken: "csrf", PasswordVersion: passenger.PasswordVersion, ExpiresAt: now.Add(time.Hour),
		}, invalidAtomicAudit(now))
		assertInvalidAuditError(t, errCreate)

		if _, errGet := store.GetSessionByDigest(t.Context(), digest); !errors.Is(errGet, domain.ErrNotFound) {
			t.Fatalf("GetSessionByDigest() error = %v, want ErrNotFound", errGet)
		}
		assertNoAuditEvents(t, store)
	})

	t.Run("revoke session", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		passenger := seedAtomicAuditPassenger(t, store)
		digest := []byte("revoke-session-rollback")
		session, errCreate := store.CreateSession(t.Context(), domain.Session{
			ID: "session-revoke-rollback", UserID: passenger.ID, TokenDigest: digest,
			CSRFToken: "csrf", PasswordVersion: passenger.PasswordVersion, ExpiresAt: now.Add(time.Hour),
		})
		if errCreate != nil {
			t.Fatalf("CreateSession() error = %v", errCreate)
		}
		errRevoke := store.RevokeSession(t.Context(), session.ID, "logout", now.Add(time.Minute), invalidAtomicAudit(now))
		assertInvalidAuditError(t, errRevoke)

		loaded, errLoad := store.GetSessionByDigest(t.Context(), digest)
		if errLoad != nil {
			t.Fatalf("GetSessionByDigest() error = %v", errLoad)
		}
		if loaded.RevokedAt != nil || loaded.RevokeReason != "" {
			t.Fatalf("session revocation committed despite audit failure: %#v", loaded)
		}
		assertNoAuditEvents(t, store)
	})

	t.Run("create API key", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		passenger := seedAtomicAuditPassenger(t, store)
		keyID := "key-create-rollback"
		_, errCreate := store.CreateAPIKey(t.Context(), domain.APIKey{
			KeyID: keyID, UserID: passenger.ID, Name: "client", SecretDigest: []byte("digest"),
		}, invalidAtomicAudit(now))
		assertInvalidAuditError(t, errCreate)

		if _, errGet := store.GetAPIKey(t.Context(), keyID); !errors.Is(errGet, domain.ErrNotFound) {
			t.Fatalf("GetAPIKey() error = %v, want ErrNotFound", errGet)
		}
		assertNoAuditEvents(t, store)
	})

	t.Run("revoke API key", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		passenger := seedAtomicAuditPassenger(t, store)
		key, errCreate := store.CreateAPIKey(t.Context(), domain.APIKey{
			KeyID: "key-revoke-rollback", UserID: passenger.ID, Name: "client", SecretDigest: []byte("digest"),
		})
		if errCreate != nil {
			t.Fatalf("CreateAPIKey() error = %v", errCreate)
		}
		errRevoke := store.RevokeAPIKey(t.Context(), key.KeyID, "revoked", now.Add(time.Minute), invalidAtomicAudit(now))
		assertInvalidAuditError(t, errRevoke)

		loaded, errLoad := store.GetAPIKey(t.Context(), key.KeyID)
		if errLoad != nil {
			t.Fatalf("GetAPIKey() error = %v", errLoad)
		}
		if loaded.RevokedAt != nil || loaded.RevokeReason != "" {
			t.Fatalf("API key revocation committed despite audit failure: %#v", loaded)
		}
		assertNoAuditEvents(t, store)
	})

	t.Run("revoke all API keys", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		passenger := seedAtomicAuditPassenger(t, store)
		keyIDs := []string{"key-revoke-all-a", "key-revoke-all-b"}
		for _, keyID := range keyIDs {
			if _, errCreate := store.CreateAPIKey(t.Context(), domain.APIKey{
				KeyID: keyID, UserID: passenger.ID, Name: keyID, SecretDigest: []byte(keyID),
			}); errCreate != nil {
				t.Fatalf("CreateAPIKey(%q) error = %v", keyID, errCreate)
			}
		}
		count, errRevoke := store.RevokeAPIKeysForUser(t.Context(), passenger.ID, "revoked_all", now.Add(time.Minute), invalidAtomicAudit(now))
		assertInvalidAuditError(t, errRevoke)
		if count != 0 {
			t.Fatalf("RevokeAPIKeysForUser() count = %d, want 0 after rollback", count)
		}

		for _, keyID := range keyIDs {
			loaded, errLoad := store.GetAPIKey(t.Context(), keyID)
			if errLoad != nil {
				t.Fatalf("GetAPIKey(%q) error = %v", keyID, errLoad)
			}
			if loaded.RevokedAt != nil || loaded.RevokeReason != "" {
				t.Fatalf("API key %q revocation committed despite audit failure: %#v", keyID, loaded)
			}
		}
		assertNoAuditEvents(t, store)
	})

	t.Run("create car", func(t *testing.T) {
		store := openAtomicAuditTestStore(t, now)
		carRef := "car_create_rollback"
		_, errCreate := store.CreateCar(t.Context(), domain.Car{
			CarRef: carRef, Name: "rollback car", Status: domain.CarStatusActive,
		}, invalidAtomicAudit(now))
		assertInvalidAuditError(t, errCreate)

		if _, errGet := store.GetCarByRef(t.Context(), carRef); !errors.Is(errGet, domain.ErrNotFound) {
			t.Fatalf("GetCarByRef() error = %v, want ErrNotFound", errGet)
		}
		assertNoAuditEvents(t, store)
	})
}

func openAtomicAuditTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	t.Cleanup(func() { closeTestStore(t, store) })
	return store
}

func seedAtomicAuditPassenger(t *testing.T, store *Store) domain.User {
	t.Helper()
	passenger, errCreate := store.CreateUser(t.Context(), testUser("atomic-passenger", domain.UserRolePassenger))
	if errCreate != nil {
		t.Fatalf("CreateUser() error = %v", errCreate)
	}
	return passenger
}

func invalidAtomicAudit(now time.Time) *domain.AuditEvent {
	return &domain.AuditEvent{
		OccurredAt: now,
		ActorType:  "user",
		ActorRef:   "usr_safe_actor",
		Action:     "atomic_test",
		TargetType: "test_target",
		Result:     "succeeded",
	}
}

func assertInvalidAuditError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("operation error = %v, want ErrInvalid", err)
	}
}

func assertNoAuditEvents(t *testing.T, store *Store) {
	t.Helper()
	events, errList := store.ListAuditEvents(t.Context(), time.Time{}, "", 10)
	if errList != nil {
		t.Fatalf("ListAuditEvents() error = %v", errList)
	}
	if len(events) != 0 {
		t.Fatalf("audit events = %#v, want none", events)
	}
}
