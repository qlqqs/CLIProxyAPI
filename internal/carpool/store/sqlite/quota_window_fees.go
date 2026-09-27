package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// quotaWindowFee is an allocation, not a second bill. Its owning window and
// permanent receipt, rather than retained request details, control its lifetime.
// Allocations survive rollover so a late fee can split earlier segments without
// losing a newer segment. Only an explicit quota reset starts a new epoch.
type quotaWindowFee struct {
	started         time.Time
	amount, unknown int64
	reliable        bool
}

type quotaWindowProjection struct {
	from, reset     time.Time
	amount, unknown int64
}

func projectQuotaWindow(fees []quotaWindowFee, kind string, location *time.Location) (quotaWindowProjection, error) {
	sort.SliceStable(fees, func(i, j int) bool {
		if fees[i].started.Equal(fees[j].started) {
			return fees[i].reliable && !fees[j].reliable
		}
		return fees[i].started.Before(fees[j].started)
	})
	var result quotaWindowProjection
	for _, fee := range fees {
		if fee.reliable && (result.from.IsZero() || !fee.started.Before(result.reset)) {
			result.from, result.reset = localQuotaWindowBounds(fee.started, kind, location)
		}
	}
	// A weekly window starts at midnight, before its first reliable request.
	// Select its bounds first so earlier unknown allocations are not discarded.
	return sumQuotaWindowFees(fees, result, nil)
}

func sumQuotaWindowFees(fees []quotaWindowFee, result quotaWindowProjection, additional *quotaWindowProjection) (quotaWindowProjection, error) {
	for _, fee := range fees {
		inWindow := !result.from.IsZero() && !fee.started.Before(result.from) && fee.started.Before(result.reset)
		inAdditional := additional != nil && !additional.from.IsZero() && !fee.started.Before(additional.from) && fee.started.Before(additional.reset)
		// Migration may combine linked and active unbound intervals. Their
		// intersection must not count any request fee twice.
		if !inWindow && !inAdditional {
			continue
		}
		if fee.amount > math.MaxInt64-result.amount || fee.unknown > math.MaxInt64-result.unknown {
			return quotaWindowProjection{}, fmt.Errorf("sqlite store: quota window total overflow")
		}
		result.amount += fee.amount
		result.unknown += fee.unknown
	}
	return result, nil
}

func insertQuotaWindowFee(ctx context.Context, tx *sql.Tx, membershipID, kind string, started time.Time, cost *int64) error {
	amount, unknown := quotaFeeValues(cost)
	return insertQuotaWindowAllocation(ctx, tx, membershipID, kind, quotaWindowFee{started: started, amount: amount, unknown: unknown, reliable: cost != nil})
}

func insertQuotaWindowAllocation(ctx context.Context, tx *sql.Tx, membershipID, kind string, fee quotaWindowFee) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO member_quota_window_fees(membership_id,kind,started_at,confirmed_nano_usd,unknown_cost_events,reliable) VALUES(?,?,?,?,?,?)`, membershipID, kind, toDatabaseTime(fee.started), fee.amount, fee.unknown, boolToInt(fee.reliable))
	return classifyError(err)
}

func rebuildQuotaWindow(ctx context.Context, tx *sql.Tx, membershipID, kind string, updated time.Time, location *time.Location) error {
	rows, errQuery := tx.QueryContext(ctx, `SELECT started_at,confirmed_nano_usd,unknown_cost_events,reliable FROM member_quota_window_fees WHERE membership_id=? AND kind=? ORDER BY started_at,reliable DESC,id`, membershipID, kind)
	if errQuery != nil {
		return classifyError(errQuery)
	}
	fees, errRead := readQuotaWindowFees(rows)
	if errRead != nil {
		return errRead
	}
	window, errProject := projectQuotaWindow(fees, kind, location)
	if errProject != nil {
		return errProject
	}
	if window.from.IsZero() {
		return fmt.Errorf("sqlite store: reliable quota window has no reliable allocations")
	}
	if _, errUpdate := tx.ExecContext(ctx, `UPDATE member_quota_windows SET window_from=?,reset_at=?,confirmed_nano_usd=?,unknown_cost_events=?,revision=revision+1,updated_at=? WHERE membership_id=? AND kind=?`, toDatabaseTime(window.from), toDatabaseTime(window.reset), window.amount, window.unknown, toDatabaseTime(updated), membershipID, kind); errUpdate != nil {
		return classifyError(errUpdate)
	}
	return nil
}

func readQuotaWindowFees(rows *sql.Rows) ([]quotaWindowFee, error) {
	var fees []quotaWindowFee
	for rows.Next() {
		var started int64
		var fee quotaWindowFee
		if errScan := rows.Scan(&started, &fee.amount, &fee.unknown, &fee.reliable); errScan != nil {
			_ = rows.Close()
			return nil, classifyError(errScan)
		}
		fee.started = fromDatabaseTime(started)
		fees = append(fees, fee)
	}
	if errRows := rows.Err(); errRows != nil {
		_ = rows.Close()
		return nil, classifyError(errRows)
	}
	if errClose := rows.Close(); errClose != nil {
		return nil, classifyError(errClose)
	}
	return fees, nil
}
