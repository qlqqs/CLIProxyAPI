package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
)

type usageRequestsFixture struct {
	control     *Control
	store       *carpoolsqlite.Store
	admin, user domain.User
	key         domain.APIKey
	now         *time.Time
}

func newUsageRequestsFixture(t *testing.T) usageRequestsFixture {
	t.Helper()
	now := time.Date(2026, time.September, 14, 23, 59, 0, 0, time.UTC)
	store, errOpen := carpoolsqlite.Open(t.Context(), carpoolsqlite.Config{Path: filepath.Join(t.TempDir(), "usage.db"), Now: func() time.Time { return now }})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	user, errUser := store.CreateUser(t.Context(), domain.User{UserRef: "usr_AAAAAAAAAAAAAAAA", Username: "usage-passenger", DefaultDisplayName: "Passenger", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test-hash"})
	if errUser != nil {
		t.Fatal(errUser)
	}
	key, errKey := store.CreateAPIKey(t.Context(), domain.APIKey{UserID: user.ID, Name: "usage-key", SecretDigest: []byte("test-digest")})
	if errKey != nil {
		t.Fatal(errKey)
	}
	control, errControl := NewControl(store, nil, ControlConfig{SessionAbsoluteTTL: time.Hour, SessionIdleTTL: 30 * time.Minute, UsageRetention: 90 * 24 * time.Hour, ReportLocation: time.UTC, Now: func() time.Time { return now }})
	if errControl != nil {
		t.Fatal(errControl)
	}
	return usageRequestsFixture{control: control, store: store, admin: domain.User{Role: domain.UserRoleAdmin}, user: user, key: key, now: &now}
}

func (f usageRequestsFixture) seed(t *testing.T, id string, started time.Time) {
	t.Helper()
	_, errBegin := f.store.BeginProxyRequest(t.Context(), domain.ProxyRequest{RequestID: id, UserID: f.user.ID, APIKeyID: f.key.KeyID, SourceFormat: "openai", RequestedModel: "wanted", StartedAt: started, Outcome: domain.RequestOutcomeRejected}, nil)
	if errBegin != nil {
		t.Fatal(errBegin)
	}
}

func TestAdminUsageRequestsStablePagesAcrossMidnightAndNewInsert(t *testing.T) {
	f := newUsageRequestsFixture(t)
	started := f.now.Add(-time.Minute)
	for i := 0; i < 55; i++ {
		f.seed(t, fmt.Sprintf("request-%02d", i), started)
	}
	query := UsageRequestQuery{Period: "today"}
	first, errFirst := f.control.AdminUsageRequests(t.Context(), f.admin, query)
	if errFirst != nil || len(first.Items) != 25 || first.Total != 55 || first.NextCursor == "" {
		t.Fatalf("first = %#v, %v", first, errFirst)
	}
	originalTo := *f.now
	*f.now = f.now.Add(2 * time.Minute)
	f.seed(t, "new-request", *f.now)
	query.Cursor = first.NextCursor
	second, errSecond := f.control.AdminUsageRequests(t.Context(), f.admin, query)
	if errSecond != nil || len(second.Items) != 25 || second.Total != 55 || second.NextCursor == "" {
		t.Fatalf("second = %#v, %v", second, errSecond)
	}
	if !second.Period.To.Equal(originalTo) || second.Period != first.Period {
		t.Fatalf("period drift: first=%#v second=%#v", first.Period, second.Period)
	}
	query.Cursor = second.NextCursor
	third, errThird := f.control.AdminUsageRequests(t.Context(), f.admin, query)
	if errThird != nil || len(third.Items) != 5 || third.Total != 55 || third.NextCursor != "" {
		t.Fatalf("third = %#v, %v", third, errThird)
	}
	items := append(append(first.Items, second.Items...), third.Items...)
	for i, item := range items {
		if want := fmt.Sprintf("request-%02d", 54-i); item.Request.RequestID != want {
			t.Fatalf("row %d = %q want %q", i, item.Request.RequestID, want)
		}
	}
}

func TestAdminUsageRequestsCursorBindsEveryFilter(t *testing.T) {
	f := newUsageRequestsFixture(t)
	for i := 0; i < 26; i++ {
		f.seed(t, fmt.Sprintf("request-%02d", i), f.now.Add(-time.Minute))
	}
	first, errFirst := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{})
	if errFirst != nil || first.NextCursor == "" {
		t.Fatalf("first cursor: %#v, %v", first, errFirst)
	}
	changes := map[string]func(*UsageRequestQuery){
		"car":     func(q *UsageRequestQuery) { q.CarRef = "car_AAAAAAAAAAAAAAAA" },
		"account": func(q *UsageRequestQuery) { q.AccountRef = "acct_AAAAAAAAAAAAAAAA" },
		"period":  func(q *UsageRequestQuery) { q.Period = "7d" },
		"user":    func(q *UsageRequestQuery) { q.UserRef = f.user.UserRef },
		"key":     func(q *UsageRequestQuery) { q.APIKeyRef = f.key.KeyID },
		"model":   func(q *UsageRequestQuery) { q.Model = "wanted" },
		"outcome": func(q *UsageRequestQuery) { q.Outcome = "rejected" },
		"billing": func(q *UsageRequestQuery) { q.BillingStatus = "pending" },
		"request": func(q *UsageRequestQuery) { q.RequestID = "request-00" },
		"custom":  func(q *UsageRequestQuery) { from, to := f.now.Add(-time.Hour), *f.now; q.From, q.To = &from, &to },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			q := UsageRequestQuery{Cursor: first.NextCursor}
			change(&q)
			if _, errPage := f.control.AdminUsageRequests(t.Context(), f.admin, q); !errors.Is(errPage, domain.ErrInvalid) {
				t.Fatalf("changed filter accepted: %v", errPage)
			}
		})
	}
	normalized, errNormalized := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{Cursor: first.NextCursor, Period: " today "})
	if errNormalized != nil || len(normalized.Items) != 1 {
		t.Fatalf("equivalent filters rejected: %#v, %v", normalized, errNormalized)
	}
}

func TestAdminUsageRequestsCustomHighWaterAndMalformedCursor(t *testing.T) {
	f := newUsageRequestsFixture(t)
	from, to := f.now.Add(-time.Hour), f.now.Add(time.Hour)
	for i := 0; i < 26; i++ {
		f.seed(t, fmt.Sprintf("request-%02d", i), f.now.Add(-time.Minute))
	}
	q := UsageRequestQuery{From: &from, To: &to}
	first, errFirst := f.control.AdminUsageRequests(t.Context(), f.admin, q)
	if errFirst != nil || first.NextCursor == "" {
		t.Fatalf("first: %#v, %v", first, errFirst)
	}
	if !first.Period.To.Equal(*f.now) {
		t.Fatalf("future upper bound not clamped: %#v", first.Period)
	}
	*f.now = f.now.Add(2 * time.Minute)
	f.seed(t, "newer", f.now.Add(-time.Minute))
	q.Cursor = first.NextCursor
	second, errSecond := f.control.AdminUsageRequests(t.Context(), f.admin, q)
	if errSecond != nil || second.Total != 26 || len(second.Items) != 1 {
		t.Fatalf("second: %#v, %v", second, errSecond)
	}
	to = to.Add(time.Minute)
	if _, errChanged := f.control.AdminUsageRequests(t.Context(), f.admin, q); !errors.Is(errChanged, domain.ErrInvalid) {
		t.Fatalf("changed custom bound accepted: %v", errChanged)
	}
	for _, raw := range []string{"!", base64.RawURLEncoding.EncodeToString([]byte(`{}`)), base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"kind":"usage_requests","started":"2026-09-14T23:58:00Z","request_id":"request-01"}`))} {
		if _, errCursor := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{Cursor: raw}); !errors.Is(errCursor, domain.ErrInvalid) {
			t.Fatalf("bad cursor accepted: %q %v", raw, errCursor)
		}
	}
	if _, errDenied := f.control.AdminUsageRequests(context.Background(), f.user, UsageRequestQuery{}); !errors.Is(errDenied, ErrForbidden) {
		t.Fatalf("passenger list allowed: %v", errDenied)
	}
	if _, errDenied := f.control.AdminUsageRequest(context.Background(), f.user, "request-00"); !errors.Is(errDenied, ErrForbidden) {
		t.Fatalf("passenger detail allowed: %v", errDenied)
	}
}

func TestAdminUsageRequestsCursorRejectsInvalidTupleAndWidenedRange(t *testing.T) {
	f := newUsageRequestsFixture(t)
	for i := 0; i < 26; i++ {
		f.seed(t, fmt.Sprintf("request-%02d", i), f.now.Add(-time.Minute))
	}
	first, errFirst := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{})
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	original, errDecode := decodeUsageCursor(first.NextCursor)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	changes := map[string]func(*usageCursor){
		"zero-start":     func(c *usageCursor) { c.Started = time.Time{} },
		"empty-id":       func(c *usageCursor) { c.RequestID = "" },
		"control-id":     func(c *usageCursor) { c.RequestID = "request\n01" },
		"before-range":   func(c *usageCursor) { c.Started = c.From.Add(-time.Microsecond) },
		"exclusive-to":   func(c *usageCursor) { c.Started = c.To },
		"reversed-range": func(c *usageCursor) { c.From = c.To.Add(time.Hour) },
		"widen-today":    func(c *usageCursor) { c.From = c.From.Add(-24 * time.Hour) },
		"future-to":      func(c *usageCursor) { c.To = c.To.Add(time.Minute) },
		"expired": func(c *usageCursor) {
			c.From = c.From.AddDate(0, -6, 0)
			c.To = c.To.AddDate(0, -6, 0)
			c.Started = c.Started.AddDate(0, -6, 0)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cursor := original
			change(&cursor)
			if _, errPage := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{Cursor: encodeUsageCursor(cursor)}); !errors.Is(errPage, domain.ErrInvalid) {
				t.Fatalf("invalid decoded cursor accepted: %v", errPage)
			}
		})
	}
	// A stricter live policy must not be bypassed by an earlier cursor.
	*f.now = f.now.AddDate(0, 0, 91)
	if _, errExpired := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{Cursor: first.NextCursor}); !errors.Is(errExpired, domain.ErrInvalid) {
		t.Fatalf("expired cursor accepted: %v", errExpired)
	}
}

type unavailableUsageRetentionRepository struct {
	Repository
	listCalled bool
}

func (*unavailableUsageRetentionRepository) GetRetentionSettings(context.Context, int64) (domain.RetentionSettings, error) {
	return domain.RetentionSettings{}, domain.ErrBusy
}

func (r *unavailableUsageRetentionRepository) ListUsageRequests(context.Context, domain.UsageRequestFilter, time.Time, string, int) ([]domain.UsageRequestDetail, error) {
	r.listCalled = true
	return nil, nil
}

func TestAdminUsageRequestsRetentionReadFailureDoesNotFallBack(t *testing.T) {
	f := newUsageRequestsFixture(t)
	repository := &unavailableUsageRetentionRepository{Repository: f.store}
	f.control.repository = repository
	if _, errPage := f.control.AdminUsageRequests(t.Context(), f.admin, UsageRequestQuery{}); !errors.Is(errPage, domain.ErrBusy) {
		t.Fatalf("retention failure silently fell back: %v", errPage)
	}
	if repository.listCalled {
		t.Fatal("queried request details after retention read failure")
	}
}

func TestAdminUsageRequestsResolvesPublicIdentityFilters(t *testing.T) {
	f := newUsageRequestsFixture(t)
	admin, errAdmin := f.store.BootstrapAdmin(t.Context(), domain.User{Username: "operator", DefaultDisplayName: "Operator", Role: domain.UserRoleAdmin, Status: domain.UserStatusActive, PasswordHash: "test-hash"}, nil)
	if errAdmin != nil {
		t.Fatal(errAdmin)
	}
	car, errCar := f.store.CreateCar(t.Context(), domain.Car{CarRef: "car_AAAAAAAAAAAAAAAA", Name: "Car", Status: domain.CarStatusActive})
	if errCar != nil {
		t.Fatal(errCar)
	}
	member, errMember := f.store.MoveMembership(t.Context(), domain.MembershipMove{Membership: domain.Membership{UserID: f.user.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID}})
	if errMember != nil {
		t.Fatal(errMember)
	}
	account, errAccount := f.store.MoveAuthAssignment(t.Context(), domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AccountRef: "acct_AAAAAAAAAAAAAAAA", CarID: car.ID, AuthID: "test-auth", SafeLabel: "Account", ProviderSnapshot: "codex", CreatedByUserID: admin.ID}})
	if errAccount != nil {
		t.Fatal(errAccount)
	}
	_, errBegin := f.store.BeginProxyRequest(t.Context(), domain.ProxyRequest{RequestID: "resolved-request", UserID: f.user.ID, APIKeyID: f.key.KeyID, CarID: car.ID, MembershipID: member.ID, MemberRefSnapshot: member.MemberRef, DisplayNameSnapshot: member.DisplayName, ScopeHash: "test-scope", ScopeSize: 1, SourceFormat: "openai", StartedAt: f.now.Add(-time.Minute), Outcome: domain.RequestOutcomeInProgress}, []domain.ProxyRequestAuthScope{{AuthID: account.AuthID, AssignmentID: account.ID, AccountRefSnapshot: account.AccountRef, SafeLabelSnapshot: account.SafeLabel, ProviderSnapshot: "codex"}})
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	for i, model := range []string{"actual", "different-event-model"} {
		_, errEvent := f.store.InsertUsageEvent(t.Context(), domain.UsageEvent{EventID: fmt.Sprintf("event-%d", i), RequestID: "resolved-request", AuthID: account.AuthID, Provider: "codex", Model: model, RequestedAt: *f.now})
		if errEvent != nil {
			t.Fatal(errEvent)
		}
	}
	if errComplete := f.store.CompleteProxyRequest(t.Context(), domain.RequestCompletion{RequestID: "resolved-request", RequestedModel: "alias", Outcome: domain.RequestOutcomeSucceeded, CompletedAt: *f.now}); errComplete != nil {
		t.Fatal(errComplete)
	}
	query := UsageRequestQuery{CarRef: " " + car.CarRef + " ", UserRef: f.user.UserRef, AccountRef: account.AccountRef, APIKeyRef: f.key.KeyID, Model: " actual ", Outcome: "succeeded", BillingStatus: "pending", RequestID: "resolved-request"}
	page, errPage := f.control.AdminUsageRequests(t.Context(), admin, query)
	if errPage != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].EventCount != 2 {
		t.Fatalf("resolved AND filters: %#v, %v", page, errPage)
	}
	detail, errDetail := f.control.AdminUsageRequest(t.Context(), admin, "resolved-request")
	if errDetail != nil || len(detail.Events) != 2 || detail.Events[1].Model != "different-event-model" {
		t.Fatalf("resolved expansion: %#v, %v", detail, errDetail)
	}
}
