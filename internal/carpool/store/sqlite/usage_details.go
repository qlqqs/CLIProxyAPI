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

// ListUsageRequests returns one row per logical request. The before tuple is
// exclusive and must be the last (started_at, request_id) pair from a prior
// page.
func (s *Store) ListUsageRequests(ctx context.Context, filter domain.UsageRequestFilter, before time.Time, beforeID string, limit int) (items []domain.UsageRequestDetail, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	if filter.From.IsZero() || filter.To.IsZero() || !filter.From.Before(filter.To) || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("sqlite store: invalid usage request filter: %w", domain.ErrInvalid)
	}
	where := `p.started_at >= ? AND p.started_at < ?`
	args := []any{toDatabaseTime(filter.From), toDatabaseTime(filter.To)}
	add := func(condition string, value any) { where += " AND " + condition; args = append(args, value) }
	if filter.CarID != "" {
		add("p.car_id = ?", filter.CarID)
	}
	if filter.UserID != "" {
		add("p.user_id = ?", filter.UserID)
	}
	if filter.AssignmentID != "" {
		add("EXISTS (SELECT 1 FROM usage_events ef WHERE ef.request_id = p.request_id AND ef.assignment_id = ?)", filter.AssignmentID)
	}
	if filter.APIKeyID != "" {
		add("p.api_key_id = ?", filter.APIKeyID)
	}
	if filter.RequestedModel != "" {
		add("(p.requested_model = ? OR EXISTS (SELECT 1 FROM usage_events em WHERE em.request_id = p.request_id AND em.model = ?))", filter.RequestedModel)
		args = append(args, filter.RequestedModel)
	}
	if filter.Outcome != "" {
		add("p.outcome = ?", filter.Outcome)
	}
	if filter.BillingStatus != "" {
		add("p.billing_status = ?", filter.BillingStatus)
	}
	if filter.RequestID != "" {
		add("p.request_id = ?", filter.RequestID)
	}
	if !before.IsZero() {
		where += " AND (p.started_at < ? OR (p.started_at = ? AND p.request_id < ?))"
		args = append(args, toDatabaseTime(before), toDatabaseTime(before), beforeID)
	}
	query := fmt.Sprintf(`
		SELECT p.request_id, p.user_id, p.api_key_id, p.car_id, p.membership_id,
		       p.member_ref_snapshot, p.display_name_snapshot, p.scope_hash, p.scope_size,
		       p.source_format, p.requested_model, p.stream, p.started_at, p.completed_at,
		       p.outcome, p.status_class, p.reason_code, p.upstream_attempted,
		       p.billing_period_id, p.pricing_catalog_hash, p.pricing_coverage_from,
		       p.billing_status, p.billed_nano_usd,
		       COALESCE(u.user_ref, ''), COALESCE(c.car_ref, ''),
		       (SELECT COUNT(*) FROM usage_events e WHERE e.request_id = p.request_id),
		       (SELECT COUNT(*) FROM usage_events e WHERE e.request_id = p.request_id AND e.pricing_status = 'unknown')
		FROM proxy_requests p
		LEFT JOIN users u ON u.id = p.user_id
		LEFT JOIN cars c ON c.id = p.car_id
		WHERE %s
		ORDER BY p.started_at DESC, p.request_id DESC
		LIMIT ?`, where)
	args = append(args, limit)
	rows, errQuery := s.db.QueryContext(ctx, query, args...)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list usage requests: %w", classifyError(errQuery))
	}
	defer func() {
		if e := rows.Close(); e != nil {
			err = errors.Join(err, e)
		}
	}()
	for rows.Next() {
		item, scanErr := scanUsageRequestDetail(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate usage requests: %w", errRows)
	}
	return items, nil
}

func (s *Store) CountUsageRequests(ctx context.Context, filter domain.UsageRequestFilter) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	if filter.From.IsZero() || filter.To.IsZero() || !filter.From.Before(filter.To) {
		return 0, fmt.Errorf("sqlite store: invalid usage request range: %w", domain.ErrInvalid)
	}
	where := `p.started_at >= ? AND p.started_at < ?`
	args := []any{toDatabaseTime(filter.From), toDatabaseTime(filter.To)}
	if filter.CarID != "" {
		where += " AND p.car_id = ?"
		args = append(args, filter.CarID)
	}
	if filter.UserID != "" {
		where += " AND p.user_id = ?"
		args = append(args, filter.UserID)
	}
	if filter.AssignmentID != "" {
		where += " AND EXISTS (SELECT 1 FROM usage_events e WHERE e.request_id=p.request_id AND e.assignment_id=?)"
		args = append(args, filter.AssignmentID)
	}
	if filter.APIKeyID != "" {
		where += " AND p.api_key_id = ?"
		args = append(args, filter.APIKeyID)
	}
	if filter.RequestedModel != "" {
		where += " AND (p.requested_model = ? OR EXISTS (SELECT 1 FROM usage_events e WHERE e.request_id=p.request_id AND e.model=?))"
		args = append(args, filter.RequestedModel, filter.RequestedModel)
	}
	if filter.Outcome != "" {
		where += " AND p.outcome = ?"
		args = append(args, filter.Outcome)
	}
	if filter.BillingStatus != "" {
		where += " AND p.billing_status = ?"
		args = append(args, filter.BillingStatus)
	}
	if filter.RequestID != "" {
		where += " AND p.request_id = ?"
		args = append(args, filter.RequestID)
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_requests p WHERE "+where, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("sqlite store: count usage requests: %w", classifyError(err))
	}
	return count, nil
}

func (s *Store) GetUsageRequestDetail(ctx context.Context, requestID string) (domain.UsageRequestDetail, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.UsageRequestDetail{}, errReady
	}
	if strings.TrimSpace(requestID) == "" {
		return domain.UsageRequestDetail{}, fmt.Errorf("sqlite store: request ID is required: %w", domain.ErrInvalid)
	}
	var item domain.UsageRequestDetail
	row := s.db.QueryRowContext(ctx, `SELECT p.request_id, p.user_id, p.api_key_id, p.car_id, p.membership_id,
		p.member_ref_snapshot, p.display_name_snapshot, p.scope_hash, p.scope_size, p.source_format,
		p.requested_model, p.stream, p.started_at, p.completed_at, p.outcome, p.status_class,
		p.reason_code, p.upstream_attempted, p.billing_period_id, p.pricing_catalog_hash,
		p.pricing_coverage_from, p.billing_status, p.billed_nano_usd, COALESCE(u.user_ref,''), COALESCE(c.car_ref,''),
		(SELECT COUNT(*) FROM usage_events e WHERE e.request_id=p.request_id),
		(SELECT COUNT(*) FROM usage_events e WHERE e.request_id=p.request_id AND e.pricing_status='unknown')
		FROM proxy_requests p LEFT JOIN users u ON u.id=p.user_id LEFT JOIN cars c ON c.id=p.car_id WHERE p.request_id=?`, requestID)
	var err error
	item, err = scanUsageRequestDetail(row)
	if err != nil {
		return domain.UsageRequestDetail{}, scanError("get usage request detail", err)
	}
	events, errEvents := s.listUsageEvents(ctx, requestID)
	if errEvents != nil {
		return domain.UsageRequestDetail{}, errEvents
	}
	item.Events = events
	return item, nil
}

type scanner interface{ Scan(...any) error }

func scanUsageRequestDetail(row scanner) (domain.UsageRequestDetail, error) {
	var item domain.UsageRequestDetail
	var r domain.ProxyRequest
	var carID, membershipID, memberRef, displayName, scopeHash, billingPeriodID, catalogHash, billingStatus sql.NullString
	var started int64
	var completed, coverage, billed sql.NullInt64
	var userRef, carRef string
	if err := row.Scan(&r.RequestID, &r.UserID, &r.APIKeyID, &carID, &membershipID, &memberRef, &displayName, &scopeHash, &r.ScopeSize, &r.SourceFormat, &r.RequestedModel, &r.Stream, &started, &completed, &r.Outcome, &r.StatusClass, &r.ReasonCode, &r.UpstreamAttempted, &billingPeriodID, &catalogHash, &coverage, &billingStatus, &billed, &userRef, &carRef, &item.EventCount, &item.UnknownEvents); err != nil {
		return item, err
	}
	r.CarID, r.MembershipID, r.MemberRefSnapshot, r.DisplayNameSnapshot, r.ScopeHash = carID.String, membershipID.String, memberRef.String, displayName.String, scopeHash.String
	r.BillingPeriodID, r.PricingCatalogHash, r.BillingStatus = billingPeriodID.String, catalogHash.String, billingStatus.String
	r.PricingCoverageFrom, r.BilledNanoUSD = fromNullableDatabaseTime(coverage), fromNullableInt64(billed)
	r.StartedAt, r.CompletedAt = fromDatabaseTime(started), fromNullableDatabaseTime(completed)
	item.Request, item.UserRef, item.CarRef, item.APIKeyRef = r, userRef, carRef, r.APIKeyID
	return item, nil
}

func (s *Store) listUsageEvents(ctx context.Context, requestID string) ([]domain.UsageEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, request_id, billing_period_id, event_seq, attempt_no,
		auth_id, assignment_id, account_ref_snapshot, safe_label_snapshot, provider, model, usage_known,
		input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens, failed, status_class,
		requested_at, recorded_at, canonical_schema, canonical_quality, uncached_input_tokens,
		cache_read_tokens, cache_write_tokens, non_reasoning_tokens, request_service_tier,
		response_service_tier, pricing_status, pricing_reason, price_input_per_token, price_output_per_token,
		price_cache_read, price_cache_write, cost_nano_usd FROM usage_events WHERE request_id=? ORDER BY requested_at, event_id`, requestID)
	if err != nil {
		return nil, fmt.Errorf("sqlite store: list usage events: %w", classifyError(err))
	}
	defer rows.Close()
	var result []domain.UsageEvent
	for rows.Next() {
		event, e := scanUsageEvent(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func scanUsageEvent(row scanner) (domain.UsageEvent, error) {
	var e domain.UsageEvent
	var period, account, label, provider, model, status, quality, tierReq, tierResp, pricing, reason, pin, pout, pcr, pcw sql.NullString
	var seq, attempt, in, out, cached, reasoning, total, requested, recorded, uncached, read, write, nonreason, cost sql.NullInt64
	if err := row.Scan(&e.EventID, &e.RequestID, &period, &seq, &attempt, &e.AuthID, &e.AssignmentID, &account, &label, &provider, &model, &e.UsageKnown, &in, &out, &cached, &reasoning, &total, &e.Failed, &status, &requested, &recorded, &e.CanonicalSchema, &quality, &uncached, &read, &write, &nonreason, &tierReq, &tierResp, &pricing, &reason, &pin, &pout, &pcr, &pcw, &cost); err != nil {
		return e, err
	}
	e.BillingPeriodID, e.AccountRefSnapshot, e.SafeLabelSnapshot, e.Provider, e.Model = period.String, account.String, label.String, provider.String, model.String
	e.EventSeq, e.AttemptNo, e.InputTokens, e.OutputTokens, e.CachedTokens, e.ReasoningTokens, e.TotalTokens = fromNullableInt64(seq), fromNullableInt64(attempt), fromNullableInt64(in), fromNullableInt64(out), fromNullableInt64(cached), fromNullableInt64(reasoning), fromNullableInt64(total)
	e.StatusClass, e.RequestedAt, e.RecordedAt, e.CanonicalQuality = status.String, fromDatabaseTime(requested.Int64), fromDatabaseTime(recorded.Int64), quality.String
	e.UncachedInputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.NonReasoningTokens = fromNullableInt64(uncached), fromNullableInt64(read), fromNullableInt64(write), fromNullableInt64(nonreason)
	e.RequestServiceTier, e.ResponseServiceTier, e.PricingStatus, e.PricingReason = tierReq.String, tierResp.String, pricing.String, reason.String
	e.PriceInputPerToken, e.PriceOutputPerToken, e.PriceCacheRead, e.PriceCacheWrite, e.CostNanoUSD = pin.String, pout.String, pcr.String, pcw.String, fromNullableInt64(cost)
	return e, nil
}
