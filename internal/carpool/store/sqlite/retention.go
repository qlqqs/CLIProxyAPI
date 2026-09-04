package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

// RetentionCleanup configures one bounded cleanup transaction.
type RetentionCleanup struct {
	UsageCutoff time.Time
	AuditCutoff time.Time
	BatchSize   int
}

// RetentionCleanupResult reports rows removed by one cleanup transaction.
type RetentionCleanupResult struct {
	UsageEventsDeleted   int64
	ProxyRequestsDeleted int64
	AuditEventsDeleted   int64
}

// CleanupRetention removes one batch of expired usage, request, and audit facts.
func (s *Store) CleanupRetention(ctx context.Context, cleanup RetentionCleanup) (RetentionCleanupResult, error) {
	if errReady := s.ready(); errReady != nil {
		return RetentionCleanupResult{}, errReady
	}
	if cleanup.UsageCutoff.IsZero() || cleanup.AuditCutoff.IsZero() || cleanup.BatchSize < 1 {
		return RetentionCleanupResult{}, fmt.Errorf("sqlite store: invalid retention cleanup: %w", domain.ErrInvalid)
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return RetentionCleanupResult{}, fmt.Errorf("sqlite store: begin retention cleanup: %w", classifyError(errBegin))
	}

	result := RetentionCleanupResult{}
	usageDeleted, errUsage := deleteRetentionBatch(ctx, tx, "expired usage events", `
		DELETE FROM usage_events
		WHERE event_id IN (
			SELECT event_id
			FROM usage_events
			WHERE requested_at < ?
			ORDER BY requested_at, event_id
			LIMIT ?
		)
	`, toDatabaseTime(cleanup.UsageCutoff), cleanup.BatchSize)
	if errUsage != nil {
		return RetentionCleanupResult{}, rollback(tx, errUsage)
	}
	result.UsageEventsDeleted = usageDeleted

	requestsDeleted, errRequests := deleteRetentionBatch(ctx, tx, "expired proxy requests", `
		DELETE FROM proxy_requests
		WHERE request_id IN (
			SELECT p.request_id
			FROM proxy_requests AS p
			WHERE p.outcome <> 'in_progress'
			  AND p.completed_at < ?
			  AND NOT EXISTS (
				SELECT 1
				FROM usage_events AS e
				WHERE e.request_id = p.request_id
			  )
			ORDER BY p.completed_at, p.request_id
			LIMIT ?
		)
	`, toDatabaseTime(cleanup.UsageCutoff), cleanup.BatchSize)
	if errRequests != nil {
		return RetentionCleanupResult{}, rollback(tx, errRequests)
	}
	result.ProxyRequestsDeleted = requestsDeleted

	auditsDeleted, errAudits := deleteRetentionBatch(ctx, tx, "expired audit events", `
		DELETE FROM audit_events
		WHERE id IN (
			SELECT id
			FROM audit_events
			WHERE occurred_at < ?
			ORDER BY occurred_at, id
			LIMIT ?
		)
	`, toDatabaseTime(cleanup.AuditCutoff), cleanup.BatchSize)
	if errAudits != nil {
		return RetentionCleanupResult{}, rollback(tx, errAudits)
	}
	result.AuditEventsDeleted = auditsDeleted

	if errCommit := tx.Commit(); errCommit != nil {
		return RetentionCleanupResult{}, fmt.Errorf("sqlite store: commit retention cleanup: %w", classifyError(errCommit))
	}
	return result, nil
}

func deleteRetentionBatch(ctx context.Context, tx *sql.Tx, operation, query string, args ...any) (int64, error) {
	result, errExec := tx.ExecContext(ctx, query, args...)
	if errExec != nil {
		return 0, fmt.Errorf("sqlite store: delete %s: %w", operation, classifyError(errExec))
	}
	return affectedRows("delete "+operation, result)
}
