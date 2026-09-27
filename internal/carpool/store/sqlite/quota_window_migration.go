package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// backfillUnboundQuotaFees groups requests by their start times, never by a
// membership-wide MIN/SUM that can expire recent fees along with old ones.
func backfillUnboundQuotaFees(ctx context.Context, tx *sql.Tx, kind, column string, at time.Time, location *time.Location) error {
	// column is one of the two fixed migration-owned column names.
	rows, errQuery := tx.QueryContext(ctx, `SELECT DISTINCT membership_id FROM quota_request_fees WHERE `+column+` IS NULL ORDER BY membership_id`)
	if errQuery != nil {
		return classifyError(errQuery)
	}
	var memberships []string
	for rows.Next() {
		var id string
		if errScan := rows.Scan(&id); errScan != nil {
			_ = rows.Close()
			return classifyError(errScan)
		}
		memberships = append(memberships, id)
	}
	if errRows := rows.Err(); errRows != nil {
		_ = rows.Close()
		return classifyError(errRows)
	}
	if errClose := rows.Close(); errClose != nil {
		return classifyError(errClose)
	}
	for _, membershipID := range memberships {
		rowsFees, errFees := tx.QueryContext(ctx, `SELECT f.started_at,f.confirmed_nano_usd,f.unknown_cost_events,
			(f.confirmed_nano_usd>0 OR EXISTS(SELECT 1 FROM billed_event_receipts r WHERE r.request_id=f.request_id AND r.cost_nano_usd IS NOT NULL))
			FROM quota_request_fees f WHERE f.membership_id=? AND f.`+column+` IS NULL ORDER BY f.started_at`, membershipID)
		if errFees != nil {
			return classifyError(errFees)
		}
		fees, errRead := readQuotaWindowFees(rowsFees)
		if errRead != nil {
			return errRead
		}
		unbound, errProject := projectQuotaWindow(fees, kind, location)
		if errProject != nil {
			return errProject
		}
		var activeUnbound *quotaWindowProjection
		if !unbound.from.IsZero() && unbound.reset.After(at) {
			activeUnbound = &unbound
		}
		var window quotaWindowProjection
		var from, reset int64
		errLinked := tx.QueryRowContext(ctx, `SELECT window_from,reset_at,confirmed_nano_usd,unknown_cost_events FROM member_quota_windows WHERE membership_id=? AND kind=?`, membershipID, kind).Scan(&from, &reset, &window.amount, &window.unknown)
		switch {
		case errLinked == nil:
			// Keep linked native bounds, including their combined reset time.
			// Add each unbound fee once if either the linked interval or its
			// independently active local interval covers the request start.
			window.from, window.reset = fromDatabaseTime(from), fromDatabaseTime(reset)
			window, errProject = sumQuotaWindowFees(fees, window, activeUnbound)
		case errors.Is(errLinked, sql.ErrNoRows):
			window = unbound
		default:
			return classifyError(errLinked)
		}
		if errProject != nil {
			return errProject
		}
		if window.from.IsZero() || !window.reset.After(at) {
			continue
		}
		_, errUpsert := tx.ExecContext(ctx, `INSERT INTO member_quota_windows(membership_id,kind,window_from,reset_at,confirmed_nano_usd,unknown_cost_events,updated_at) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(membership_id,kind) DO UPDATE SET window_from=excluded.window_from,reset_at=excluded.reset_at,confirmed_nano_usd=excluded.confirmed_nano_usd,unknown_cost_events=excluded.unknown_cost_events,revision=member_quota_windows.revision+1,updated_at=excluded.updated_at`, membershipID, kind, toDatabaseTime(window.from), toDatabaseTime(window.reset), window.amount, window.unknown, toDatabaseTime(at))
		if errUpsert != nil {
			return classifyError(errUpsert)
		}
	}
	return nil
}

// backfillQuotaWindowFees retains the published v6 aggregate, including explicit
// resets and fees whose request details have already been removed. Recoverable
// request timestamps refine that aggregate; an untraceable remainder stays at
// its existing anchor rather than inventing timestamps or restoring reset fees.
func backfillQuotaWindowFees(ctx context.Context, tx *sql.Tx) error {
	type legacyWindow struct {
		membershipID, kind           string
		from, reset, amount, unknown int64
	}
	rows, errQuery := tx.QueryContext(ctx, `SELECT membership_id,kind,window_from,reset_at,confirmed_nano_usd,unknown_cost_events FROM member_quota_windows`)
	if errQuery != nil {
		return classifyError(errQuery)
	}
	var windows []legacyWindow
	for rows.Next() {
		var window legacyWindow
		if errScan := rows.Scan(&window.membershipID, &window.kind, &window.from, &window.reset, &window.amount, &window.unknown); errScan != nil {
			_ = rows.Close()
			return classifyError(errScan)
		}
		windows = append(windows, window)
	}
	if errRows := rows.Err(); errRows != nil {
		_ = rows.Close()
		return classifyError(errRows)
	}
	if errClose := rows.Close(); errClose != nil {
		return classifyError(errClose)
	}
	for _, window := range windows {
		rowsFees, errFees := tx.QueryContext(ctx, `SELECT p.started_at,COALESCE(r.cost_nano_usd,0),r.cost_nano_usd IS NULL,r.cost_nano_usd IS NOT NULL
			FROM billed_event_receipts r JOIN proxy_requests p ON p.request_id=r.request_id
			WHERE p.membership_id=? AND p.started_at>=? AND p.started_at<?
			AND NOT EXISTS(SELECT 1 FROM quota_request_fees f WHERE f.request_id=p.request_id)
			UNION ALL SELECT f.started_at,f.confirmed_nano_usd,f.unknown_cost_events,
			(f.confirmed_nano_usd>0 OR EXISTS(SELECT 1 FROM billed_event_receipts r WHERE r.request_id=f.request_id AND r.cost_nano_usd IS NOT NULL))
			FROM quota_request_fees f WHERE f.membership_id=? AND f.started_at>=? AND f.started_at<?`, window.membershipID, window.from, window.reset, window.membershipID, window.from, window.reset)
		if errFees != nil {
			return classifyError(errFees)
		}
		fees, errRead := readQuotaWindowFees(rowsFees)
		if errRead != nil {
			return errRead
		}
		amount, unknown := window.amount, window.unknown
		for _, fee := range fees {
			if fee.amount > amount || fee.unknown > unknown {
				// A reset may have excluded some otherwise matching old requests.
				fees = nil
				amount, unknown = window.amount, window.unknown
				break
			}
			amount -= fee.amount
			unknown -= fee.unknown
		}
		// Even a zero remainder preserves a reliable window's original anchor.
		fees = append(fees, quotaWindowFee{started: fromDatabaseTime(window.from), amount: amount, unknown: unknown, reliable: true})
		for _, fee := range fees {
			if errInsert := insertQuotaWindowAllocation(ctx, tx, window.membershipID, window.kind, fee); errInsert != nil {
				return errInsert
			}
		}
	}
	return nil
}
