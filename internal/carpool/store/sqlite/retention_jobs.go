package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

// Confirmation contains only server-selected IDs and their observed state. It is
// never accepted from the client or exposed by the HTTP response.
type retentionConfirmation struct {
	Version  int                      `json:"version"`
	Cutoff   int64                    `json:"cutoff"`
	Requests []retentionRequestTarget `json:"requests,omitempty"`
	Periods  []retentionPeriodTarget  `json:"periods,omitempty"`
}
type retentionRequestTarget struct {
	ID          string `json:"id"`
	Outcome     string `json:"outcome"`
	CompletedAt int64  `json:"completed_at"`
	Events      int64  `json:"events"`
}
type retentionPeriodTarget struct {
	ID        string `json:"id"`
	From      int64  `json:"from"`
	To        int64  `json:"to"`
	Revision  int64  `json:"revision"`
	Confirmed int64  `json:"confirmed_nano_usd"`
	Baseline  int64  `json:"reset_baseline_nano_usd"`
	Unknown   int64  `json:"unknown_cost_events"`
}

func (s *Store) PreviewRetentionJob(ctx context.Context, operation, actorRef string, cutoff time.Time) (domain.RetentionJob, error) {
	if err := s.ready(); err != nil {
		return domain.RetentionJob{}, err
	}
	if !validRetentionOperation(operation) || strings.TrimSpace(actorRef) == "" || cutoff.IsZero() {
		return domain.RetentionJob{}, domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	job, err := s.previewRetentionJob(ctx, tx, operation, actorRef, cutoff)
	if err != nil {
		return domain.RetentionJob{}, rollback(tx, err)
	}
	if err = tx.Commit(); err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	return job, nil
}

func (s *Store) previewRetentionJob(ctx context.Context, tx *sql.Tx, operation, actorRef string, cutoff time.Time) (domain.RetentionJob, error) {
	now := s.currentTime().Truncate(time.Microsecond)
	job := domain.RetentionJob{ID: uuid.NewString(), Operation: operation, ActorRef: actorRef, Status: "queued", RequestedAt: now}
	confirmation := retentionConfirmation{Version: 1, Cutoff: toDatabaseTime(cutoff)}
	var rows *sql.Rows
	var err error
	if operation == "usage_details" {
		rows, err = tx.QueryContext(ctx, `SELECT p.request_id,p.outcome,p.completed_at,(SELECT COUNT(*) FROM usage_events e WHERE e.request_id=p.request_id) FROM proxy_requests p WHERE p.outcome NOT IN ('in_progress','incomplete') AND p.completed_at < ? ORDER BY p.completed_at,p.request_id LIMIT ?`, confirmation.Cutoff, domain.RetentionBatchLimit)
	} else {
		// Exclude no-op resets so another explicit preview can reach the next
		// batch instead of repeatedly selecting the same zero-use periods.
		predicate := `bp.period_from <= ? AND bp.period_to > ? AND (bp.confirmed_nano_usd != bp.reset_baseline_nano_usd OR bp.unknown_cost_events != 0)`
		args := []any{confirmation.Cutoff, confirmation.Cutoff, domain.RetentionBatchLimit}
		if operation == "closed_periods" {
			predicate = `bp.period_to <= ? AND NOT EXISTS (SELECT 1 FROM proxy_requests p WHERE p.billing_period_id=bp.id AND p.outcome IN ('in_progress','incomplete'))`
			args = []any{confirmation.Cutoff, domain.RetentionBatchLimit}
		}
		rows, err = tx.QueryContext(ctx, `SELECT bp.id,bp.period_from,bp.period_to,bp.revision,bp.confirmed_nano_usd,bp.reset_baseline_nano_usd,bp.unknown_cost_events FROM billing_periods bp WHERE `+predicate+` ORDER BY bp.period_to,bp.id LIMIT ?`, args...)
	}
	if err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	for rows.Next() {
		if operation == "usage_details" {
			var target retentionRequestTarget
			err = rows.Scan(&target.ID, &target.Outcome, &target.CompletedAt, &target.Events)
			confirmation.Requests = append(confirmation.Requests, target)
		} else {
			var target retentionPeriodTarget
			err = rows.Scan(&target.ID, &target.From, &target.To, &target.Revision, &target.Confirmed, &target.Baseline, &target.Unknown)
			confirmation.Periods = append(confirmation.Periods, target)
		}
		if err != nil {
			return domain.RetentionJob{}, errors.Join(err, rows.Close())
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	job.ExpectedCount = int64(len(confirmation.Requests) + len(confirmation.Periods))
	if operation == "usage_details" {
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_requests WHERE outcome IN ('in_progress','incomplete')`).Scan(&job.InFlightCount)
	} else if operation == "reset_current_period" {
		for _, target := range confirmation.Periods {
			var count int64
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_requests WHERE billing_period_id=? AND outcome IN ('in_progress','incomplete')`, target.ID).Scan(&count); err != nil {
				break
			}
			job.InFlightCount += count
		}
	}
	if err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	encoded, err := json.Marshal(confirmation)
	if err != nil {
		return domain.RetentionJob{}, err
	}
	job.Confirmation = string(encoded)
	_, err = tx.ExecContext(ctx, `INSERT INTO retention_jobs(id,operation,status,requested_at,actor_ref,confirmation,expected_count,in_flight_count) VALUES(?,?,?,?,?,?,?,?)`, job.ID, job.Operation, job.Status, toDatabaseTime(now), job.ActorRef, job.Confirmation, job.ExpectedCount, job.InFlightCount)
	if err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	return job, nil
}

func (s *Store) ConfirmRetentionJob(ctx context.Context, id, operation, actorRef string) (domain.RetentionJob, error) {
	if err := s.ready(); err != nil {
		return domain.RetentionJob{}, err
	}
	if strings.TrimSpace(id) == "" || !validRetentionOperation(operation) || strings.TrimSpace(actorRef) == "" {
		return domain.RetentionJob{}, domain.ErrInvalid
	}
	// The store DSN uses immediate transactions: competing confirmations wait for
	// the first commit and then observe its completed result, without re-execution.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	job, err := s.confirmRetentionJob(ctx, tx, id, operation, actorRef)
	if err != nil {
		return domain.RetentionJob{}, rollback(tx, err)
	}
	if err = tx.Commit(); err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	return job, nil
}

func (s *Store) confirmRetentionJob(ctx context.Context, tx *sql.Tx, id, operation, actorRef string) (domain.RetentionJob, error) {
	job, err := scanRetentionJob(tx.QueryRowContext(ctx, retentionJobSelect+` WHERE id=?`, id))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
	}
	if err != nil {
		return domain.RetentionJob{}, err
	}
	if job.ActorRef != actorRef || job.Operation != operation {
		return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
	}
	if job.Status == "completed" {
		return job, nil
	}
	now := s.currentTime().Truncate(time.Microsecond)
	if job.Status != "queued" || now.Before(job.RequestedAt) || !now.Before(job.RequestedAt.Add(domain.RetentionPreviewTTL)) {
		return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
	}
	var confirmation retentionConfirmation
	if err = json.Unmarshal([]byte(job.Confirmation), &confirmation); err != nil {
		return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
	}
	if confirmation.Version != 1 || confirmation.Cutoff == 0 ||
		job.ExpectedCount != int64(len(confirmation.Requests)+len(confirmation.Periods)) ||
		job.ExpectedCount > domain.RetentionBatchLimit ||
		(operation == "usage_details" && len(confirmation.Periods) != 0) ||
		(operation != "usage_details" && len(confirmation.Requests) != 0) {
		return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
	}
	// Validate every target before changing any of them. New targets never enter
	// this batch, even if they would match the original time cutoff.
	for _, target := range confirmation.Requests {
		var current retentionRequestTarget
		err = tx.QueryRowContext(ctx, `SELECT p.request_id,p.outcome,p.completed_at,(SELECT COUNT(*) FROM usage_events e WHERE e.request_id=p.request_id) FROM proxy_requests p WHERE p.request_id=? AND p.outcome NOT IN ('in_progress','incomplete') AND p.completed_at < ?`, target.ID, confirmation.Cutoff).Scan(&current.ID, &current.Outcome, &current.CompletedAt, &current.Events)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current != target) {
			return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
		}
		if err != nil {
			return domain.RetentionJob{}, classifyError(err)
		}
	}
	for _, target := range confirmation.Periods {
		var current retentionPeriodTarget
		err = tx.QueryRowContext(ctx, `SELECT id,period_from,period_to,revision,confirmed_nano_usd,reset_baseline_nano_usd,unknown_cost_events FROM billing_periods WHERE id=?`, target.ID).Scan(&current.ID, &current.From, &current.To, &current.Revision, &current.Confirmed, &current.Baseline, &current.Unknown)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current != target) {
			return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
		}
		if err != nil {
			return domain.RetentionJob{}, classifyError(err)
		}
		if operation == "reset_current_period" {
			if target.From > confirmation.Cutoff || target.To <= confirmation.Cutoff || target.From > toDatabaseTime(now) || target.To <= toDatabaseTime(now) {
				return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
			}
		} else {
			if target.To > confirmation.Cutoff || target.To > toDatabaseTime(now) {
				return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
			}
			var protected int
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM proxy_requests WHERE billing_period_id=? AND outcome IN ('in_progress','incomplete'))`, target.ID).Scan(&protected); err != nil {
				return domain.RetentionJob{}, classifyError(err)
			}
			if protected != 0 {
				return domain.RetentionJob{}, domain.ErrRetentionPreviewInvalid
			}
		}
	}
	for _, target := range confirmation.Requests {
		for _, query := range []string{`DELETE FROM usage_events WHERE request_id=?`, `DELETE FROM proxy_request_auth_scopes WHERE request_id=?`} {
			if _, err = tx.ExecContext(ctx, query, target.ID); err != nil {
				return domain.RetentionJob{}, classifyError(err)
			}
		}
		if err = retentionMutateOne(ctx, tx, `DELETE FROM proxy_requests WHERE request_id=?`, target.ID); err != nil {
			return domain.RetentionJob{}, err
		}
		job.DeletedCount++
	}
	for _, target := range confirmation.Periods {
		queries := []string{`UPDATE billing_periods SET reset_baseline_nano_usd=confirmed_nano_usd,unknown_cost_events=0,revision=revision+1 WHERE id=?`}
		if operation == "closed_periods" {
			queries = []string{`UPDATE proxy_requests SET billing_period_id=NULL WHERE billing_period_id=?`, `UPDATE usage_events SET billing_period_id=NULL WHERE billing_period_id=?`, `DELETE FROM billing_periods WHERE id=?`}
		}
		for _, query := range queries[:len(queries)-1] {
			if _, err = tx.ExecContext(ctx, query, target.ID); err != nil {
				return domain.RetentionJob{}, classifyError(err)
			}
		}
		if err = retentionMutateOne(ctx, tx, queries[len(queries)-1], target.ID); err != nil {
			return domain.RetentionJob{}, err
		}
		job.DeletedCount++
	}
	job.Status = "completed"
	job.CompletedAt = &now
	if err = retentionMutateOne(ctx, tx, `UPDATE retention_jobs SET status='completed',completed_at=?,deleted_count=? WHERE id=?`, toDatabaseTime(now), job.DeletedCount, job.ID); err != nil {
		return domain.RetentionJob{}, classifyError(err)
	}
	// Preserve the pre-reset amounts, baseline and unknown counts per period in
	// the same audit commit. No request bodies, credentials or raw errors are kept.
	metadata, err := json.Marshal(struct {
		Operation string                  `json:"operation"`
		Expected  int64                   `json:"expected_count"`
		Deleted   int64                   `json:"deleted_count"`
		Periods   []retentionPeriodTarget `json:"periods,omitempty"`
	}{operation, job.ExpectedCount, job.DeletedCount, confirmation.Periods})
	if err != nil {
		return domain.RetentionJob{}, err
	}
	audit := domain.AuditEvent{OccurredAt: now, ActorType: "user", ActorRef: actorRef, Action: "retention." + operation, TargetType: "retention_job", TargetRef: job.ID, Result: "succeeded", MetadataJSON: metadata}
	if err = insertAudit(ctx, tx, &audit, now); err != nil {
		return domain.RetentionJob{}, err
	}
	return job, nil
}

// A logical target must be changed exactly once. Child event counts are not
// bounded, but their deletion shares the logical target's transaction.
func retentionMutateOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return classifyError(err)
	}
	count, err := affectedRows("retention mutation", result)
	if err != nil {
		return err
	}
	if count != 1 {
		return domain.ErrRetentionPreviewInvalid
	}
	return nil
}

const retentionJobSelect = `SELECT id,operation,status,requested_at,completed_at,actor_ref,confirmation,expected_count,deleted_count,in_flight_count,failure_reason FROM retention_jobs`

func scanRetentionJob(row *sql.Row) (domain.RetentionJob, error) {
	var job domain.RetentionJob
	var requested int64
	var completed sql.NullInt64
	err := row.Scan(&job.ID, &job.Operation, &job.Status, &requested, &completed, &job.ActorRef, &job.Confirmation, &job.ExpectedCount, &job.DeletedCount, &job.InFlightCount, &job.FailureReason)
	if err != nil {
		return domain.RetentionJob{}, scanError("get retention job", err)
	}
	job.RequestedAt = fromDatabaseTime(requested)
	job.CompletedAt = fromNullableDatabaseTime(completed)
	return job, nil
}

func (s *Store) GetRetentionJob(ctx context.Context, id string) (domain.RetentionJob, error) {
	if err := s.ready(); err != nil {
		return domain.RetentionJob{}, err
	}
	if strings.TrimSpace(id) == "" {
		return domain.RetentionJob{}, fmt.Errorf("sqlite store: retention job ID is required: %w", domain.ErrInvalid)
	}
	return scanRetentionJob(s.db.QueryRowContext(ctx, retentionJobSelect+` WHERE id=?`, id))
}
