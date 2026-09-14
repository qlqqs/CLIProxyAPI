package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestBootstrapAdminIsUniqueUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 10, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)

	const workers = 8
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, errBootstrap := store.BootstrapAdmin(ctx, testUser("admin-"+string(rune('a'+index)), domain.UserRoleAdmin), nil)
			errorsByWorker <- errBootstrap
		}(i)
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)

	var succeeded, alreadyBootstrapped int
	for errBootstrap := range errorsByWorker {
		switch {
		case errBootstrap == nil:
			succeeded++
		case errors.Is(errBootstrap, domain.ErrAlreadyBootstrapped):
			alreadyBootstrapped++
		default:
			t.Fatalf("BootstrapAdmin() unexpected error = %v", errBootstrap)
		}
	}
	if succeeded != 1 || alreadyBootstrapped != workers-1 {
		t.Fatalf("bootstrap results: succeeded=%d already=%d", succeeded, alreadyBootstrapped)
	}
}

func TestMembershipAndAuthMovesUseExpectedCurrentRelationship(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 11, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, errAdmin := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	if errAdmin != nil {
		t.Fatalf("BootstrapAdmin() error = %v", errAdmin)
	}
	passenger, errPassenger := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	if errPassenger != nil {
		t.Fatalf("CreateUser() error = %v", errPassenger)
	}
	carA, _ := store.CreateCar(ctx, domain.Car{Name: "car-a", Status: domain.CarStatusActive})
	carB, _ := store.CreateCar(ctx, domain.Car{Name: "car-b", Status: domain.CarStatusActive})

	membershipMoves := []domain.MembershipMove{
		{Membership: domain.Membership{UserID: passenger.ID, CarID: carA.ID, DisplayName: "Passenger A", CreatedByUserID: admin.ID}},
		{Membership: domain.Membership{UserID: passenger.ID, CarID: carB.ID, DisplayName: "Passenger B", CreatedByUserID: admin.ID}},
	}
	membershipResults := concurrentlyMoveMemberships(ctx, store, membershipMoves)
	assertOneSuccessOneConflict(t, membershipResults)
	currentMembership, errMembership := store.CurrentMembership(ctx, passenger.ID)
	if errMembership != nil {
		t.Fatalf("CurrentMembership() error = %v", errMembership)
	}
	targetCar := carA
	if currentMembership.CarID == carA.ID {
		targetCar = carB
	}
	movedMembership, errMove := store.MoveMembership(ctx, domain.MembershipMove{
		Membership: domain.Membership{
			UserID: passenger.ID, CarID: targetCar.ID, DisplayName: "Moved Passenger", CreatedByUserID: admin.ID,
		},
		ExpectedCurrentID: currentMembership.ID,
	})
	if errMove != nil {
		t.Fatalf("MoveMembership() error = %v", errMove)
	}
	if movedMembership.CarID != targetCar.ID {
		t.Fatalf("moved membership car = %q, want %q", movedMembership.CarID, targetCar.ID)
	}

	assignmentMoves := []domain.AuthAssignmentMove{
		{AuthAssignment: domain.AuthAssignment{CarID: carA.ID, AuthID: "auth-shared", SafeLabel: "Account A", ProviderSnapshot: "codex", CreatedByUserID: admin.ID}},
		{AuthAssignment: domain.AuthAssignment{CarID: carB.ID, AuthID: "auth-shared", SafeLabel: "Account B", ProviderSnapshot: "codex", CreatedByUserID: admin.ID}},
	}
	assignmentResults := concurrentlyMoveAssignments(ctx, store, assignmentMoves)
	assertOneSuccessOneConflict(t, assignmentResults)
	currentAssignment, errAssignment := store.CurrentAuthAssignment(ctx, "auth-shared")
	if errAssignment != nil {
		t.Fatalf("CurrentAuthAssignment() error = %v", errAssignment)
	}
	targetAssignmentCar := carA
	if currentAssignment.CarID == carA.ID {
		targetAssignmentCar = carB
	}
	_, errMoveAssignment := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{
		AuthAssignment: domain.AuthAssignment{
			CarID: targetAssignmentCar.ID, AuthID: "auth-shared", SafeLabel: "Moved Account",
			ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
		},
		ExpectedCurrentID: currentAssignment.ID,
	})
	if errMoveAssignment != nil {
		t.Fatalf("MoveAuthAssignment() error = %v", errMoveAssignment)
	}

	var currentMemberships, currentAssignments int
	if errCount := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memberships WHERE user_id = ? AND ended_at IS NULL", passenger.ID).Scan(&currentMemberships); errCount != nil {
		t.Fatalf("count current memberships: %v", errCount)
	}
	if errCount := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM car_auth_assignments WHERE auth_id = ? AND ended_at IS NULL", "auth-shared").Scan(&currentAssignments); errCount != nil {
		t.Fatalf("count current assignments: %v", errCount)
	}
	if currentMemberships != 1 || currentAssignments != 1 {
		t.Fatalf("current rows: memberships=%d assignments=%d", currentMemberships, currentAssignments)
	}
}

func TestSeatLimitAndDisplayNameConstraints(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	first, _ := store.CreateUser(ctx, testUser("first", domain.UserRolePassenger))
	second, _ := store.CreateUser(ctx, testUser("second", domain.UserRolePassenger))
	limit := 1
	car, _ := store.CreateCar(ctx, domain.Car{Name: "limited", Status: domain.CarStatusActive, SeatLimit: &limit})
	if _, errMove := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: first.ID, CarID: car.ID, DisplayName: "Visible", CreatedByUserID: admin.ID,
	}}); errMove != nil {
		t.Fatalf("first MoveMembership() error = %v", errMove)
	}
	_, errMove := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: second.ID, CarID: car.ID, DisplayName: "Other", CreatedByUserID: admin.ID,
	}})
	if !errors.Is(errMove, domain.ErrConflict) {
		t.Fatalf("second MoveMembership() error = %v, want ErrConflict", errMove)
	}
}

func TestProxyRequestScopeUsageAndCrashRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	key, errKey := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "client", SecretDigest: []byte("digest")})
	if errKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errKey)
	}
	carA, _ := store.CreateCar(ctx, domain.Car{Name: "car-a", Status: domain.CarStatusActive})
	carB, _ := store.CreateCar(ctx, domain.Car{Name: "car-b", Status: domain.CarStatusActive})
	membership, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: carA.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID,
	}})
	assignment, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: carA.ID, AuthID: "auth-1", SafeLabel: "Primary", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	request, errBegin := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		UserID: passenger.ID, APIKeyID: key.KeyID, CarID: carA.ID, MembershipID: membership.ID,
		MemberRefSnapshot: membership.MemberRef, DisplayNameSnapshot: membership.DisplayName,
		ScopeHash: "scope-hash", ScopeSize: 1, SourceFormat: "openai",
		Outcome: domain.RequestOutcomeInProgress,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: "auth-1", AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	if errBegin != nil {
		t.Fatalf("BeginProxyRequest() error = %v", errBegin)
	}

	_, errMove := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{
		AuthAssignment: domain.AuthAssignment{
			CarID: carB.ID, AuthID: "auth-1", SafeLabel: "Moved", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
		},
		ExpectedCurrentID: assignment.ID,
	})
	if errMove != nil {
		t.Fatalf("MoveAuthAssignment() after request error = %v", errMove)
	}
	zero := int64(0)
	event, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "event-1", RequestID: request.RequestID, AuthID: "auth-1", Provider: "ignored",
		Model: "gpt-test", UsageKnown: true, InputTokens: &zero, TotalTokens: &zero,
	})
	if errUsage != nil {
		t.Fatalf("InsertUsageEvent() error = %v", errUsage)
	}
	if event.AssignmentID != assignment.ID || event.AccountRefSnapshot != assignment.AccountRef || event.SafeLabelSnapshot != "Primary" {
		t.Fatalf("usage snapshot drifted after move: %#v", event)
	}
	if _, errDuplicate := store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "event-1", RequestID: request.RequestID, AuthID: "auth-1", Provider: "ignored",
		Model: "gpt-test", UsageKnown: true, InputTokens: &zero, TotalTokens: &zero,
	}); errDuplicate != nil {
		t.Fatalf("idempotent InsertUsageEvent() error = %v", errDuplicate)
	}
	_, errUnknown := store.InsertUsageEvent(ctx, domain.UsageEvent{
		RequestID: request.RequestID, AuthID: "auth-1", Provider: "codex", UsageKnown: false, TotalTokens: &zero,
	})
	if !errors.Is(errUnknown, domain.ErrInvalid) {
		t.Fatalf("unknown usage with tokens error = %v, want ErrInvalid", errUnknown)
	}
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{
		RequestID: request.RequestID, RequestedModel: "gpt-test", Stream: true,
		Outcome: domain.RequestOutcomeSucceeded, StatusClass: "2xx", UpstreamAttempted: true,
	}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest() error = %v", errComplete)
	}
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{
		RequestID: request.RequestID, RequestedModel: "ignored-duplicate", Stream: false,
		Outcome: domain.RequestOutcomeSucceeded, StatusClass: "2xx", UpstreamAttempted: true,
	}); errComplete != nil {
		t.Fatalf("idempotent CompleteProxyRequest() error = %v", errComplete)
	}
	completedRequest, errCompletedRequest := store.GetProxyRequest(ctx, request.RequestID)
	if errCompletedRequest != nil {
		t.Fatalf("GetProxyRequest(completed) error = %v", errCompletedRequest)
	}
	if completedRequest.RequestedModel != "gpt-test" || !completedRequest.Stream {
		t.Fatalf("completed request metadata = model %q stream %t, want gpt-test true", completedRequest.RequestedModel, completedRequest.Stream)
	}
	rejectedAfterBegin, errRejectedBegin := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		UserID: passenger.ID, APIKeyID: key.KeyID, CarID: carA.ID, MembershipID: membership.ID,
		MemberRefSnapshot: membership.MemberRef, DisplayNameSnapshot: membership.DisplayName,
		ScopeHash: "scope-hash", ScopeSize: 1, SourceFormat: "openai", RequestedModel: "gpt-test",
		Outcome: domain.RequestOutcomeInProgress,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: "auth-1", AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	if errRejectedBegin != nil {
		t.Fatalf("BeginProxyRequest(rejected completion) error = %v", errRejectedBegin)
	}
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{
		RequestID: rejectedAfterBegin.RequestID, Outcome: domain.RequestOutcomeRejected,
		StatusClass: "4xx", ReasonCode: "request_interceptor_rejected",
	}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest(rejected) error = %v", errComplete)
	}

	interrupted, errInterrupted := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		UserID: passenger.ID, APIKeyID: key.KeyID, CarID: carA.ID, MembershipID: membership.ID,
		MemberRefSnapshot: membership.MemberRef, DisplayNameSnapshot: membership.DisplayName,
		ScopeHash: "scope-hash", ScopeSize: 1, SourceFormat: "openai", RequestedModel: "gpt-test",
		Outcome: domain.RequestOutcomeInProgress,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: "auth-1", AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	if errInterrupted != nil {
		t.Fatalf("BeginProxyRequest(interrupted) error = %v", errInterrupted)
	}
	recoveredAt := now.Add(time.Minute)
	recovered, errRecover := store.RecoverInterruptedRequests(ctx, recoveredAt)
	if errRecover != nil || recovered != 1 {
		t.Fatalf("RecoverInterruptedRequests() = (%d, %v), want (1, nil)", recovered, errRecover)
	}
	gotInterrupted, errGet := store.GetProxyRequest(ctx, interrupted.RequestID)
	if errGet != nil {
		t.Fatalf("GetProxyRequest() error = %v", errGet)
	}
	if gotInterrupted.Outcome != domain.RequestOutcomeIncomplete || gotInterrupted.ReasonCode != "process_interrupted" || gotInterrupted.CompletedAt == nil || !gotInterrupted.CompletedAt.Equal(recoveredAt) {
		t.Fatalf("recovered request = %#v", gotInterrupted)
	}
}

func TestLoginSessionKeyAndCRUDRepository(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 13, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("Admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("Passenger", domain.UserRolePassenger))

	byName, errByName := store.GetUserByNormalizedUsername(ctx, "  PASSENGER ")
	if errByName != nil || byName.ID != passenger.ID {
		t.Fatalf("GetUserByNormalizedUsername() = (%#v, %v)", byName, errByName)
	}
	byRef, errByRef := store.GetUserByRef(ctx, passenger.UserRef)
	if errByRef != nil || byRef.ID != passenger.ID {
		t.Fatalf("GetUserByRef() = (%#v, %v)", byRef, errByRef)
	}
	users, errUsers := store.ListUsers(ctx, "", 10)
	if errUsers != nil || len(users) != 2 {
		t.Fatalf("ListUsers() = (%d, %v), want (2, nil)", len(users), errUsers)
	}

	session, errSession := store.CreateSession(ctx, domain.Session{
		UserID: passenger.ID, TokenDigest: []byte("session-digest"), CSRFToken: "csrf",
		PasswordVersion: 1, ExpiresAt: now.Add(24 * time.Hour),
	})
	if errSession != nil {
		t.Fatalf("CreateSession() error = %v", errSession)
	}
	touched, errTouch := store.TouchSessionLastSeen(ctx, session.ID, now.Add(30*time.Second), time.Minute)
	if errTouch != nil || touched {
		t.Fatalf("TouchSessionLastSeen(throttled) = (%v, %v), want (false, nil)", touched, errTouch)
	}
	touched, errTouch = store.TouchSessionLastSeen(ctx, session.ID, now.Add(2*time.Minute), time.Minute)
	if errTouch != nil || !touched {
		t.Fatalf("TouchSessionLastSeen(update) = (%v, %v), want (true, nil)", touched, errTouch)
	}
	loadedSession, errLoadedSession := store.GetSessionByDigest(ctx, []byte("session-digest"))
	if errLoadedSession != nil || !loadedSession.LastSeenAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("GetSessionByDigest() = (%#v, %v)", loadedSession, errLoadedSession)
	}

	firstKey, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "one", SecretDigest: []byte("one")})
	secondKey, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "two", SecretDigest: []byte("two")})
	keys, errKeys := store.ListAPIKeysForUser(ctx, passenger.ID, "", 10)
	if errKeys != nil || len(keys) != 2 {
		t.Fatalf("ListAPIKeysForUser() = (%d, %v), want (2, nil)", len(keys), errKeys)
	}
	used, errUsed := store.TouchAPIKeyLastUsed(ctx, firstKey.KeyID, now.Add(time.Minute), 30*time.Second)
	if errUsed != nil || !used {
		t.Fatalf("TouchAPIKeyLastUsed() = (%v, %v), want (true, nil)", used, errUsed)
	}
	if errRevoke := store.RevokeAPIKey(ctx, firstKey.KeyID, "rotated", now.Add(2*time.Minute)); errRevoke != nil {
		t.Fatalf("RevokeAPIKey() error = %v", errRevoke)
	}
	revoked, errRevokeAll := store.RevokeAPIKeysForUser(ctx, passenger.ID, "admin_revoke", now.Add(3*time.Minute))
	if errRevokeAll != nil || revoked != 1 {
		t.Fatalf("RevokeAPIKeysForUser() = (%d, %v), want (1, nil); second=%s", revoked, errRevokeAll, secondKey.KeyID)
	}

	_, errAdminKey := store.CreateAPIKey(ctx, domain.APIKey{UserID: admin.ID, Name: "forbidden", SecretDigest: []byte("admin")})
	if !errors.Is(errAdminKey, domain.ErrConflict) {
		t.Fatalf("CreateAPIKey(admin) error = %v, want ErrConflict", errAdminKey)
	}

	car, _ := store.CreateCar(ctx, domain.Car{Name: "car", Status: domain.CarStatusActive})
	cars, errCars := store.ListCars(ctx, "", 10)
	if errCars != nil || len(cars) != 1 {
		t.Fatalf("ListCars() = (%d, %v), want (1, nil)", len(cars), errCars)
	}
	passenger.DefaultDisplayName = "Updated"
	passenger.Role = domain.UserRoleAdmin
	if _, errRole := store.UpdateUser(ctx, passenger, nil); !errors.Is(errRole, domain.ErrConflict) {
		t.Fatalf("UpdateUser(role change) error = %v, want ErrConflict", errRole)
	}
	passenger.Role = domain.UserRolePassenger
	passenger.Status = domain.UserStatusDisabled
	passenger.UpdatedAt = now.Add(4 * time.Minute)
	updatedPassenger, errUpdateUser := store.UpdateUser(ctx, passenger, nil)
	if errUpdateUser != nil || updatedPassenger.Status != domain.UserStatusDisabled || updatedPassenger.DisabledAt == nil {
		t.Fatalf("UpdateUser(disable) = (%#v, %v)", updatedPassenger, errUpdateUser)
	}
	if _, errTouchRevoked := store.TouchSessionLastSeen(ctx, session.ID, now.Add(5*time.Minute), 0); errTouchRevoked != nil {
		t.Fatalf("TouchSessionLastSeen(revoked) error = %v", errTouchRevoked)
	}
	car.Description = "updated"
	car.Status = domain.CarStatusDisabled
	updatedCar, errUpdateCar := store.UpdateCar(ctx, car, nil)
	if errUpdateCar != nil || updatedCar.Version != 2 || updatedCar.DisabledAt == nil {
		t.Fatalf("UpdateCar() = (%#v, %v)", updatedCar, errUpdateCar)
	}
}

func TestAuthorizeAndBeginProxyRequestFreezesFilteredScope(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 14, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	key, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "proxy", SecretDigest: []byte("digest")})
	car, _ := store.CreateCar(ctx, domain.Car{Name: "car", Status: domain.CarStatusActive})
	otherCar, _ := store.CreateCar(ctx, domain.Car{Name: "other", Status: domain.CarStatusActive})
	limitNanoUSD := int64(25_000_000_000)
	membership, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID,
		MonthlyLimitNanoUSD: &limitNanoUSD,
	}})
	first, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: car.ID, AuthID: "auth-1", SafeLabel: "First", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	second, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: car.ID, AuthID: "auth-2", SafeLabel: "Second", ProviderSnapshot: "claude", CreatedByUserID: admin.ID,
	}})
	_, _ = store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: otherCar.ID, AuthID: "auth-other", SafeLabel: "Other", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})

	snapshot, errAuthorize := store.AuthorizeAndBeginProxyRequest(ctx, domain.ProxyAuthorization{
		RequestID: "authorized-request", UserID: passenger.ID, APIKeyID: key.KeyID,
		SourceFormat: "openai", RequestedModel: "gpt-test", RuntimeAuthIDs: []string{"auth-2", "auth-other", "missing"},
	})
	if errAuthorize != nil {
		t.Fatalf("AuthorizeAndBeginProxyRequest() error = %v", errAuthorize)
	}
	if snapshot.User.UserRef != passenger.UserRef || snapshot.Membership == nil || snapshot.Membership.ID != membership.ID || snapshot.Car == nil || snapshot.Car.ID != car.ID {
		t.Fatalf("authorization attribution = %#v", snapshot)
	}
	if len(snapshot.Scopes) != 1 || snapshot.Scopes[0].AuthID != "auth-2" || snapshot.Scopes[0].AssignmentID != second.ID {
		t.Fatalf("authorization scopes = %#v", snapshot.Scopes)
	}
	if snapshot.Request.ScopeHash == "" || snapshot.Request.ScopeSize != 1 || snapshot.Request.Outcome != domain.RequestOutcomeInProgress {
		t.Fatalf("authorization request = %#v", snapshot.Request)
	}
	persistedScopes, errScopes := store.ListRequestScopes(ctx, snapshot.Request.RequestID)
	if errScopes != nil || len(persistedScopes) != 1 || persistedScopes[0].AuthID != "auth-2" {
		t.Fatalf("ListRequestScopes() = (%#v, %v)", persistedScopes, errScopes)
	}

	_, errMove := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{
		AuthAssignment: domain.AuthAssignment{
			CarID: otherCar.ID, AuthID: "auth-2", SafeLabel: "Moved", ProviderSnapshot: "claude", CreatedByUserID: admin.ID,
		},
		ExpectedCurrentID: second.ID,
	})
	if errMove != nil {
		t.Fatalf("MoveAuthAssignment() error = %v", errMove)
	}
	persistedScopes, _ = store.ListRequestScopes(ctx, snapshot.Request.RequestID)
	if persistedScopes[0].AssignmentID != second.ID || persistedScopes[0].SafeLabelSnapshot != "Second" {
		t.Fatalf("frozen scope changed after assignment move: %#v", persistedScopes[0])
	}
	if _, errSetLegacy := store.SetMonthlyLimit(ctx, membership.ID, nil, nil); errSetLegacy != nil {
		t.Fatalf("SetMonthlyLimit(nil) error = %v", errSetLegacy)
	}
	legacy, errLegacy := store.AuthorizeAndBeginProxyRequest(ctx, domain.ProxyAuthorization{
		RequestID: "legacy-limit-request", UserID: passenger.ID, APIKeyID: key.KeyID,
		SourceFormat: "openai", RuntimeAuthIDs: []string{"auth-1"},
	})
	if !errors.Is(errLegacy, domain.ErrAuthorizationRejected) || legacy.Request.ReasonCode != "quota_not_configured" {
		t.Fatalf("legacy limit authorization = (%#v, %v), want quota_not_configured rejection", legacy.Request, errLegacy)
	}
	if _, errRestore := store.SetMonthlyLimit(ctx, membership.ID, &limitNanoUSD, nil); errRestore != nil {
		t.Fatalf("restore monthly limit error = %v", errRestore)
	}

	rejected, errRejected := store.AuthorizeAndBeginProxyRequest(ctx, domain.ProxyAuthorization{
		RequestID: "rejected-request", UserID: passenger.ID, APIKeyID: key.KeyID,
		SourceFormat: "openai", RuntimeAuthIDs: []string{"not-present"},
	})
	if !errors.Is(errRejected, domain.ErrAuthorizationRejected) {
		t.Fatalf("AuthorizeAndBeginProxyRequest(rejected) error = %v", errRejected)
	}
	if rejected.Request.Outcome != domain.RequestOutcomeRejected || rejected.Request.ReasonCode != "no_available_accounts" || rejected.Request.CompletedAt == nil {
		t.Fatalf("rejected request = %#v", rejected.Request)
	}
	storedRejected, errStored := store.GetProxyRequest(ctx, "rejected-request")
	if errStored != nil || storedRejected.Outcome != domain.RequestOutcomeRejected {
		t.Fatalf("GetProxyRequest(rejected) = (%#v, %v)", storedRejected, errStored)
	}
	preflightRejected, errPreflight := store.AuthorizeAndBeginProxyRequest(ctx, domain.ProxyAuthorization{
		RequestID: "preflight-rejected-request", UserID: passenger.ID, APIKeyID: key.KeyID,
		PreflightReasonCode: "route_not_supported",
	})
	if !errors.Is(errPreflight, domain.ErrAuthorizationRejected) {
		t.Fatalf("AuthorizeAndBeginProxyRequest(preflight rejected) error = %v", errPreflight)
	}
	if preflightRejected.Request.Outcome != domain.RequestOutcomeRejected || preflightRejected.Request.ReasonCode != "route_not_supported" || preflightRejected.Request.SourceFormat != "" {
		t.Fatalf("preflight rejected request = %#v", preflightRejected.Request)
	}
	storedPreflight, errStoredPreflight := store.GetProxyRequest(ctx, "preflight-rejected-request")
	if errStoredPreflight != nil || storedPreflight.Outcome != domain.RequestOutcomeRejected || storedPreflight.ReasonCode != "route_not_supported" {
		t.Fatalf("GetProxyRequest(preflight rejected) = (%#v, %v)", storedPreflight, errStoredPreflight)
	}
	audits, errAudits := store.ListAuditEvents(ctx, time.Time{}, "", 10)
	if errAudits != nil {
		t.Fatalf("ListAuditEvents() error = %v", errAudits)
	}
	if len(audits) != 3 {
		t.Fatalf("authorization audit count = %d, want 3", len(audits))
	}
	auditsByRequest := make(map[string]domain.AuditEvent, len(audits))
	for _, audit := range audits {
		auditsByRequest[audit.RequestID] = audit
	}
	for requestID, reasonCode := range map[string]string{
		"rejected-request":           "no_available_accounts",
		"preflight-rejected-request": "route_not_supported",
	} {
		audit, ok := auditsByRequest[requestID]
		if !ok {
			t.Fatalf("missing authorization audit for request %q: %#v", requestID, audits)
		}
		if audit.ActorType != "user_api_key" || audit.ActorRef != passenger.UserRef ||
			audit.Action != "authorization_reject" || audit.TargetType != "proxy_request" ||
			audit.TargetRef != requestID || audit.Result != "rejected" || audit.ReasonCode != reasonCode {
			t.Fatalf("authorization audit for %q = %#v", requestID, audit)
		}
	}

	if first.ID == "" {
		t.Fatal("first assignment was not created")
	}
}

func TestDatabaseRejectsCrossCarScopeAndRoleMutation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	key, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "proxy", SecretDigest: []byte("digest")})
	carA, _ := store.CreateCar(ctx, domain.Car{Name: "a", Status: domain.CarStatusActive})
	carB, _ := store.CreateCar(ctx, domain.Car{Name: "b", Status: domain.CarStatusActive})
	membership, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: carA.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID,
	}})
	assignment, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: carB.ID, AuthID: "wrong-car-auth", SafeLabel: "Wrong", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	_, errRequest := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		RequestID: "cross-car", UserID: passenger.ID, APIKeyID: key.KeyID, CarID: carA.ID,
		MembershipID: membership.ID, MemberRefSnapshot: membership.MemberRef,
		DisplayNameSnapshot: membership.DisplayName, ScopeHash: "hash", ScopeSize: 1,
		SourceFormat: "openai", Outcome: domain.RequestOutcomeInProgress,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: assignment.AuthID, AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	if !errors.Is(errRequest, domain.ErrConflict) {
		t.Fatalf("BeginProxyRequest(cross car) error = %v, want ErrConflict", errRequest)
	}
	if _, errGet := store.GetProxyRequest(ctx, "cross-car"); !errors.Is(errGet, domain.ErrNotFound) {
		t.Fatalf("rolled-back GetProxyRequest() error = %v, want ErrNotFound", errGet)
	}
	_, errRole := store.db.ExecContext(ctx, "UPDATE users SET role = 'carpool_admin' WHERE id = ?", passenger.ID)
	if !errors.Is(classifyError(errRole), domain.ErrConflict) {
		t.Fatalf("direct role update error = %v, want ErrConflict", errRole)
	}
}

func TestEndMembershipAndAssignment(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 14, 30, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	car, _ := store.CreateCar(ctx, domain.Car{Name: "car", Status: domain.CarStatusActive})
	membership, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID,
	}})
	assignment, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: car.ID, AuthID: "auth", SafeLabel: "Account", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	endedAt := now.Add(time.Minute)
	if errEnd := store.EndMembership(ctx, membership.ID, "removed", endedAt, nil); errEnd != nil {
		t.Fatalf("EndMembership() error = %v", errEnd)
	}
	if _, errCurrent := store.CurrentMembership(ctx, passenger.ID); !errors.Is(errCurrent, domain.ErrNotFound) {
		t.Fatalf("CurrentMembership() error = %v, want ErrNotFound", errCurrent)
	}
	if errEnd := store.EndAuthAssignment(ctx, assignment.ID, "removed", endedAt, nil); errEnd != nil {
		t.Fatalf("EndAuthAssignment() error = %v", errEnd)
	}
	if _, errCurrent := store.CurrentAuthAssignment(ctx, assignment.AuthID); !errors.Is(errCurrent, domain.ErrNotFound) {
		t.Fatalf("CurrentAuthAssignment() error = %v, want ErrNotFound", errCurrent)
	}
	members, errMembers := store.ListCurrentMembershipsByCar(ctx, car.ID)
	assignments, errAssignments := store.ListCurrentAuthAssignmentsByCar(ctx, car.ID)
	if errMembers != nil || errAssignments != nil || len(members) != 0 || len(assignments) != 0 {
		t.Fatalf("current rows after end: members=%v assignments=%v errors=(%v,%v)", members, assignments, errMembers, errAssignments)
	}
}

func TestAuditPaginationAndUsageAggregates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 15, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	key, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "proxy", SecretDigest: []byte("digest")})
	car, _ := store.CreateCar(ctx, domain.Car{Name: "car", Status: domain.CarStatusActive})
	membership, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID,
	}})
	assignment, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: car.ID, AuthID: "auth-1", SafeLabel: "Primary", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	request, _ := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		RequestID: "report-request", UserID: passenger.ID, APIKeyID: key.KeyID, CarID: car.ID,
		MembershipID: membership.ID, MemberRefSnapshot: membership.MemberRef,
		DisplayNameSnapshot: membership.DisplayName, ScopeHash: "hash", ScopeSize: 1,
		SourceFormat: "openai", Outcome: domain.RequestOutcomeInProgress, StartedAt: now,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: assignment.AuthID, AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}})
	input, output, total := int64(10), int64(4), int64(14)
	_, _ = store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "known", RequestID: request.RequestID, AuthID: assignment.AuthID, Provider: "codex",
		UsageKnown: true, InputTokens: &input, OutputTokens: &output, TotalTokens: &total, RequestedAt: now,
	})
	_, _ = store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "unknown", RequestID: request.RequestID, AuthID: assignment.AuthID, Provider: "codex",
		UsageKnown: false, RequestedAt: now,
	})
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{
		RequestID: request.RequestID, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: now.Add(time.Second),
	}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest() error = %v", errComplete)
	}
	members, errMembers := store.MemberUsageByCar(ctx, car.ID, now.Add(-time.Minute), now.Add(time.Minute))
	if errMembers != nil || len(members) != 1 {
		t.Fatalf("MemberUsageByCar() = (%#v, %v)", members, errMembers)
	}
	if members[0].RequestCount != 1 || members[0].SucceededCount != 1 || members[0].KnownTotalTokens != 14 || members[0].UnknownUsageCount != 1 {
		t.Fatalf("member aggregate = %#v", members[0])
	}
	accounts, errAccounts := store.AccountUsageByCar(ctx, car.ID, now.Add(-time.Minute), now.Add(time.Minute))
	if errAccounts != nil || len(accounts) != 1 {
		t.Fatalf("AccountUsageByCar() = (%#v, %v)", accounts, errAccounts)
	}
	if accounts[0].RequestCount != 1 || accounts[0].KnownTotalTokens != 14 || accounts[0].UnknownUsageCount != 1 {
		t.Fatalf("account aggregate = %#v", accounts[0])
	}

	for _, event := range []domain.AuditEvent{
		{ID: "audit-a", OccurredAt: now, ActorType: "admin", ActorRef: admin.UserRef, Action: "car.create", TargetType: "car", TargetRef: car.CarRef, Result: "success"},
		{ID: "audit-b", OccurredAt: now.Add(time.Second), ActorType: "admin", ActorRef: admin.UserRef, Action: "user.create", TargetType: "user", TargetRef: passenger.UserRef, Result: "success"},
	} {
		if _, errAudit := store.InsertAuditEvent(ctx, event); errAudit != nil {
			t.Fatalf("InsertAuditEvent() error = %v", errAudit)
		}
	}
	page, errPage := store.ListAuditEvents(ctx, time.Time{}, "", 1)
	if errPage != nil || len(page) != 1 || page[0].ID != "audit-b" {
		t.Fatalf("ListAuditEvents(first) = (%#v, %v)", page, errPage)
	}
	next, errNext := store.ListAuditEvents(ctx, page[0].OccurredAt, page[0].ID, 10)
	if errNext != nil || len(next) != 1 || next[0].ID != "audit-a" {
		t.Fatalf("ListAuditEvents(next) = (%#v, %v)", next, errNext)
	}
}

func TestMemberUsageUsesIndependentRequestAndUsageTimeRanges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 4, 15, 0, 0, 0, time.UTC)
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	admin, _ := store.BootstrapAdmin(ctx, testUser("admin", domain.UserRoleAdmin), nil)
	passenger, _ := store.CreateUser(ctx, testUser("passenger", domain.UserRolePassenger))
	key, _ := store.CreateAPIKey(ctx, domain.APIKey{UserID: passenger.ID, Name: "proxy", SecretDigest: []byte("digest")})
	car, _ := store.CreateCar(ctx, domain.Car{Name: "car", Status: domain.CarStatusActive})
	membership, _ := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: passenger.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID,
	}})
	assignment, _ := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: car.ID, AuthID: "auth-1", SafeLabel: "Primary", ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	scope := []domain.ProxyRequestAuthScope{{
		AuthID: assignment.AuthID, AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef,
		SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot,
	}}
	beginRequest := func(requestID string, startedAt time.Time) {
		t.Helper()
		_, errBegin := store.BeginProxyRequest(ctx, domain.ProxyRequest{
			RequestID: requestID, UserID: passenger.ID, APIKeyID: key.KeyID, CarID: car.ID,
			MembershipID: membership.ID, MemberRefSnapshot: membership.MemberRef,
			DisplayNameSnapshot: membership.DisplayName, ScopeHash: "hash", ScopeSize: 1,
			SourceFormat: "openai", Outcome: domain.RequestOutcomeInProgress, StartedAt: startedAt,
		}, scope)
		if errBegin != nil {
			t.Fatalf("BeginProxyRequest(%q) error = %v", requestID, errBegin)
		}
		if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{
			RequestID: requestID, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: now,
		}); errComplete != nil {
			t.Fatalf("CompleteProxyRequest(%q) error = %v", requestID, errComplete)
		}
	}
	beginRequest("started-before-range", from.Add(-time.Microsecond))
	beginRequest("started-inside-range", now)

	insideTotal := int64(11)
	if _, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "usage-inside-range", RequestID: "started-before-range", AuthID: assignment.AuthID,
		Provider: "codex", UsageKnown: true, TotalTokens: &insideTotal, RequestedAt: now,
	}); errUsage != nil {
		t.Fatalf("InsertUsageEvent(inside range) error = %v", errUsage)
	}
	if _, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "unknown-inside-range", RequestID: "started-before-range", AuthID: assignment.AuthID,
		Provider: "codex", UsageKnown: false, RequestedAt: now,
	}); errUsage != nil {
		t.Fatalf("InsertUsageEvent(unknown inside range) error = %v", errUsage)
	}
	outsideTotal := int64(17)
	if _, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{
		EventID: "usage-at-exclusive-end", RequestID: "started-inside-range", AuthID: assignment.AuthID,
		Provider: "codex", UsageKnown: true, TotalTokens: &outsideTotal, RequestedAt: to,
	}); errUsage != nil {
		t.Fatalf("InsertUsageEvent(exclusive end) error = %v", errUsage)
	}

	members, errMembers := store.MemberUsageByCar(ctx, car.ID, from, to)
	if errMembers != nil || len(members) != 1 {
		t.Fatalf("MemberUsageByCar() = (%#v, %v)", members, errMembers)
	}
	member := members[0]
	if member.RequestCount != 1 || member.SucceededCount != 1 || member.KnownTotalTokens != insideTotal || member.UnknownUsageCount != 1 {
		t.Fatalf("member aggregate = %#v", member)
	}
}

func TestBusyErrorsAreClassified(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "carpool.db")
	first, errFirst := Open(ctx, Config{Path: path, BusyTimeout: time.Millisecond})
	if errFirst != nil {
		t.Fatalf("first Open() error = %v", errFirst)
	}
	defer closeTestStore(t, first)
	second, errSecond := Open(ctx, Config{Path: path, BusyTimeout: time.Millisecond})
	if errSecond != nil {
		t.Fatalf("second Open() error = %v", errSecond)
	}
	defer closeTestStore(t, second)
	tx, errBegin := first.db.BeginTx(ctx, nil)
	if errBegin != nil {
		t.Fatalf("BeginTx() error = %v", errBegin)
	}
	defer func() { _ = tx.Rollback() }()
	_, errCreate := second.CreateCar(ctx, domain.Car{Name: "blocked", Status: domain.CarStatusActive})
	if !errors.Is(errCreate, domain.ErrBusy) {
		t.Fatalf("CreateCar() error = %v, want ErrBusy", errCreate)
	}
}

func concurrentlyMoveMemberships(ctx context.Context, store *Store, moves []domain.MembershipMove) []error {
	start := make(chan struct{})
	result := make(chan error, len(moves))
	var wait sync.WaitGroup
	for _, move := range moves {
		move := move
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, errMove := store.MoveMembership(ctx, move)
			result <- errMove
		}()
	}
	close(start)
	wait.Wait()
	close(result)
	return collectErrors(result)
}

func concurrentlyMoveAssignments(ctx context.Context, store *Store, moves []domain.AuthAssignmentMove) []error {
	start := make(chan struct{})
	result := make(chan error, len(moves))
	var wait sync.WaitGroup
	for _, move := range moves {
		move := move
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, errMove := store.MoveAuthAssignment(ctx, move)
			result <- errMove
		}()
	}
	close(start)
	wait.Wait()
	close(result)
	return collectErrors(result)
}

func collectErrors(input <-chan error) []error {
	var result []error
	for errResult := range input {
		result = append(result, errResult)
	}
	return result
}

func assertOneSuccessOneConflict(t *testing.T, results []error) {
	t.Helper()
	var successes, conflicts int
	for _, errResult := range results {
		switch {
		case errResult == nil:
			successes++
		case errors.Is(errResult, domain.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result = %v", errResult)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results successes=%d conflicts=%d", successes, conflicts)
	}
}

func testUser(username string, role domain.UserRole) domain.User {
	return domain.User{
		Username: username, DefaultDisplayName: username, Role: role,
		Status: domain.UserStatusActive, PasswordHash: "$argon2id$test",
	}
}
