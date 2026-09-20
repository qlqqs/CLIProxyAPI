package sqlite

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func quotaTestFixture(t *testing.T, now *time.Time) (*Store, retentionFixture) {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "quota.db"), func() time.Time { return *now })
	t.Cleanup(func() { closeTestStore(t, store) })
	f := newRetentionFixture(t, context.Background(), store)
	monthly := int64(math.MaxInt64)
	if _, err := store.SetMonthlyLimit(context.Background(), f.membership.ID, &monthly, nil); err != nil {
		t.Fatal(err)
	}
	return store, f
}

func quotaAuthorize(t *testing.T, s *Store, f retentionFixture, id string, at time.Time) domain.AuthorizationSnapshot {
	t.Helper()
	snapshot, err := s.AuthorizeAndBeginProxyRequest(context.Background(), quotaAuthorization(f, id, at))
	if err != nil {
		t.Fatalf("authorize %s: %v (%s)", id, err, snapshot.Request.ReasonCode)
	}
	return snapshot
}

func quotaAuthorization(f retentionFixture, id string, at time.Time) domain.ProxyAuthorization {
	return domain.ProxyAuthorization{RequestID: id, UserID: f.user.ID, APIKeyID: f.apiKey.KeyID, SourceFormat: "openai", StartedAt: at, RuntimeAuthIDs: []string{f.assignment.AuthID}}
}

func quotaCharge(t *testing.T, s *Store, f retentionFixture, requestID, eventID string, cost *int64) domain.UsageEvent {
	t.Helper()
	event := domain.UsageEvent{EventID: eventID, RequestID: requestID, AuthID: f.assignment.AuthID, Model: "test", UsageKnown: cost != nil}
	result, err := s.RecordUsageEventBilled(context.Background(), event, "", cost, "", "")
	if err != nil {
		t.Fatalf("charge %s: %v", eventID, err)
	}
	return result
}

func quotaView(t *testing.T, s *Store, f retentionFixture, at time.Time) []domain.MemberQuotaWindow {
	t.Helper()
	result, err := s.MemberQuotaWindows(context.Background(), f.membership.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestQuotaPolicyRestartAndStableAssignment(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	limit := 3
	if err := s.SetAccountConcurrency(ctx, f.assignment.AuthID, &limit, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &limit}, nil); err != nil {
		t.Fatal(err)
	}
	otherCar, err := s.CreateCar(ctx, domain.Car{Name: "other", Status: domain.CarStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AuthID: f.assignment.AuthID, CarID: otherCar.ID, SafeLabel: "other", ProviderSnapshot: "codex", CreatedByUserID: f.membership.CreatedByUserID}, ExpectedCurrentID: f.assignment.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AuthID: f.assignment.AuthID, CarID: f.car.ID, SafeLabel: "returned", ProviderSnapshot: "codex", CreatedByUserID: f.membership.CreatedByUserID}, ExpectedCurrentID: moved.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || got == nil || *got != limit {
		t.Fatalf("stable account: %v %v", got, err)
	}
	key, err := s.CreateAPIKey(ctx, domain.APIKey{UserID: f.user.ID, Name: "second", SecretDigest: []byte("second")})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetUserConcurrency(ctx, key.UserID)
	if err != nil || got == nil || *got != limit {
		t.Fatalf("stable user: %v %v", got, err)
	}
	quotaAuthorize(t, s, f, "persisted", now)
	cost := int64(20)
	quotaCharge(t, s, f, "persisted", "persisted", &cost)
	// Close all connections before reopening to verify actual restart persistence.
	var path string
	var seq int
	var name string
	if err = s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	closeTestStore(t, s)
	reopened := openTestStore(t, path, func() time.Time { return now })
	defer closeTestStore(t, reopened)
	if view := quotaView(t, reopened, f, now)[0]; view.ConfirmedNanoUSD != cost {
		t.Fatalf("restart: %+v", view)
	}
	got, err = reopened.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || got == nil || *got != limit {
		t.Fatalf("restart policy: %v %v", got, err)
	}
	for _, bad := range []int{0, -1} {
		if err = reopened.SetAccountConcurrency(ctx, f.assignment.AuthID, &bad, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid account limit: %v", err)
		}
	}
	if err = reopened.SetAccountConcurrency(ctx, f.assignment.AuthID, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err = reopened.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || got != nil {
		t.Fatalf("clear account: %v %v", got, err)
	}
}
