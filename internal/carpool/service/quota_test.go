package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
)

func TestQuotaServiceDefaultQueueBoundAndCheckOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		one := 1
		f.setAccountLimit(t, &one)
		active := f.admit(t, 0, 0)
		var queued []<-chan quotaServiceAdmission
		var cancels []context.CancelFunc
		for i := range 10 {
			ctx, cancel := context.WithCancel(context.Background())
			cancels = append(cancels, cancel)
			queued = append(queued, f.enqueue(t, ctx, i%2, (i/2)%2))
		}
		if got := f.control.concurrency.Snapshot(f.users[0].ID, f.assignment.AuthID); got.QueueCapacity != 10 || got.QueueLength != 10 || got.AccountActive != 1 {
			t.Fatalf("default queue snapshot = %+v", got)
		}
		// A single database connection also proves the waiters retain no transaction.
		if rows := f.requests(t); len(rows) != 1 || rows[0].Request.RequestID != active.RequestID() {
			t.Fatalf("CheckOnly persisted queued requests: %+v", rows)
		}
		if snapshot, errAuthorize := f.authorize(context.Background(), 1, 1); snapshot != nil || !errors.Is(errAuthorize, carpoolruntime.ErrConcurrencyQueueFull) {
			t.Fatalf("eleventh waiting request = %v, %v", snapshot, errAuthorize)
		}
		for _, cancel := range cancels {
			cancel()
		}
		for _, result := range queued {
			got := takeQuotaServiceAdmission(t, result)
			if got.snapshot != nil || !errors.Is(got.err, context.Canceled) {
				t.Fatalf("canceled service admission = %+v", got)
			}
		}
		active.Release()
		f.assertIdle(t)
		rows := f.requests(t)
		if len(rows) != 12 {
			t.Fatalf("logical request records = %d, want active + full + 10 canceled", len(rows))
		}
		reasons := map[string]int{}
		for _, row := range rows {
			if row.Request.RequestID != active.RequestID() && (row.Request.Outcome != domain.RequestOutcomeRejected || row.Request.CompletedAt == nil || row.Request.UpstreamAttempted || row.Request.ScopeSize != 0) {
				t.Fatalf("waiter left an executing record: %+v", row.Request)
			}
			reasons[row.Request.ReasonCode]++
		}
		if reasons["request_canceled"] != 10 || reasons["concurrency_queue_full"] != 1 {
			t.Fatalf("rejection records = %+v", reasons)
		}
	})
}

func TestQuotaServiceSharedUserKeysAndAccountUsers(t *testing.T) {
	for _, dimension := range []string{"user", "account"} {
		t.Run(dimension, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newQuotaServiceFixture(t)
				one := 1
				waitUser := 0
				if dimension == "user" {
					f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &one})
				} else {
					f.setAccountLimit(t, &one)
					waitUser = 1
				}
				active := f.admit(t, 0, 0)
				queued := f.enqueue(t, context.Background(), waitUser, 1)
				if dimension == "user" {
					// Another user can use the same account while the first user's other Key waits.
					other := f.admit(t, 1, 0)
					other.Release()
				}
				active.Release()
				got := takeQuotaServiceAdmission(t, queued)
				if got.err != nil || got.snapshot == nil || got.snapshot.UserID() != f.users[waitUser].ID || got.snapshot.APIKeyID() != f.keys[waitUser][1].KeyID {
					t.Fatalf("shared identity admission = %+v", got)
				}
				got.snapshot.Release()
				f.assertIdle(t)
			})
		})
	}
}

func TestQuotaServiceFreezesAdmissionTimeAndPricesAfterWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		f.clock.Store(time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC).UnixMicro())
		catalogA := f.setPrices(t, "0.001")
		one := 1
		f.setAccountLimit(t, &one)
		active := f.admit(t, 0, 0)
		queued := f.enqueue(t, context.Background(), 0, 1)
		if len(f.requests(t)) != 1 {
			t.Fatal("queued request must not freeze a request/price row")
		}
		f.clock.Add(int64(2 * time.Minute / time.Microsecond))
		admittedAt := f.now()
		catalogB := f.setPrices(t, "0.009")
		active.Release()
		got := takeQuotaServiceAdmission(t, queued)
		if got.err != nil || got.snapshot == nil {
			t.Fatalf("post-wait admission = %+v", got)
		}
		defer got.snapshot.Release()
		if active.Prices().Hash() != catalogA.Hash || got.snapshot.Prices().Hash() != catalogB.Hash {
			t.Fatal("prices were frozen at enqueue time or an active snapshot changed")
		}
		first, errFirst := f.store.GetUsageRequestDetail(context.Background(), active.RequestID())
		second, errSecond := f.store.GetUsageRequestDetail(context.Background(), got.snapshot.RequestID())
		if errFirst != nil || errSecond != nil {
			t.Fatalf("request detail errors = %v, %v", errFirst, errSecond)
		}
		if !second.Request.StartedAt.Equal(admittedAt) || second.Request.PricingCatalogHash != catalogB.Hash || second.Request.PricingCoverageFrom == nil || !second.Request.PricingCoverageFrom.Equal(admittedAt) {
			t.Fatalf("admission did not freeze post-queue time/prices: first=%+v second=%+v", first.Request, second.Request)
		}
		got.snapshot.Release()
		f.assertIdle(t)
	})
}

func TestQuotaServiceRechecksAuthorizationAfterWaiting(t *testing.T) {
	for _, scenario := range []string{"five-hour limit", "revoked key", "disabled user", "moved account", "writer failure"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newQuotaServiceFixture(t)
				one := 1
				f.setAccountLimit(t, &one)
				active := f.admit(t, 0, 0)
				queued := f.enqueue(t, context.Background(), 0, 1)
				ctx := context.Background()
				wantReason := ""
				switch scenario {
				case "five-hour limit":
					limit := int64(1)
					f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &limit})
					if _, errBill := f.store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "queued-quota-event", RequestID: active.RequestID(), AuthID: f.assignment.AuthID, Model: "quota-model", UsageKnown: true}, "", &limit, "", ""); errBill != nil {
						t.Fatal(errBill)
					}
					wantReason = "five_hour_quota_exhausted"
				case "revoked key":
					if errRevoke := f.control.RevokeAPIKey(ctx, f.admin, f.users[0], f.keys[0][1].KeyID); errRevoke != nil {
						t.Fatal(errRevoke)
					}
					wantReason = "api_key_revoked"
				case "disabled user":
					if _, errDisable := f.control.SetUserStatus(ctx, f.admin, f.users[0].UserRef, domain.UserStatusDisabled); errDisable != nil {
						t.Fatal(errDisable)
					}
					wantReason = "user_disabled"
				case "moved account":
					other, errCar := f.store.CreateCar(ctx, domain.Car{CarRef: quotaServiceRef(t, "car"), Name: "Other car", Status: domain.CarStatusActive})
					if errCar != nil {
						t.Fatal(errCar)
					}
					if _, errMove := f.control.MoveAccount(ctx, f.admin, other.CarRef, f.assignment.AuthID, "Moved"); errMove != nil {
						t.Fatal(errMove)
					}
					wantReason = "no_available_accounts"
				case "writer failure":
					f.unhealthy.Store(true)
					wantReason = "accounting_unavailable"
				}
				if got := f.control.concurrency.Snapshot("", ""); got.QueueLength != 1 || got.TotalActive != 1 || len(f.requests(t)) != 1 {
					t.Fatalf("policy mutation prematurely granted/recorded waiter: %+v", got)
				}
				active.Release()
				got := takeQuotaServiceAdmission(t, queued)
				if got.snapshot != nil || got.err == nil {
					t.Fatalf("stale authorization admitted: %+v", got)
				}
				if scenario == "writer failure" {
					if !errors.Is(got.err, domain.ErrAccountingUnavailable) {
						t.Fatalf("writer gate error = %v", got.err)
					}
				} else {
					var rejected *ProxyAdmissionError
					if !errors.As(got.err, &rejected) || rejected.Reason != wantReason || !errors.Is(got.err, domain.ErrAuthorizationRejected) {
						t.Fatalf("rejection = %v, want %s", got.err, wantReason)
					}
				}
				f.assertIdle(t)
				rows := f.requests(t)
				if len(rows) != 2 {
					t.Fatalf("request records = %d, want original plus one rejection", len(rows))
				}
				for _, row := range rows {
					if row.Request.RequestID == active.RequestID() {
						continue
					}
					if row.Request.Outcome != domain.RequestOutcomeRejected || row.Request.ReasonCode != wantReason || row.Request.CompletedAt == nil || row.Request.UpstreamAttempted || row.Request.ScopeSize != 0 {
						t.Fatalf("invalid final rejection record: %+v", row.Request)
					}
				}
			})
		})
	}
}

func TestQuotaServiceFirstObservationIncludesPendingCharges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		limit := int64(10_000_000)
		f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &limit})
		active := f.admit(t, 0, 0)
		if _, errBill := f.store.RecordUsageEventBilled(context.Background(), domain.UsageEvent{EventID: "first-signal-event", RequestID: active.RequestID(), AuthID: f.assignment.AuthID, Model: "quota-model", UsageKnown: true}, "", &limit, "", ""); errBill != nil {
			t.Fatal(errBill)
		}
		active.Release()
		_, errAuthorize := f.authorize(context.Background(), 0, 1)
		var rejected *ProxyAdmissionError
		if !errors.As(errAuthorize, &rejected) || rejected.Reason != "five_hour_quota_exhausted" {
			t.Fatalf("first observation dropped pending charges: %v", errAuthorize)
		}
		_, windows, errView := f.control.PassengerQuota(context.Background(), f.users[0])
		if errView != nil {
			t.Fatal(errView)
		}
		for _, window := range windows {
			if window.Kind == "5h" && window.ConfirmedNanoUSD != limit {
				t.Fatalf("pending charge projection = %+v", window)
			}
		}
		f.assertIdle(t)
	})
}

func TestQuotaServicePolicyUpdatesWakeWaitingRequests(t *testing.T) {
	for _, dimension := range []string{"user", "account"} {
		for _, mode := range []string{"raise", "unlimited"} {
			t.Run(dimension+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newQuotaServiceFixture(t)
					set := func(limit *int) {
						f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: limit})
					}
					if dimension == "account" {
						set = func(limit *int) { f.setAccountLimit(t, limit) }
					}
					one, two := 1, 2
					set(&one)
					active := f.admit(t, 0, 0)
					queued := f.enqueue(t, context.Background(), 0, 1)
					if mode == "raise" {
						set(&two)
					} else {
						set(nil)
					}
					got := takeQuotaServiceAdmission(t, queued)
					if got.err != nil || got.snapshot == nil {
						t.Fatalf("policy update did not admit waiter: %+v", got)
					}
					if counts := f.control.concurrency.Snapshot(f.users[0].ID, f.assignment.AuthID); counts.TotalActive != 2 || counts.QueueLength != 0 {
						t.Fatalf("wake counts = %+v", counts)
					}
					active.Release()
					got.snapshot.Release()
					f.assertIdle(t)
				})
			})
		}
	}
}

func TestQuotaServiceModelQueriesDoNotConsumeGenerationPermits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newQuotaServiceFixture(t)
		one := 1
		f.setAccountLimit(t, &one)
		active := f.admit(t, 0, 0)
		zero := int64(0)
		f.setMemberLimits(t, 0, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &zero})
		f.unhealthy.Store(true)
		models, errModels := f.control.AuthorizeProxy(context.Background(), f.users[0].ID, f.keys[0][1].KeyID, "synthetic-scope", http.MethodGet, "/v1/models", false)
		if errModels != nil || models == nil {
			t.Fatalf("model query used generation gates: %v", errModels)
		}
		defer models.Release()
		if got := f.control.concurrency.Snapshot("", ""); got.TotalActive != 1 || got.QueueLength != 0 {
			t.Fatalf("models consumed a generation permit: %+v", got)
		}
		detail, errDetail := f.store.GetUsageRequestDetail(context.Background(), models.RequestID())
		if errDetail != nil || detail.Request.BillingStatus != "not_billable" {
			t.Fatalf("models billing = %+v, %v", detail.Request, errDetail)
		}
		models.Release()
		active.Release()
		f.assertIdle(t)
	})
}
