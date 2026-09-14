package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func (s *Store) GetRetentionSettings(ctx context.Context, defaultDays int64) (domain.RetentionSettings, error) {
	if err := s.ready(); err != nil {
		return domain.RetentionSettings{}, err
	}
	var override sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT usage_retention_days FROM carpool_billing_settings WHERE id=1`).Scan(&override); err != nil {
		return domain.RetentionSettings{}, fmt.Errorf("sqlite store: read retention settings: %w", classifyError(err))
	}
	if defaultDays <= 0 {
		defaultDays = 90
	}
	result := domain.RetentionSettings{DefaultDays: defaultDays, EffectiveDays: defaultDays, Source: "config"}
	if override.Valid {
		result.OverrideDays = fromNullableInt64(override)
		result.EffectiveDays = override.Int64
		result.Source = "database"
	}
	return result, nil
}

func (s *Store) SetRetentionOverride(ctx context.Context, days *int64, audit *domain.AuditEvent) (domain.RetentionSettings, error) {
	if err := s.ready(); err != nil {
		return domain.RetentionSettings{}, err
	}
	if days != nil && (*days < 0 || (*days != 0 && *days != 90 && *days != 180 && *days != 365)) {
		return domain.RetentionSettings{}, fmt.Errorf("sqlite store: invalid retention days: %w", domain.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RetentionSettings{}, fmt.Errorf("sqlite store: begin retention settings: %w", classifyError(err))
	}
	if _, err = tx.ExecContext(ctx, `UPDATE carpool_billing_settings SET usage_retention_days=?, updated_at=? WHERE id=1`, nullableInt64(days), toDatabaseTime(s.currentTime())); err != nil {
		return domain.RetentionSettings{}, rollback(tx, fmt.Errorf("sqlite store: update retention settings: %w", classifyError(err)))
	}
	if err = insertAudit(ctx, tx, audit, s.currentTime()); err != nil {
		return domain.RetentionSettings{}, rollback(tx, err)
	}
	if err = tx.Commit(); err != nil {
		return domain.RetentionSettings{}, fmt.Errorf("sqlite store: commit retention settings: %w", classifyError(err))
	}
	return s.GetRetentionSettings(ctx, 90)
}

func (s *Store) PreviewRetention(ctx context.Context, operation string, cutoff time.Time) (int64, int64, error) {
	if err := s.ready(); err != nil {
		return 0, 0, err
	}
	if cutoff.IsZero() || !validRetentionOperation(operation) {
		return 0, 0, fmt.Errorf("sqlite store: invalid retention preview: %w", domain.ErrInvalid)
	}
	var requests, inFlight int64
	switch operation {
	case "usage_details":
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_requests p WHERE p.outcome NOT IN ('in_progress', 'incomplete') AND p.completed_at < ?`, toDatabaseTime(cutoff)).Scan(&requests); err != nil {
			return 0, 0, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_requests WHERE outcome IN ('in_progress', 'incomplete')`).Scan(&inFlight); err != nil {
			return 0, 0, err
		}
	case "closed_periods":
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_periods bp
			WHERE bp.period_to <= ?
			  AND NOT EXISTS (
				SELECT 1 FROM proxy_requests p
				WHERE p.billing_period_id = bp.id AND p.outcome IN ('in_progress', 'incomplete')
			  )`, toDatabaseTime(cutoff)).Scan(&requests); err != nil {
			return 0, 0, err
		}
	case "reset_current_period":
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_periods WHERE period_from <= ? AND period_to > ?`, toDatabaseTime(cutoff), toDatabaseTime(cutoff)).Scan(&requests); err != nil {
			return 0, 0, err
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_requests WHERE outcome IN ('in_progress', 'incomplete')`).Scan(&inFlight); err != nil {
			return 0, 0, err
		}
	}
	return requests, inFlight, nil
}

func (s *Store) ExecuteRetention(ctx context.Context, operation string, cutoff time.Time, batchSize int) (int64, int64, error) {
	if err := s.ready(); err != nil {
		return 0, 0, err
	}
	if cutoff.IsZero() || !validRetentionOperation(operation) || batchSize < 1 || batchSize > 1000 {
		return 0, 0, fmt.Errorf("sqlite store: invalid retention execution: %w", domain.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, classifyError(err)
	}
	var deleted, inFlight int64
	switch operation {
	case "usage_details":
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_requests WHERE outcome IN ('in_progress', 'incomplete')`).Scan(&inFlight); err != nil {
			return 0, 0, rollback(tx, err)
		}
		// Freeze a request batch first; deleting complete requests and their scopes
		// together keeps logical request totals and foreign-key cascades consistent.
		rows, e := tx.QueryContext(ctx, `SELECT request_id FROM proxy_requests WHERE outcome NOT IN ('in_progress', 'incomplete') AND completed_at < ? ORDER BY completed_at, request_id LIMIT ?`, toDatabaseTime(cutoff), batchSize)
		if e != nil {
			return 0, 0, rollback(tx, e)
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return 0, 0, rollback(tx, e)
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, e = tx.ExecContext(ctx, `DELETE FROM usage_events WHERE request_id=?`, id); e != nil {
				return 0, 0, rollback(tx, e)
			}
			if _, e = tx.ExecContext(ctx, `DELETE FROM proxy_request_auth_scopes WHERE request_id=?`, id); e != nil {
				return 0, 0, rollback(tx, e)
			}
			if _, e = tx.ExecContext(ctx, `DELETE FROM proxy_requests WHERE request_id=? AND outcome NOT IN ('in_progress', 'incomplete')`, id); e != nil {
				return 0, 0, rollback(tx, e)
			}
		}
		deleted = int64(len(ids))
	case "closed_periods":
		eligiblePeriods := `(SELECT bp.id FROM billing_periods bp
			WHERE bp.period_to <= ?
			  AND NOT EXISTS (
				SELECT 1 FROM proxy_requests p
				WHERE p.billing_period_id = bp.id AND p.outcome IN ('in_progress', 'incomplete')
			  )
			ORDER BY bp.period_to, bp.id LIMIT ?)`
		if _, e := tx.ExecContext(ctx, `UPDATE proxy_requests SET billing_period_id = NULL WHERE billing_period_id IN `+eligiblePeriods, toDatabaseTime(cutoff), batchSize); e != nil {
			return 0, 0, rollback(tx, e)
		}
		if _, e := tx.ExecContext(ctx, `UPDATE usage_events SET billing_period_id = NULL WHERE billing_period_id IN `+eligiblePeriods, toDatabaseTime(cutoff), batchSize); e != nil {
			return 0, 0, rollback(tx, e)
		}
		res, e := tx.ExecContext(ctx, `DELETE FROM billing_periods WHERE id IN `+eligiblePeriods, toDatabaseTime(cutoff), batchSize)
		if e != nil {
			return 0, 0, rollback(tx, e)
		}
		deleted, err = res.RowsAffected()
	case "reset_current_period":
		res, e := tx.ExecContext(ctx, `UPDATE billing_periods SET reset_baseline_nano_usd=confirmed_nano_usd, unknown_cost_events=0, revision=revision+1 WHERE period_from <= ? AND period_to > ?`, toDatabaseTime(cutoff), toDatabaseTime(cutoff))
		if e != nil {
			return 0, 0, rollback(tx, e)
		}
		deleted, err = res.RowsAffected()
	}
	if err != nil {
		return 0, 0, rollback(tx, err)
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, classifyError(err)
	}
	return deleted, inFlight, nil
}

func validRetentionOperation(op string) bool {
	return op == "usage_details" || op == "closed_periods" || op == "reset_current_period"
}

func (s *Store) CreateRetentionJob(ctx context.Context, operation, actorRef string, expected int64) (domain.RetentionJob, error) {
	if !validRetentionOperation(operation) || strings.TrimSpace(actorRef) == "" {
		return domain.RetentionJob{}, fmt.Errorf("sqlite store: invalid retention job: %w", domain.ErrInvalid)
	}
	now := s.currentTime()
	job := domain.RetentionJob{ID: uuid.NewString(), Operation: operation, Status: "queued", RequestedAt: now, ActorRef: actorRef, ExpectedCount: expected}
	_, err := s.db.ExecContext(ctx, `INSERT INTO retention_jobs(id, operation, status, requested_at, actor_ref, expected_count, deleted_count, in_flight_count, failure_reason) VALUES(?,?,?,?,?,?,?,?,?)`, job.ID, job.Operation, job.Status, toDatabaseTime(now), job.ActorRef, expected, 0, 0, "")
	if err != nil {
		return domain.RetentionJob{}, fmt.Errorf("sqlite store: create retention job: %w", classifyError(err))
	}
	return job, nil
}

func (s *Store) GetRetentionJob(ctx context.Context, id string) (domain.RetentionJob, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.RetentionJob{}, errReady
	}
	if strings.TrimSpace(id) == "" {
		return domain.RetentionJob{}, fmt.Errorf("sqlite store: retention job ID is required: %w", domain.ErrInvalid)
	}
	var j domain.RetentionJob
	var requested, completed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,operation,status,requested_at,completed_at,actor_ref,confirmation,expected_count,deleted_count,in_flight_count,failure_reason FROM retention_jobs WHERE id=?`, id).Scan(&j.ID, &j.Operation, &j.Status, &requested, &completed, &j.ActorRef, &j.Confirmation, &j.ExpectedCount, &j.DeletedCount, &j.InFlightCount, &j.FailureReason)
	if err != nil {
		return j, scanError("get retention job", err)
	}
	j.RequestedAt = fromDatabaseTime(requested.Int64)
	j.CompletedAt = fromNullableDatabaseTime(completed)
	return j, nil
}

func (s *Store) FinishRetentionJob(ctx context.Context, id, status string, deleted, inFlight int64, reason string) error {
	if id == "" || (status != "completed" && status != "failed") || deleted < 0 || inFlight < 0 {
		return fmt.Errorf("sqlite store: invalid retention job result: %w", domain.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE retention_jobs SET status=?, completed_at=?, deleted_count=?, in_flight_count=?, failure_reason=? WHERE id=?`, status, toDatabaseTime(s.currentTime()), deleted, inFlight, reason, id)
	if err != nil {
		return fmt.Errorf("sqlite store: finish retention job: %w", classifyError(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.ErrNotFound
	}
	return nil
}
