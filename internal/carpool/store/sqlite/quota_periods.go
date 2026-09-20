package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func (s *Store) MemberQuotaWindows(ctx context.Context, membershipID string, at time.Time) ([]domain.MemberQuotaWindow, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(membershipID) == "" || at.IsZero() {
		return nil, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifyError(err)
	}
	result, err := memberQuotaWindowsTx(ctx, tx, membershipID, at)
	if err != nil {
		return nil, rollback(tx, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, classifyError(err)
	}
	return result, nil
}

func memberQuotaWindowsTx(ctx context.Context, tx *sql.Tx, membershipID string, at time.Time) ([]domain.MemberQuotaWindow, error) {
	limits, err := memberQuotaLimits(ctx, tx, membershipID)
	if err != nil {
		return nil, err
	}
	result := []domain.MemberQuotaWindow{
		{Kind: domain.QuotaWindowFiveHour, LimitNanoUSD: limits.FiveHourNanoUSD},
		{Kind: domain.QuotaWindowWeekly, LimitNanoUSD: limits.WeeklyNanoUSD},
	}
	for i := range result {
		window := &result[i]
		var from, reset int64
		err = tx.QueryRowContext(ctx, `SELECT window_from,reset_at,confirmed_nano_usd,unknown_cost_events FROM member_quota_windows WHERE membership_id=? AND kind=?`, membershipID, window.Kind).Scan(&from, &reset, &window.ConfirmedNanoUSD, &window.UnknownCostEvents)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, scanError("read member quota window", err)
		}
		window.From, window.ResetAt = fromDatabaseTime(from), fromDatabaseTime(reset)
		if !at.Before(window.ResetAt) {
			window.From, window.ResetAt = time.Time{}, time.Time{}
			window.ConfirmedNanoUSD, window.UnknownCostEvents = 0, 0
		}
	}
	return result, nil
}

func recordQuotaFeeTx(ctx context.Context, tx *sql.Tx, event domain.UsageEvent, cost *int64, pricingStatus, pricingReason string, location *time.Location) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO billed_event_receipts(event_id,request_id,cost_nano_usd,pricing_status,pricing_reason) VALUES (?,?,?,?,?)`, event.EventID, event.RequestID, nullableInt64(cost), pricingStatus, pricingReason); err != nil {
		return classifyError(err)
	}
	var membershipID string
	var startedAt int64
	if err := tx.QueryRowContext(ctx, `SELECT membership_id,started_at FROM proxy_requests WHERE request_id=? AND membership_id IS NOT NULL`, event.RequestID).Scan(&membershipID, &startedAt); err != nil {
		return scanError("read quota fee request", err)
	}
	started := fromDatabaseTime(startedAt)
	updated := event.RecordedAt
	if updated.IsZero() {
		updated = started
	}
	if location == nil {
		location = time.UTC
	}
	for _, kind := range []string{domain.QuotaWindowFiveHour, domain.QuotaWindowWeekly} {
		from, reset := localQuotaWindowBounds(started, kind, location)
		var currentFrom, currentReset int64
		err := tx.QueryRowContext(ctx, `SELECT window_from,reset_at FROM member_quota_windows WHERE membership_id=? AND kind=?`, membershipID, kind).Scan(&currentFrom, &currentReset)
		if errors.Is(err, sql.ErrNoRows) {
			if cost == nil {
				continue
			}
			if err = insertMemberQuotaWindow(ctx, tx, membershipID, kind, from, reset, cost, updated); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return scanError("read quota window for fee", err)
		}
		currentFromTime, currentResetTime := fromDatabaseTime(currentFrom), fromDatabaseTime(currentReset)
		if cost == nil {
			if (started.Before(currentResetTime) && !started.Before(currentFromTime)) || (started.Before(currentFromTime) && reset.After(currentFromTime)) {
				if _, err = tx.ExecContext(ctx, `UPDATE member_quota_windows SET unknown_cost_events=unknown_cost_events+1,revision=revision+1,updated_at=? WHERE membership_id=? AND kind=?`, toDatabaseTime(updated), membershipID, kind); err != nil {
					return fmt.Errorf("sqlite store: accumulate unknown member quota fee: %w", classifyError(err))
				}
			}
			continue
		}
		switch {
		case !started.Before(currentResetTime) && !started.Before(currentFromTime):
			if err = replaceMemberQuotaWindow(ctx, tx, membershipID, kind, from, reset, cost, updated); err != nil {
				return err
			}
		case !started.Before(currentFromTime):
			if err = accumulateMemberQuotaWindow(ctx, tx, membershipID, kind, cost, updated); err != nil {
				return err
			}
		case reset.After(currentFromTime):
			if err = prependMemberQuotaWindow(ctx, tx, membershipID, kind, from, reset, cost, updated); err != nil {
				return err
			}
		}
	}
	return nil
}

func localQuotaWindowBounds(started time.Time, kind string, location *time.Location) (time.Time, time.Time) {
	if kind == domain.QuotaWindowWeekly {
		local := started.In(location)
		from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		return from, from.AddDate(0, 0, 7)
	}
	return started, started.Add(5 * time.Hour)
}

func quotaFeeValues(cost *int64) (int64, int64) {
	if cost == nil {
		return 0, 1
	}
	return *cost, 0
}

func insertMemberQuotaWindow(ctx context.Context, tx *sql.Tx, membershipID, kind string, from, reset time.Time, cost *int64, updated time.Time) error {
	amount, unknown := quotaFeeValues(cost)
	_, err := tx.ExecContext(ctx, `INSERT INTO member_quota_windows(membership_id,kind,window_from,reset_at,confirmed_nano_usd,unknown_cost_events,updated_at) VALUES(?,?,?,?,?,?,?)`, membershipID, kind, toDatabaseTime(from), toDatabaseTime(reset), amount, unknown, toDatabaseTime(updated))
	return classifyError(err)
}

func replaceMemberQuotaWindow(ctx context.Context, tx *sql.Tx, membershipID, kind string, from, reset time.Time, cost *int64, updated time.Time) error {
	amount, unknown := quotaFeeValues(cost)
	_, err := tx.ExecContext(ctx, `UPDATE member_quota_windows SET window_from=?,reset_at=?,confirmed_nano_usd=?,unknown_cost_events=?,revision=revision+1,updated_at=? WHERE membership_id=? AND kind=?`, toDatabaseTime(from), toDatabaseTime(reset), amount, unknown, toDatabaseTime(updated), membershipID, kind)
	return classifyError(err)
}

func accumulateMemberQuotaWindow(ctx context.Context, tx *sql.Tx, membershipID, kind string, cost *int64, updated time.Time) error {
	amount, unknown := quotaFeeValues(cost)
	_, err := tx.ExecContext(ctx, `UPDATE member_quota_windows SET confirmed_nano_usd=confirmed_nano_usd+?,unknown_cost_events=unknown_cost_events+?,revision=revision+1,updated_at=? WHERE membership_id=? AND kind=?`, amount, unknown, toDatabaseTime(updated), membershipID, kind)
	if err != nil {
		return fmt.Errorf("sqlite store: accumulate member quota: %w", classifyError(err))
	}
	return nil
}

func prependMemberQuotaWindow(ctx context.Context, tx *sql.Tx, membershipID, kind string, from, reset time.Time, cost *int64, updated time.Time) error {
	amount, unknown := quotaFeeValues(cost)
	_, err := tx.ExecContext(ctx, `UPDATE member_quota_windows SET window_from=?,reset_at=?,confirmed_nano_usd=confirmed_nano_usd+?,unknown_cost_events=unknown_cost_events+?,revision=revision+1,updated_at=? WHERE membership_id=? AND kind=?`, toDatabaseTime(from), toDatabaseTime(reset), amount, unknown, toDatabaseTime(updated), membershipID, kind)
	if err != nil {
		return fmt.Errorf("sqlite store: prepend member quota: %w", classifyError(err))
	}
	return nil
}
