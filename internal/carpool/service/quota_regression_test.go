package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestMultiAccountStillEnforcesMemberQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		ctx := context.Background()
		limit := int64(100)
		f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &limit, WeeklySet: true, WeeklyNanoUSD: &limit})
		active := f.admit(t, 0, 0)
		_, err := f.store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "review-exhausted", RequestID: active.RequestID(), AuthID: f.assignment.AuthID, Model: "quota-model", UsageKnown: true}, "", &limit, "priced", "")
		if err != nil {
			t.Fatal(err)
		}
		active.Release()
		single, err := f.authorize(ctx, 0, 0)
		var rejected *ProxyAdmissionError
		if single != nil || !errors.As(err, &rejected) || rejected.Reason != "five_hour_quota_exhausted" {
			if single != nil {
				single.Release()
			}
			t.Fatalf("single-account control not exhausted: snapshot=%v err=%v", single, err)
		}
		if _, err := f.manager.Register(ctx, &coreauth.Auth{ID: "review-second", Provider: "claude", Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.control.MoveAccount(ctx, f.admin, f.car.CarRef, "review-second", "Second"); err != nil {
			t.Fatal(err)
		}
		multi, err := f.authorize(ctx, 0, 0)
		if multi != nil {
			multi.Release()
		}
		if err == nil {
			t.Fatal("exhausted 5h and 7d quotas were bypassed after assigning a second account")
		}
		if !errors.As(err, &rejected) || rejected.Reason != "five_hour_quota_exhausted" {
			t.Fatalf("unexpected rejection: %v", err)
		}
	})
}

func TestMultiAccountQuotaRecheckedAfterUserQueue(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newQuotaServiceFixture(t)
				ctx := context.Background()
				if _, errRegister := f.manager.Register(ctx, &coreauth.Auth{ID: "queued-second", Provider: "claude", Status: coreauth.StatusActive}); errRegister != nil {
					t.Fatal(errRegister)
				}
				if _, errMove := f.control.MoveAccount(ctx, f.admin, f.car.CarRef, "queued-second", "Second"); errMove != nil {
					t.Fatal(errMove)
				}
				one, limit := 1, int64(100)
				limits := domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &one}
				if window == "5h" {
					limits.FiveHourSet, limits.FiveHourNanoUSD = true, &limit
				} else {
					limits.WeeklySet, limits.WeeklyNanoUSD = true, &limit
				}
				f.setMemberLimits(t, 0, limits)
				active := f.admit(t, 0, 0)
				queued := f.enqueue(t, ctx, 0, 1)
				if _, errBill := f.store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "multi-queued-exhausted", RequestID: active.RequestID(), AuthID: f.assignment.AuthID, Model: "quota-model", UsageKnown: true}, "", &limit, "priced", ""); errBill != nil {
					t.Fatal(errBill)
				}
				active.Release()
				got := takeQuotaServiceAdmission(t, queued)
				var rejected *ProxyAdmissionError
				want := "five_hour_quota_exhausted"
				if window == "7d" {
					want = "weekly_quota_exhausted"
				}
				if got.snapshot != nil {
					got.snapshot.Release()
				}
				if got.snapshot != nil || !errors.As(got.err, &rejected) || rejected.Reason != want {
					t.Fatalf("queued multi-account admission: %+v; want %s", got, want)
				}
				f.assertIdle(t)
			})
		})
	}
}
