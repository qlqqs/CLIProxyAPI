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

func quotaWindowColumn(kind string) string {
	if kind == domain.QuotaWindowFiveHour {
		return "five_hour_window_id"
	}
	return "weekly_window_id"
}

// ObserveQuotaWindows accepts trusted upstream boundaries only. Corrections keep
// the same identity: already attributed fees can never disappear on a refresh.
func (s *Store) ObserveQuotaWindows(ctx context.Context, observations []domain.QuotaWindowObservation) error {
	if err := s.ready(); err != nil {
		return err
	}
	if len(observations) == 0 {
		return nil
	}
	for _, o := range observations {
		if strings.TrimSpace(o.AuthID) == "" || (o.Kind != domain.QuotaWindowFiveHour && o.Kind != domain.QuotaWindowWeekly) || o.From.IsZero() || o.ResetAt.IsZero() || o.ObservedAt.IsZero() || !o.ResetAt.After(o.From) || o.ObservedAt.Before(o.From) {
			return fmt.Errorf("sqlite store: invalid quota observation: %w", domain.ErrInvalid)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(err)
	}
	for _, o := range observations {
		var id, reset, observed int64
		err = tx.QueryRowContext(ctx, `SELECT id,reset_at,observed_at FROM quota_windows WHERE auth_id=? AND kind=? ORDER BY id DESC LIMIT 1`, o.AuthID, o.Kind).Scan(&id, &reset, &observed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return rollback(tx, scanError("read quota observation", err))
		}
		nextFrom, nextReset, nextObserved := toDatabaseTime(o.From), toDatabaseTime(o.ResetAt), toDatabaseTime(o.ObservedAt)
		if err == nil && nextObserved <= observed {
			continue
		}
		// Sub-minute reset rounding is a correction even when an observation crosses
		// the old reset. A genuinely new window requires a later start and reset.
		rounding := int64(time.Minute / time.Microsecond)
		isNew := errors.Is(err, sql.ErrNoRows) || (nextObserved >= reset && nextFrom >= reset-rounding && nextReset > reset+rounding)
		if isNew {
			result, errInsert := tx.ExecContext(ctx, `INSERT INTO quota_windows(auth_id,kind,window_from,reset_at,observed_at) VALUES (?,?,?,?,?)`, o.AuthID, o.Kind, nextFrom, nextReset, nextObserved)
			if errInsert != nil {
				return rollback(tx, classifyError(errInsert))
			}
			id, err = result.LastInsertId()
			if err != nil {
				return rollback(tx, err)
			}
		} else {
			// Report the corrected upstream boundaries, while existing fee links
			// remain frozen even when the start or reset moves forwards/backwards.
			if _, err = tx.ExecContext(ctx, `UPDATE quota_windows SET window_from=?,reset_at=?,observed_at=? WHERE id=?`, nextFrom, nextReset, nextObserved, id); err != nil {
				return rollback(tx, classifyError(err))
			}
		}
		column := quotaWindowColumn(o.Kind)
		if _, err = tx.ExecContext(ctx, `UPDATE quota_request_fees SET `+column+`=? WHERE auth_id=? AND `+column+` IS NULL AND started_at>=? AND started_at<?`, id, o.AuthID, nextFrom, nextReset); err != nil {
			return rollback(tx, classifyError(err))
		}
		// SQLite's integer SUM fails on overflow rather than silently weakening gates.
		if err = checkQuotaWindowTotals(ctx, tx, column, id, ""); err != nil {
			return rollback(tx, err)
		}
	}
	return classifyError(tx.Commit())
}

func checkQuotaWindowTotals(ctx context.Context, tx *sql.Tx, column string, id int64, membershipID string) (err error) {
	rows, err := tx.QueryContext(ctx, `SELECT SUM(confirmed_nano_usd),SUM(unknown_cost_events) FROM quota_request_fees WHERE `+column+`=? AND (?='' OR membership_id=?) GROUP BY membership_id`, id, membershipID, membershipID)
	if err != nil {
		return fmt.Errorf("sqlite store: sum quota fees: %w", classifyError(err))
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var amount, unknown int64
		if errScan := rows.Scan(&amount, &unknown); errScan != nil {
			return fmt.Errorf("sqlite store: quota amount overflow: %w", errScan)
		}
	}
	return rows.Err()
}

func (s *Store) MemberQuotaWindows(ctx context.Context, membershipID, authID string, at time.Time) ([]domain.MemberQuotaWindow, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = s.currentTime()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classifyError(err)
	}
	result, err := memberQuotaWindowsTx(ctx, tx, membershipID, authID, at)
	if err != nil {
		return nil, rollback(tx, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, classifyError(err)
	}
	return result, nil
}

func memberQuotaWindowsTx(ctx context.Context, tx *sql.Tx, membershipID, authID string, at time.Time) ([]domain.MemberQuotaWindow, error) {
	limits, err := memberQuotaLimits(ctx, tx, membershipID)
	if err != nil {
		return nil, err
	}
	var enabled int64
	if err = tx.QueryRowContext(ctx, `SELECT MAX(s.applied_at,m.started_at) FROM schema_migrations s JOIN memberships m ON m.id=? WHERE s.version=4`, membershipID).Scan(&enabled); err != nil {
		return nil, scanError("read quota coverage", err)
	}
	result := []domain.MemberQuotaWindow{{Kind: domain.QuotaWindowFiveHour, LimitNanoUSD: limits.FiveHourNanoUSD, PendingSync: true}, {Kind: domain.QuotaWindowWeekly, LimitNanoUSD: limits.WeeklyNanoUSD, PendingSync: true}}
	for i := range result {
		w := &result[i]
		w.CoverageFrom = fromDatabaseTime(enabled)
		var id, from, reset int64
		// Select by execution time, never the eventual completion time.
		err = tx.QueryRowContext(ctx, `SELECT id,window_from,reset_at FROM quota_windows WHERE auth_id=? AND kind=? AND window_from<=? AND reset_at>? ORDER BY id DESC LIMIT 1`, authID, w.Kind, toDatabaseTime(at), toDatabaseTime(at)).Scan(&id, &from, &reset)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, scanError("read member quota window", err)
		}
		w.From, w.ResetAt, w.PendingSync = fromDatabaseTime(from), fromDatabaseTime(reset), false
		if w.From.After(w.CoverageFrom) {
			w.CoverageFrom = w.From
		}
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(confirmed_nano_usd),0),COALESCE(SUM(unknown_cost_events),0) FROM quota_request_fees WHERE membership_id=? AND `+quotaWindowColumn(w.Kind)+`=?`, membershipID, id).Scan(&w.ConfirmedNanoUSD, &w.UnknownCostEvents)
		if err != nil {
			return nil, scanError("read confirmed quota fees", err)
		}
	}
	return result, nil
}

// ensureQuotaRequestFeeTx freezes the admission timestamp and any known window
// before the first usage event. Missing windows remain assignable later.
func ensureQuotaRequestFeeTx(ctx context.Context, tx *sql.Tx, requestID, authID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO quota_request_fees(request_id,auth_id,membership_id,started_at,five_hour_window_id,weekly_window_id)
 SELECT p.request_id,?,p.membership_id,p.started_at,
 (SELECT id FROM quota_windows WHERE auth_id=? AND kind='5h' AND window_from<=p.started_at AND reset_at>p.started_at ORDER BY id DESC LIMIT 1),
 (SELECT id FROM quota_windows WHERE auth_id=? AND kind='7d' AND window_from<=p.started_at AND reset_at>p.started_at ORDER BY id DESC LIMIT 1)
 FROM proxy_requests p WHERE p.request_id=? AND p.membership_id IS NOT NULL AND p.billing_status!='not_billable'
 ON CONFLICT(request_id,auth_id) DO NOTHING`, authID, authID, authID, requestID)
	if err != nil {
		return fmt.Errorf("sqlite store: freeze quota fee attribution: %w", classifyError(err))
	}
	return nil
}

func recordQuotaFeeTx(ctx context.Context, tx *sql.Tx, event domain.UsageEvent, cost *int64, pricingStatus, pricingReason string) error {
	if err := ensureQuotaRequestFeeTx(ctx, tx, event.RequestID, event.AuthID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billed_event_receipts(event_id,request_id,cost_nano_usd,pricing_status,pricing_reason) VALUES (?,?,?,?,?)`, event.EventID, event.RequestID, nullableInt64(cost), pricingStatus, pricingReason); err != nil {
		return classifyError(err)
	}
	amount, unknown := int64(0), int64(0)
	if cost == nil {
		unknown = 1
	} else {
		amount = *cost
	}
	if _, err := tx.ExecContext(ctx, `UPDATE quota_request_fees SET confirmed_nano_usd=confirmed_nano_usd+?,unknown_cost_events=unknown_cost_events+? WHERE request_id=? AND auth_id=?`, amount, unknown, event.RequestID, event.AuthID); err != nil {
		return fmt.Errorf("sqlite store: accumulate quota fee: %w", classifyError(err))
	}
	var five, week sql.NullInt64
	var membershipID string
	err := tx.QueryRowContext(ctx, `SELECT five_hour_window_id,weekly_window_id,membership_id FROM quota_request_fees WHERE request_id=? AND auth_id=?`, event.RequestID, event.AuthID).Scan(&five, &week, &membershipID)
	if err != nil {
		return scanError("read fee attribution", err)
	}
	for _, v := range []struct {
		column string
		id     sql.NullInt64
	}{{"five_hour_window_id", five}, {"weekly_window_id", week}} {
		if v.id.Valid {
			if err = checkQuotaWindowTotals(ctx, tx, v.column, v.id.Int64, membershipID); err != nil {
				return err
			}
		}
	}
	return nil
}
