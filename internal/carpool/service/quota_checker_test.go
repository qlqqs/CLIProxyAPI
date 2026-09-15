package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type checkerFailFirstAuthorization struct {
	Repository
	failure error
	calls   int
}

func (r *checkerFailFirstAuthorization) AuthorizeAndBeginProxyRequest(ctx context.Context, input domain.ProxyAuthorization) (domain.AuthorizationSnapshot, error) {
	r.calls++
	if r.calls == 1 {
		return domain.AuthorizationSnapshot{}, r.failure
	}
	return r.Repository.AuthorizeAndBeginProxyRequest(ctx, input)
}

func TestCheckerFailedPrecheckCannotCreateAdmittedRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		failure := errors.New("synthetic precheck storage failure")
		repository := &checkerFailFirstAuthorization{Repository: f.store, failure: failure}
		f.control.repository = repository
		snapshot, err := f.authorize(context.Background(), 0, 0)
		if snapshot != nil || !errors.Is(err, failure) {
			t.Fatalf("authorize = %v, %v", snapshot, err)
		}
		if requests := f.requests(t); len(requests) != 0 {
			t.Fatalf("failed precheck created request facts: %+v", requests)
		}
		f.assertIdle(t)
	})
}

func TestCheckerMultiAccountCapAddedWhileQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		ctx := context.Background()
		if _, err := f.manager.Register(ctx, &coreauth.Auth{ID: "checker-second-auth", Provider: "claude", Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{CarID: f.car.ID, AuthID: "checker-second-auth", SafeLabel: "Second", ProviderSnapshot: "claude", CreatedByUserID: f.admin.ID}}); err != nil {
			t.Fatal(err)
		}
		one := 1
		f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &one})
		active := f.admit(t, 0, 0)
		queued := f.enqueue(t, ctx, 0, 1)
		f.setAccountLimit(t, &one)
		active.Release()
		got := takeQuotaServiceAdmission(t, queued)
		var admission *ProxyAdmissionError
		if got.snapshot != nil || !errors.As(got.err, &admission) || admission.Reason != "account_concurrency_requires_single_account" {
			t.Fatalf("queued admission = %v, %v", got.snapshot, got.err)
		}
		f.assertIdle(t)
	})
}
