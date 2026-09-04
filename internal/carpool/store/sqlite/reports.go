package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func (s *Store) ListAuditEvents(ctx context.Context, before time.Time, beforeID string, limit int) (events []domain.AuditEvent, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	limit, errLimit := normalizeLimit(limit)
	if errLimit != nil {
		return nil, errLimit
	}
	beforeValue := int64(0)
	if !before.IsZero() {
		beforeValue = toDatabaseTime(before)
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT id, occurred_at, request_id, actor_type, actor_ref, action,
		       target_type, target_ref, result, reason_code, metadata_json
		FROM audit_events
		WHERE (? = 0 OR occurred_at < ? OR (occurred_at = ? AND id < ?))
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?
	`, beforeValue, beforeValue, beforeValue, beforeID, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list audit events: %w", classifyError(errQuery))
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("sqlite store: close audit event rows: %w", errClose))
		}
	}()
	for rows.Next() {
		var event domain.AuditEvent
		var occurredAt int64
		var requestID sql.NullString
		var metadata string
		if errScan := rows.Scan(&event.ID, &occurredAt, &requestID, &event.ActorType,
			&event.ActorRef, &event.Action, &event.TargetType, &event.TargetRef,
			&event.Result, &event.ReasonCode, &metadata); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan audit event: %w", errScan)
		}
		event.OccurredAt = fromDatabaseTime(occurredAt)
		event.RequestID = requestID.String
		event.MetadataJSON = json.RawMessage(metadata)
		events = append(events, event)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate audit events: %w", errRows)
	}
	return events, nil
}

func (s *Store) MemberUsageByCar(ctx context.Context, carID string, from, to time.Time) (aggregates []domain.MemberUsageAggregate, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	if errRange := validateReportRange(carID, from, to); errRange != nil {
		return nil, errRange
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		WITH request_facts AS (
			SELECT membership_id, member_ref_snapshot, display_name_snapshot,
			       COUNT(*) AS request_count,
			       SUM(CASE WHEN outcome = 'succeeded' THEN 1 ELSE 0 END) AS succeeded_count,
			       SUM(CASE WHEN outcome = 'failed' THEN 1 ELSE 0 END) AS failed_count,
			       SUM(CASE WHEN outcome = 'rejected' THEN 1 ELSE 0 END) AS rejected_count,
			       SUM(CASE WHEN outcome = 'canceled' THEN 1 ELSE 0 END) AS canceled_count,
			       SUM(CASE WHEN outcome = 'incomplete' THEN 1 ELSE 0 END) AS incomplete_count
			FROM proxy_requests
			WHERE car_id = ? AND membership_id IS NOT NULL
			  AND started_at >= ? AND started_at < ?
			GROUP BY membership_id, member_ref_snapshot, display_name_snapshot
		),
		usage_facts AS (
			SELECT p.membership_id, p.member_ref_snapshot, p.display_name_snapshot,
			       SUM(CASE WHEN e.usage_known = 1 THEN COALESCE(e.input_tokens, 0) ELSE 0 END) AS input_tokens,
			       SUM(CASE WHEN e.usage_known = 1 THEN COALESCE(e.output_tokens, 0) ELSE 0 END) AS output_tokens,
			       SUM(CASE WHEN e.usage_known = 1 THEN COALESCE(e.total_tokens, 0) ELSE 0 END) AS total_tokens,
			       SUM(CASE WHEN e.usage_known = 0 THEN 1 ELSE 0 END) AS unknown_count
			FROM usage_events e
			JOIN proxy_requests p ON p.request_id = e.request_id
			WHERE p.car_id = ? AND p.membership_id IS NOT NULL
			  AND e.requested_at >= ? AND e.requested_at < ?
			GROUP BY p.membership_id, p.member_ref_snapshot, p.display_name_snapshot
		),
		members AS (
			SELECT membership_id, member_ref_snapshot, display_name_snapshot FROM request_facts
			UNION
			SELECT membership_id, member_ref_snapshot, display_name_snapshot FROM usage_facts
		)
		SELECT m.membership_id, m.member_ref_snapshot, m.display_name_snapshot,
		       COALESCE(r.request_count, 0),
		       COALESCE(r.succeeded_count, 0),
		       COALESCE(r.failed_count, 0),
		       COALESCE(r.rejected_count, 0),
		       COALESCE(r.canceled_count, 0),
		       COALESCE(r.incomplete_count, 0),
		       COALESCE(u.input_tokens, 0),
		       COALESCE(u.output_tokens, 0),
		       COALESCE(u.total_tokens, 0),
		       COALESCE(u.unknown_count, 0)
		FROM members m
		LEFT JOIN request_facts r
		  ON r.membership_id = m.membership_id
		 AND r.member_ref_snapshot = m.member_ref_snapshot
		 AND r.display_name_snapshot = m.display_name_snapshot
		LEFT JOIN usage_facts u
		  ON u.membership_id = m.membership_id
		 AND u.member_ref_snapshot = m.member_ref_snapshot
		 AND u.display_name_snapshot = m.display_name_snapshot
		ORDER BY m.display_name_snapshot, m.membership_id
	`, carID, toDatabaseTime(from), toDatabaseTime(to), carID, toDatabaseTime(from), toDatabaseTime(to))
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: query member usage: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "member usage")()
	for rows.Next() {
		var aggregate domain.MemberUsageAggregate
		if errScan := rows.Scan(&aggregate.MembershipID, &aggregate.MemberRef, &aggregate.DisplayName,
			&aggregate.RequestCount, &aggregate.SucceededCount, &aggregate.FailedCount,
			&aggregate.RejectedCount, &aggregate.CanceledCount, &aggregate.IncompleteCount,
			&aggregate.KnownInputTokens, &aggregate.KnownOutputTokens, &aggregate.KnownTotalTokens,
			&aggregate.UnknownUsageCount); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan member usage: %w", errScan)
		}
		aggregates = append(aggregates, aggregate)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate member usage: %w", errRows)
	}
	return aggregates, nil
}

func (s *Store) AccountUsageByCar(ctx context.Context, carID string, from, to time.Time) (aggregates []domain.AccountUsageAggregate, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	if errRange := validateReportRange(carID, from, to); errRange != nil {
		return nil, errRange
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT e.assignment_id, e.account_ref_snapshot, e.safe_label_snapshot, e.provider,
		       COUNT(DISTINCT e.request_id),
		       SUM(CASE WHEN e.usage_known = 1 THEN COALESCE(e.input_tokens, 0) ELSE 0 END),
		       SUM(CASE WHEN e.usage_known = 1 THEN COALESCE(e.output_tokens, 0) ELSE 0 END),
		       SUM(CASE WHEN e.usage_known = 1 THEN COALESCE(e.total_tokens, 0) ELSE 0 END),
		       SUM(CASE WHEN e.usage_known = 0 THEN 1 ELSE 0 END)
		FROM usage_events e
		JOIN proxy_requests p ON p.request_id = e.request_id
		WHERE p.car_id = ? AND e.requested_at >= ? AND e.requested_at < ?
		GROUP BY e.assignment_id, e.account_ref_snapshot, e.safe_label_snapshot, e.provider
		ORDER BY e.safe_label_snapshot, e.assignment_id
	`, carID, toDatabaseTime(from), toDatabaseTime(to))
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: query account usage: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "account usage")()
	for rows.Next() {
		var aggregate domain.AccountUsageAggregate
		if errScan := rows.Scan(&aggregate.AssignmentID, &aggregate.AccountRef, &aggregate.SafeLabel,
			&aggregate.Provider, &aggregate.RequestCount, &aggregate.KnownInputTokens,
			&aggregate.KnownOutputTokens, &aggregate.KnownTotalTokens, &aggregate.UnknownUsageCount); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan account usage: %w", errScan)
		}
		aggregates = append(aggregates, aggregate)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate account usage: %w", errRows)
	}
	return aggregates, nil
}

func (s *Store) AdminUsage(ctx context.Context, filter domain.AdminUsageFilter) (aggregates []domain.AdminUsageAggregate, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	if filter.From.IsZero() || filter.To.IsZero() || !filter.From.Before(filter.To) || !filter.GroupBy.Valid() {
		return nil, fmt.Errorf("sqlite store: invalid administrator usage filter: %w", domain.ErrInvalid)
	}

	var requestGroup, usageGroup, entityJoin, refColumn, labelColumn, providerColumn string
	switch filter.GroupBy {
	case domain.AdminUsageGroupCar:
		requestGroup = "COALESCE(p.car_id, '')"
		usageGroup = "COALESCE(p.car_id, '')"
		entityJoin = "LEFT JOIN cars entity ON entity.id = NULLIF(g.group_id, '')"
		refColumn = "COALESCE(entity.car_ref, '')"
		labelColumn = "COALESCE(entity.name, '')"
		providerColumn = "''"
	case domain.AdminUsageGroupUser:
		requestGroup = "p.user_id"
		usageGroup = "p.user_id"
		entityJoin = "LEFT JOIN users entity ON entity.id = g.group_id"
		refColumn = "COALESCE(entity.user_ref, '')"
		labelColumn = "COALESCE(entity.default_display_name, '')"
		providerColumn = "''"
	case domain.AdminUsageGroupAccount:
		requestGroup = "COALESCE(ra.assignment_id, '')"
		usageGroup = "e.assignment_id"
		entityJoin = "LEFT JOIN car_auth_assignments entity ON entity.id = NULLIF(g.group_id, '')"
		refColumn = "COALESCE(entity.account_ref, '')"
		labelColumn = "COALESCE(entity.safe_label, '')"
		providerColumn = "COALESCE(entity.provider_snapshot, '')"
	}

	query := fmt.Sprintf(`
		WITH request_accounts AS (
			SELECT DISTINCT request_id, assignment_id
			FROM usage_events
		),
		request_dimensions AS (
			SELECT DISTINCT p.request_id, %s AS group_id, p.outcome
			FROM proxy_requests p
			LEFT JOIN request_accounts ra ON ra.request_id = p.request_id
			WHERE p.started_at >= ? AND p.started_at < ?
			  AND (? = '' OR p.car_id = ?)
			  AND (? = '' OR p.user_id = ?)
			  AND (? = '' OR ra.assignment_id = ?)
		),
		request_facts AS (
			SELECT group_id,
			       COUNT(*) AS request_count,
			       SUM(CASE WHEN outcome = 'in_progress' THEN 1 ELSE 0 END) AS in_progress_count,
			       SUM(CASE WHEN outcome = 'succeeded' THEN 1 ELSE 0 END) AS succeeded_count,
			       SUM(CASE WHEN outcome = 'failed' THEN 1 ELSE 0 END) AS failed_count,
			       SUM(CASE WHEN outcome = 'rejected' THEN 1 ELSE 0 END) AS rejected_count,
			       SUM(CASE WHEN outcome = 'canceled' THEN 1 ELSE 0 END) AS canceled_count,
			       SUM(CASE WHEN outcome = 'incomplete' THEN 1 ELSE 0 END) AS incomplete_count
			FROM request_dimensions
			GROUP BY group_id
		),
		usage_dimensions AS (
			SELECT %s AS group_id, e.request_id, e.usage_known,
			       e.input_tokens, e.output_tokens, e.cached_tokens,
			       e.reasoning_tokens, e.total_tokens
			FROM usage_events e
			JOIN proxy_requests p ON p.request_id = e.request_id
			WHERE e.requested_at >= ? AND e.requested_at < ?
			  AND (? = '' OR p.car_id = ?)
			  AND (? = '' OR p.user_id = ?)
			  AND (? = '' OR e.assignment_id = ?)
		),
		usage_facts AS (
			SELECT group_id,
			       SUM(CASE WHEN usage_known = 1 THEN COALESCE(input_tokens, 0) ELSE 0 END) AS input_tokens,
			       SUM(CASE WHEN usage_known = 1 THEN COALESCE(output_tokens, 0) ELSE 0 END) AS output_tokens,
			       SUM(CASE WHEN usage_known = 1 THEN COALESCE(cached_tokens, 0) ELSE 0 END) AS cached_tokens,
			       SUM(CASE WHEN usage_known = 1 THEN COALESCE(reasoning_tokens, 0) ELSE 0 END) AS reasoning_tokens,
			       SUM(CASE WHEN usage_known = 1 THEN COALESCE(total_tokens, 0) ELSE 0 END) AS total_tokens,
			       SUM(CASE WHEN usage_known = 0 THEN 1 ELSE 0 END) AS unknown_count
			FROM usage_dimensions
			GROUP BY group_id
		),
		groups AS (
			SELECT group_id FROM request_facts
			UNION
			SELECT group_id FROM usage_facts
		)
		SELECT g.group_id, %s, %s, %s,
		       COALESCE(r.request_count, 0),
		       COALESCE(r.in_progress_count, 0),
		       COALESCE(r.succeeded_count, 0),
		       COALESCE(r.failed_count, 0),
		       COALESCE(r.rejected_count, 0),
		       COALESCE(r.canceled_count, 0),
		       COALESCE(r.incomplete_count, 0),
		       COALESCE(u.input_tokens, 0),
		       COALESCE(u.output_tokens, 0),
		       COALESCE(u.cached_tokens, 0),
		       COALESCE(u.reasoning_tokens, 0),
		       COALESCE(u.total_tokens, 0),
		       COALESCE(u.unknown_count, 0)
		FROM groups g
		%s
		LEFT JOIN request_facts r ON r.group_id = g.group_id
		LEFT JOIN usage_facts u ON u.group_id = g.group_id
		ORDER BY CASE WHEN g.group_id = '' THEN 1 ELSE 0 END, 3 COLLATE NOCASE, 2
	`, requestGroup, usageGroup, refColumn, labelColumn, providerColumn, entityJoin)
	fromValue, toValue := toDatabaseTime(filter.From), toDatabaseTime(filter.To)
	rows, errQuery := s.db.QueryContext(ctx, query,
		fromValue, toValue,
		filter.CarID, filter.CarID,
		filter.UserID, filter.UserID,
		filter.AssignmentID, filter.AssignmentID,
		fromValue, toValue,
		filter.CarID, filter.CarID,
		filter.UserID, filter.UserID,
		filter.AssignmentID, filter.AssignmentID,
	)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: query administrator usage: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "administrator usage")()
	for rows.Next() {
		var groupID string
		var aggregate domain.AdminUsageAggregate
		if errScan := rows.Scan(
			&groupID, &aggregate.GroupRef, &aggregate.GroupLabel, &aggregate.Provider,
			&aggregate.RequestCount, &aggregate.InProgressCount, &aggregate.SucceededCount,
			&aggregate.FailedCount, &aggregate.RejectedCount, &aggregate.CanceledCount,
			&aggregate.IncompleteCount, &aggregate.KnownInputTokens,
			&aggregate.KnownOutputTokens, &aggregate.KnownCachedTokens,
			&aggregate.KnownReasoningTokens, &aggregate.KnownTotalTokens,
			&aggregate.UnknownUsageCount,
		); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan administrator usage: %w", errScan)
		}
		aggregate.GroupBy = filter.GroupBy
		aggregates = append(aggregates, aggregate)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate administrator usage: %w", errRows)
	}
	return aggregates, nil
}

func validateReportRange(carID string, from, to time.Time) error {
	if carID == "" || from.IsZero() || to.IsZero() || !from.Before(to) {
		return fmt.Errorf("sqlite store: invalid report range: %w", domain.ErrInvalid)
	}
	return nil
}
