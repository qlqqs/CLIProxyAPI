package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestPublicReferencePaginationAndCounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)

	admin := testUser("admin", domain.UserRoleAdmin)
	admin.UserRef = "usr_AAAAAAAAAAAAAAAA"
	if _, errAdmin := store.BootstrapAdmin(ctx, admin, nil); errAdmin != nil {
		t.Fatalf("BootstrapAdmin() error = %v", errAdmin)
	}
	for _, item := range []struct {
		username string
		userRef  string
	}{
		{username: "charlie", userRef: "usr_DAwMDAwMDAwMDAwM"},
		{username: "bravo", userRef: "usr_CwsLCwsLCwsLCwsL"},
	} {
		user := testUser(item.username, domain.UserRolePassenger)
		user.UserRef = item.userRef
		if _, errCreate := store.CreateUser(ctx, user); errCreate != nil {
			t.Fatalf("CreateUser(%q) error = %v", item.username, errCreate)
		}
	}
	firstUsers, errUsers := store.ListUsers(ctx, "", 2)
	if errUsers != nil || len(firstUsers) != 2 {
		t.Fatalf("ListUsers(first) = (%#v, %v)", firstUsers, errUsers)
	}
	if firstUsers[0].UserRef != "usr_AAAAAAAAAAAAAAAA" || firstUsers[1].UserRef != "usr_CwsLCwsLCwsLCwsL" {
		t.Fatalf("first user page is not ordered by public reference: %#v", firstUsers)
	}
	secondUsers, errUsers := store.ListUsers(ctx, firstUsers[1].UserRef, 2)
	if errUsers != nil || len(secondUsers) != 1 || secondUsers[0].UserRef != "usr_DAwMDAwMDAwMDAwM" {
		t.Fatalf("ListUsers(second) = (%#v, %v)", secondUsers, errUsers)
	}
	if total, errCount := store.CountUsers(ctx); errCount != nil || total != 3 {
		t.Fatalf("CountUsers() = (%d, %v), want (3, nil)", total, errCount)
	}

	passenger := secondUsers[0]
	for _, keyID := range []string{"AAAAAAAAAAAAAAAA", "AQEBAQEBAQEBAQEB", "AgICAgICAgICAgIC"} {
		if _, errCreate := store.CreateAPIKey(ctx, domain.APIKey{KeyID: keyID, UserID: passenger.ID, Name: keyID, SecretDigest: []byte(keyID)}); errCreate != nil {
			t.Fatalf("CreateAPIKey(%q) error = %v", keyID, errCreate)
		}
	}
	firstKeys, errKeys := store.ListAPIKeysForUser(ctx, passenger.ID, "", 2)
	if errKeys != nil || len(firstKeys) != 2 || firstKeys[0].KeyID != "AAAAAAAAAAAAAAAA" || firstKeys[1].KeyID != "AQEBAQEBAQEBAQEB" {
		t.Fatalf("ListAPIKeysForUser(first) = (%#v, %v)", firstKeys, errKeys)
	}
	secondKeys, errKeys := store.ListAPIKeysForUser(ctx, passenger.ID, firstKeys[1].KeyID, 2)
	if errKeys != nil || len(secondKeys) != 1 || secondKeys[0].KeyID != "AgICAgICAgICAgIC" {
		t.Fatalf("ListAPIKeysForUser(second) = (%#v, %v)", secondKeys, errKeys)
	}
	if total, errCount := store.CountAPIKeysForUser(ctx, passenger.ID); errCount != nil || total != 3 {
		t.Fatalf("CountAPIKeysForUser() = (%d, %v), want (3, nil)", total, errCount)
	}

	for _, car := range []domain.Car{
		{CarRef: "car_AAAAAAAAAAAAAAAA", Name: "Alpha", Status: domain.CarStatusActive},
		{CarRef: "car_AQEBAQEBAQEBAQEB", Name: "Beta", Status: domain.CarStatusActive},
		{CarRef: "car_AgICAgICAgICAgIC", Name: "Gamma", Status: domain.CarStatusActive},
	} {
		if _, errCreate := store.CreateCar(ctx, car); errCreate != nil {
			t.Fatalf("CreateCar(%q) error = %v", car.CarRef, errCreate)
		}
	}
	firstCars, errCars := store.ListCars(ctx, "", 2)
	if errCars != nil || len(firstCars) != 2 || firstCars[0].CarRef != "car_AAAAAAAAAAAAAAAA" || firstCars[1].CarRef != "car_AQEBAQEBAQEBAQEB" {
		t.Fatalf("ListCars(first) = (%#v, %v)", firstCars, errCars)
	}
	secondCars, errCars := store.ListCars(ctx, firstCars[1].CarRef, 2)
	if errCars != nil || len(secondCars) != 1 || secondCars[0].CarRef != "car_AgICAgICAgICAgIC" {
		t.Fatalf("ListCars(second) = (%#v, %v)", secondCars, errCars)
	}
	if total, errCount := store.CountCars(ctx); errCount != nil || total != 3 {
		t.Fatalf("CountCars() = (%d, %v), want (3, nil)", total, errCount)
	}
	if car, errCar := store.GetCarByRef(ctx, "car_AQEBAQEBAQEBAQEB"); errCar != nil || car.Name != "Beta" {
		t.Fatalf("GetCarByRef() = (%#v, %v)", car, errCar)
	}
}

func TestAdminUsageGroupsAndFiltersResolvedIDs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 15, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)

	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passengerA, _ := store.CreateUser(ctx, testUser("passenger-a", domain.UserRolePassenger))
	passengerB, _ := store.CreateUser(ctx, testUser("passenger-b", domain.UserRolePassenger))
	keyA, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passengerA.ID, Name: "a", SecretDigest: []byte("a")})
	keyB, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passengerB.ID, Name: "b", SecretDigest: []byte("b")})
	carA, _ := store.CreateCar(ctx, domain.Car{CarRef: "car_AAAAAAAAAAAAAAAA", Name: "Alpha", Status: domain.CarStatusActive})
	carB, _ := store.CreateCar(ctx, domain.Car{CarRef: "car_AQEBAQEBAQEBAQEB", Name: "Beta", Status: domain.CarStatusActive})
	membershipA, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passengerA.ID, CarID: carA.ID, DisplayName: "Passenger A", CreatedByUserID: admin.ID,
	}})
	membershipB, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passengerB.ID, CarID: carB.ID, DisplayName: "Passenger B", CreatedByUserID: admin.ID,
	}})
	assignmentA, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		AccountRef: "acct_AAAAAAAAAAAAAAAA", CarID: carA.ID, AuthID: "auth-a", SafeLabel: "Account A",
		ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	assignmentB, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		AccountRef: "acct_AQEBAQEBAQEBAQEB", CarID: carB.ID, AuthID: "auth-b", SafeLabel: "Account B",
		ProviderSnapshot: "claude", CreatedByUserID: admin.ID,
	}})

	requestA := beginReportRequest(t, ctx, store, "request-a", now, passengerA, keyA, carA, membershipA, assignmentA)
	input, output, cached, reasoning, total := int64(10), int64(4), int64(2), int64(1), int64(14)
	if _, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "usage-a", RequestID: requestA.RequestID, AuthID: assignmentA.AuthID, Provider: assignmentA.ProviderSnapshot,
		UsageKnown: true, InputTokens: &input, OutputTokens: &output, CachedTokens: &cached,
		ReasoningTokens: &reasoning, TotalTokens: &total, RequestedAt: now,
	}); errUsage != nil {
		t.Fatalf("InsertUsageEvent() error = %v", errUsage)
	}
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{RequestID: requestA.RequestID, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: now}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest() error = %v", errComplete)
	}
	requestB := beginReportRequest(t, ctx, store, "request-b", now, passengerB, keyB, carB, membershipB, assignmentB)
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{RequestID: requestB.RequestID, Outcome: domain.RequestOutcomeIncomplete, CompletedAt: now}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest(incomplete) error = %v", errComplete)
	}
	if _, errRejected := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		RequestID: "request-rejected", UserID: passengerA.ID, APIKeyID: keyA.KeyID,
		SourceFormat: "openai", StartedAt: now, Outcome: domain.RequestOutcomeRejected,
	}, nil); errRejected != nil {
		t.Fatalf("BeginProxyRequest(rejected) error = %v", errRejected)
	}

	baseFilter := domain.AdminUsageFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)}
	carRows, errCars := store.AdminUsage(ctx, domain.AdminUsageFilter{
		From: baseFilter.From, To: baseFilter.To, GroupBy: domain.AdminUsageGroupCar,
	})
	if errCars != nil || len(carRows) != 3 {
		t.Fatalf("AdminUsage(car) = (%#v, %v), want three groups", carRows, errCars)
	}
	carByRef := adminUsageByRef(carRows)
	if got := carByRef[carA.CarRef]; got.RequestCount != 1 || got.SucceededCount != 1 || got.KnownCachedTokens != 2 || got.KnownReasoningTokens != 1 || got.KnownTotalTokens != 14 {
		t.Fatalf("car A aggregate = %#v", got)
	}
	if got := carByRef[carB.CarRef]; got.RequestCount != 1 || got.IncompleteCount != 1 {
		t.Fatalf("car B aggregate = %#v", got)
	}
	if got := carByRef[""]; got.RequestCount != 1 || got.RejectedCount != 1 {
		t.Fatalf("unattributed car aggregate = %#v", got)
	}

	userRows, errUsers := store.AdminUsage(ctx, domain.AdminUsageFilter{
		From: baseFilter.From, To: baseFilter.To, CarID: carA.ID, GroupBy: domain.AdminUsageGroupUser,
	})
	if errUsers != nil || len(userRows) != 1 || userRows[0].GroupRef != passengerA.UserRef || userRows[0].KnownInputTokens != 10 {
		t.Fatalf("AdminUsage(user filtered by car) = (%#v, %v)", userRows, errUsers)
	}
	accountRows, errAccounts := store.AdminUsage(ctx, domain.AdminUsageFilter{
		From: baseFilter.From, To: baseFilter.To, AssignmentID: assignmentA.ID, GroupBy: domain.AdminUsageGroupAccount,
	})
	if errAccounts != nil || len(accountRows) != 1 || accountRows[0].GroupRef != assignmentA.AccountRef || accountRows[0].Provider != "codex" || accountRows[0].RequestCount != 1 {
		t.Fatalf("AdminUsage(account filter) = (%#v, %v)", accountRows, errAccounts)
	}
	if assignment, errAssignment := store.GetAuthAssignmentByRef(ctx, assignmentA.AccountRef); errAssignment != nil || assignment.AuthID != assignmentA.AuthID {
		t.Fatalf("GetAuthAssignmentByRef() = (%#v, %v)", assignment, errAssignment)
	}

	_, errInvalid := store.AdminUsage(ctx, domain.AdminUsageFilter{From: baseFilter.To, To: baseFilter.From, GroupBy: domain.AdminUsageGroupUser})
	if !errors.Is(errInvalid, domain.ErrInvalid) {
		t.Fatalf("AdminUsage(invalid range) error = %v, want ErrInvalid", errInvalid)
	}
}

func beginReportRequest(t *testing.T, ctx context.Context, store *Store, requestID string, startedAt time.Time, user domain.User, key domain.APIKey, car domain.Car, membership domain.Membership, assignment domain.AuthAssignment) domain.ProxyRequest {
	t.Helper()
	request, errRequest := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		RequestID: requestID, UserID: user.ID, APIKeyID: key.KeyID, CarID: car.ID,
		MembershipID: membership.ID, MemberRefSnapshot: membership.MemberRef,
		DisplayNameSnapshot: membership.DisplayName, ScopeHash: "scope", ScopeSize: 1,
		SourceFormat: "openai", StartedAt: startedAt, Outcome: domain.RequestOutcomeInProgress,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: assignment.AuthID, AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	if errRequest != nil {
		t.Fatalf("BeginProxyRequest(%q) error = %v", requestID, errRequest)
	}
	return request
}

func adminUsageByRef(rows []domain.AdminUsageAggregate) map[string]domain.AdminUsageAggregate {
	result := make(map[string]domain.AdminUsageAggregate, len(rows))
	for _, row := range rows {
		result[row.GroupRef] = row
	}
	return result
}
