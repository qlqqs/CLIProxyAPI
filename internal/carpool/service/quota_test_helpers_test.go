package service

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/pricing"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type quotaServicePrices struct {
	value atomic.Pointer[pricing.Catalog]
}

func (p *quotaServicePrices) Current() *pricing.Catalog { return p.value.Load() }

type quotaServiceFixture struct {
	store      *carpoolsqlite.Store
	control    *Control
	manager    *coreauth.Manager
	admin      domain.User
	car        domain.Car
	users      []domain.User
	members    []domain.Membership
	keys       [][]domain.APIKey
	assignment domain.AuthAssignment
	clock      atomic.Int64
	prices     quotaServicePrices
	unhealthy  atomic.Bool
}

func (f *quotaServiceFixture) now() time.Time { return time.UnixMicro(f.clock.Load()).UTC() }

func newQuotaServiceFixture(t *testing.T) *quotaServiceFixture {
	t.Helper()
	ctx := context.Background()
	f := &quotaServiceFixture{}
	f.clock.Store(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).UnixMicro())
	store, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: filepath.Join(t.TempDir(), "quota-service.db"), Now: f.now, MaxOpenConnections: 1})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	f.store = store
	t.Cleanup(func() {
		if f.control != nil {
			f.control.CloseAdmission()
			synctest.Wait()
		}
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	var err error
	f.admin, err = store.BootstrapAdmin(ctx, domain.User{UserRef: quotaServiceRef(t, "usr"), Username: "admin", DefaultDisplayName: "Admin", Role: domain.UserRoleAdmin, Status: domain.UserStatusActive, PasswordHash: "test-hash"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.car, err = store.CreateCar(ctx, domain.Car{CarRef: quotaServiceRef(t, "car"), Name: "Quota car", Status: domain.CarStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		name := fmt.Sprintf("passenger-%d", i)
		user, errUser := store.CreateUser(ctx, domain.User{UserRef: quotaServiceRef(t, "usr"), Username: name, DefaultDisplayName: name, Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test-hash"})
		if errUser != nil {
			t.Fatal(errUser)
		}
		limit := int64(100_000_000_000)
		anchor := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		member, errMember := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{MemberRef: quotaServiceRef(t, "mem"), UserID: user.ID, CarID: f.car.ID, DisplayName: name, CreatedByUserID: f.admin.ID, MonthlyLimitNanoUSD: &limit, StartedAt: anchor, BillingAnchorAt: anchor, BillingTimezone: "UTC"}})
		if errMember != nil {
			t.Fatal(errMember)
		}
		var keys []domain.APIKey
		for j := range 2 {
			secret, errSecret := NewUserAPIKeySecret(nil)
			if errSecret != nil {
				t.Fatal(errSecret)
			}
			key, errKey := store.CreateAPIKey(ctx, domain.APIKey{KeyID: secret.KeyID, UserID: user.ID, Name: fmt.Sprintf("Key %d", j), SecretDigest: secret.Digest})
			if errKey != nil {
				t.Fatal(errKey)
			}
			keys = append(keys, key)
		}
		f.users, f.members, f.keys = append(f.users, user), append(f.members, member), append(f.keys, keys)
	}
	f.manager = coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	if _, errRegister := f.manager.Register(ctx, &coreauth.Auth{ID: "quota-service-auth", Provider: "claude", Status: coreauth.StatusActive}); errRegister != nil {
		t.Fatal(errRegister)
	}
	f.assignment, err = store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AccountRef: quotaServiceRef(t, "acct"), CarID: f.car.ID, AuthID: "quota-service-auth", SafeLabel: "Primary", ProviderSnapshot: "claude", CreatedByUserID: f.admin.ID}})
	if err != nil {
		t.Fatal(err)
	}
	f.setPrices(t, "0.001")
	f.control, err = NewControl(store, f.manager, ControlConfig{
		SessionAbsoluteTTL: time.Hour, SessionIdleTTL: 30 * time.Minute, UsageRetention: 90 * 24 * time.Hour,
		Now: f.now, PasswordHasher: PasswordHasher{Params: testPasswordParams()}, PricingProvider: &f.prices,
		AccountingAdmission: func(context.Context) error {
			if f.unhealthy.Load() {
				return domain.ErrAccountingUnavailable
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *quotaServiceFixture) setPrices(t *testing.T, amount string) *pricing.Catalog {
	t.Helper()
	catalog, errParse := pricing.ParseCatalog([]byte(`{"quota-model":{"input_cost_per_token":`+amount+`,"output_cost_per_token":0}}`), "test")
	if errParse != nil {
		t.Fatal(errParse)
	}
	catalog.LoadedAt = f.now().Format(time.RFC3339)
	f.prices.value.Store(catalog)
	return catalog
}

func (f *quotaServiceFixture) authorize(ctx context.Context, user, key int) (*carpoolruntime.AuthorizationSnapshot, error) {
	return f.control.AuthorizeProxyWithModel(ctx, f.users[user].ID, f.keys[user][key].KeyID, "synthetic-scope", http.MethodPost, "/v1/chat/completions", "quota-model", false)
}

func (f *quotaServiceFixture) admit(t *testing.T, user, key int) *carpoolruntime.AuthorizationSnapshot {
	t.Helper()
	snapshot, errAuthorize := f.authorize(context.Background(), user, key)
	if errAuthorize != nil || snapshot == nil {
		t.Fatalf("authorize = %v, %v", snapshot, errAuthorize)
	}
	t.Cleanup(snapshot.Release)
	return snapshot
}

type quotaServiceAdmission struct {
	snapshot *carpoolruntime.AuthorizationSnapshot
	err      error
}

func (f *quotaServiceFixture) enqueue(t *testing.T, ctx context.Context, user, key int) <-chan quotaServiceAdmission {
	t.Helper()
	before := f.control.concurrency.Snapshot("", "").QueueLength
	result := make(chan quotaServiceAdmission, 1)
	go func() {
		snapshot, errAuthorize := f.authorize(ctx, user, key)
		result <- quotaServiceAdmission{snapshot: snapshot, err: errAuthorize}
	}()
	synctest.Wait()
	if got := f.control.concurrency.Snapshot("", ""); got.QueueLength != before+1 {
		select {
		case early := <-result:
			t.Fatalf("admission did not queue: %v", early.err)
		default:
			t.Fatalf("queue = %+v, want %d waiters", got, before+1)
		}
	}
	return result
}

func takeQuotaServiceAdmission(t *testing.T, result <-chan quotaServiceAdmission) quotaServiceAdmission {
	t.Helper()
	synctest.Wait()
	select {
	case admission := <-result:
		if admission.snapshot != nil {
			t.Cleanup(admission.snapshot.Release)
		}
		return admission
	default:
		t.Fatal("service admission did not finish")
		return quotaServiceAdmission{}
	}
}

func (f *quotaServiceFixture) requests(t *testing.T) []domain.UsageRequestDetail {
	t.Helper()
	rows, errList := f.store.ListUsageRequests(context.Background(), domain.UsageRequestFilter{From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, time.Time{}, "", 100)
	if errList != nil {
		t.Fatal(errList)
	}
	return rows
}

func (f *quotaServiceFixture) assertIdle(t *testing.T) {
	t.Helper()
	synctest.Wait()
	if got := f.control.concurrency.Snapshot("", ""); got.TotalActive != 0 || got.QueueLength != 0 {
		t.Fatalf("admission leaked resources: %+v", got)
	}
}

func (f *quotaServiceFixture) setMemberLimits(t *testing.T, user int, update domain.MemberLimitsUpdate) {
	t.Helper()
	if _, _, errSet := f.control.SetMemberLimits(context.Background(), f.admin, f.car.CarRef, f.members[user].MemberRef, update); errSet != nil {
		t.Fatal(errSet)
	}
}

func (f *quotaServiceFixture) setAccountLimit(t *testing.T, limit *int) {
	t.Helper()
	if errSet := f.control.SetAccountConcurrency(context.Background(), f.admin, f.car.CarRef, f.assignment.AccountRef, limit); errSet != nil {
		t.Fatal(errSet)
	}
}

func (f *quotaServiceFixture) observe(t *testing.T, kind string, reset time.Time) {
	t.Helper()
	auth, _ := f.manager.GetByID(f.assignment.AuthID)
	auth.Quota = coreauth.QuotaState{ObservedAt: f.now(), Signals: map[string]string{"Anthropic-Ratelimit-Unified-" + kind + "-Reset": fmt.Sprint(reset.Unix())}}
	if _, errUpdate := f.manager.Update(context.Background(), auth); errUpdate != nil {
		t.Fatal(errUpdate)
	}
}

func quotaServiceRef(t *testing.T, prefix string) string {
	t.Helper()
	ref, errRef := NewPublicRef(nil, prefix)
	if errRef != nil {
		t.Fatal(errRef)
	}
	return ref
}
