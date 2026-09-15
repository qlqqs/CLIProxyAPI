package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

// SetMonthlyLimit changes the current member limit and the current period limit
// atomically. A nil limit leaves the member in the legacy, not-configured state.
func (s *Store) SetMonthlyLimit(ctx context.Context, membershipID string, limit *int64, audit *domain.AuditEvent) (domain.Membership, error) {
	if err := s.SetMemberLimits(ctx, membershipID, domain.MemberLimitsUpdate{MonthlySet: true, MonthlyNanoUSD: limit}, audit); err != nil {
		return domain.Membership{}, err
	}
	return s.membershipByID(ctx, membershipID)
}

func (s *Store) membershipByID(ctx context.Context, id string) (domain.Membership, error) {
	return scanMembership(s.db.QueryRowContext(ctx, `SELECT id, member_ref, user_id, car_id, display_name, display_name_key, started_at, ended_at, ended_reason, created_by_user_id, monthly_limit_nano_usd, billing_timezone, billing_anchor_at FROM memberships WHERE id = ?`, id))
}

// EnsureBillingPeriod creates a period once and returns the stored immutable facts.
func (s *Store) EnsureBillingPeriod(ctx context.Context, period domain.BillingPeriod) (domain.BillingPeriod, error) {
	if err := s.ready(); err != nil {
		return domain.BillingPeriod{}, err
	}
	if period.MembershipID == "" || period.CarID == "" || period.From.IsZero() || period.To.IsZero() || !period.To.After(period.From) {
		return domain.BillingPeriod{}, fmt.Errorf("sqlite store: invalid billing period: %w", domain.ErrInvalid)
	}
	if period.ID == "" {
		period.ID = uuid.NewString()
	}
	if period.Revision == 0 {
		period.Revision = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO billing_periods (id, membership_id, member_ref_snapshot, car_id, timezone, anchor_at, period_from, period_to, limit_nano_usd, confirmed_nano_usd, reset_baseline_nano_usd, unknown_cost_events, revision) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, period.ID, period.MembershipID, period.MemberRefSnapshot, period.CarID, period.Timezone, toDatabaseTime(period.AnchorAt), toDatabaseTime(period.From), toDatabaseTime(period.To), nullableInt64(period.LimitNanoUSD), period.ConfirmedNanoUSD, period.ResetBaselineNanoUSD, period.UnknownCostEvents, period.Revision)
	if err != nil {
		return domain.BillingPeriod{}, fmt.Errorf("sqlite store: create billing period: %w", classifyError(err))
	}
	return s.GetBillingPeriod(ctx, period.MembershipID, period.From)
}

// GetBillingPeriod reads the period containing at for a membership.
func (s *Store) GetBillingPeriod(ctx context.Context, membershipID string, at time.Time) (domain.BillingPeriod, error) {
	if err := s.ready(); err != nil {
		return domain.BillingPeriod{}, err
	}
	var p domain.BillingPeriod
	var limit, anchor sql.NullInt64
	var from, to int64
	err := s.db.QueryRowContext(ctx, `SELECT id, membership_id, member_ref_snapshot, car_id, timezone, anchor_at, period_from, period_to, limit_nano_usd, confirmed_nano_usd, reset_baseline_nano_usd, unknown_cost_events, revision FROM billing_periods WHERE membership_id = ? AND period_from <= ? AND period_to > ? ORDER BY period_from DESC LIMIT 1`, membershipID, toDatabaseTime(at), toDatabaseTime(at)).Scan(&p.ID, &p.MembershipID, &p.MemberRefSnapshot, &p.CarID, &p.Timezone, &anchor, &from, &to, &limit, &p.ConfirmedNanoUSD, &p.ResetBaselineNanoUSD, &p.UnknownCostEvents, &p.Revision)
	if err != nil {
		return domain.BillingPeriod{}, scanError("get billing period", err)
	}
	p.AnchorAt = fromDatabaseTime(anchor.Int64)
	p.From = fromDatabaseTime(from)
	p.To = fromDatabaseTime(to)
	p.LimitNanoUSD = fromNullableInt64(limit)
	return p, nil
}

// RecordUsageEventBilled inserts an event and updates its period in one transaction.
// Duplicate event IDs are idempotent and do not update the cumulative amount.
func (s *Store) RecordUsageEventBilled(ctx context.Context, event domain.UsageEvent, periodID string, cost *int64, pricingStatus, pricingReason string) (domain.UsageEvent, error) {
	if err := s.ready(); err != nil {
		return domain.UsageEvent{}, err
	}
	if event.EventID == "" || event.RequestID == "" || event.AuthID == "" {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: incomplete billed usage event: %w", domain.ErrInvalid)
	}
	if !event.UsageKnown && cost != nil {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: unknown usage cannot have cost: %w", domain.ErrInvalid)
	}
	if cost != nil && *cost < 0 {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: negative event cost: %w", domain.ErrInvalid)
	}

	if pricingStatus == "" {
		if cost == nil {
			pricingStatus = "unknown"
		} else {
			pricingStatus = "priced"
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: begin billed usage: %w", classifyError(err))
	}
	// Receipts outlive request details, so replay after retention remains a no-op.
	var receiptRequest, receiptStatus, receiptReason string
	var receiptCost sql.NullInt64
	errReceipt := tx.QueryRowContext(ctx, `SELECT request_id,cost_nano_usd,pricing_status,pricing_reason FROM billed_event_receipts WHERE event_id=?`, event.EventID).Scan(&receiptRequest, &receiptCost, &receiptStatus, &receiptReason)
	if errReceipt == nil {
		if receiptRequest != event.RequestID {
			return domain.UsageEvent{}, rollback(tx, domain.ErrConflict)
		}
		event.CostNanoUSD, event.PricingStatus, event.PricingReason = fromNullableInt64(receiptCost), receiptStatus, receiptReason
		event.BillingPeriodID = periodID
		if errCommit := tx.Commit(); errCommit != nil {
			return domain.UsageEvent{}, classifyError(errCommit)
		}
		return event, nil
	}
	if !errors.Is(errReceipt, sql.ErrNoRows) {
		return domain.UsageEvent{}, rollback(tx, scanError("read billed receipt", errReceipt))
	}
	if strings.TrimSpace(periodID) == "" {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(billing_period_id, '') FROM proxy_requests WHERE request_id = ?`, event.RequestID).Scan(&periodID); err != nil {
			return domain.UsageEvent{}, rollback(tx, scanError("read request billing period", err))
		}
		if strings.TrimSpace(periodID) == "" {
			return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: request has no billing period: %w", domain.ErrInvalid))
		}
	}
	if err = populateUsageScope(ctx, tx, &event); err != nil {
		return domain.UsageEvent{}, rollback(tx, err)
	}
	if event.RequestedAt.IsZero() {
		event.RequestedAt = s.currentTime()
	}
	if event.RecordedAt.IsZero() {
		event.RecordedAt = s.currentTime()
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_events (event_id, request_id, billing_period_id, event_seq, attempt_no, auth_id, assignment_id, account_ref_snapshot, safe_label_snapshot, provider, model, usage_known, input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens, failed, status_class, requested_at, recorded_at, canonical_schema, canonical_quality, uncached_input_tokens, cache_read_tokens, cache_write_tokens, non_reasoning_tokens, request_service_tier, response_service_tier, pricing_status, pricing_reason, price_input_per_token, price_output_per_token, price_cache_read, price_cache_write, cost_nano_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.EventID, event.RequestID, nullableString(periodID), nullableInt64(event.EventSeq), nullableInt64(event.AttemptNo), event.AuthID, event.AssignmentID, event.AccountRefSnapshot, event.SafeLabelSnapshot, event.Provider, event.Model, boolToInt(event.UsageKnown), nullableInt64(event.InputTokens), nullableInt64(event.OutputTokens), nullableInt64(event.CachedTokens), nullableInt64(event.ReasoningTokens), nullableInt64(event.TotalTokens), boolToInt(event.Failed), event.StatusClass, toDatabaseTime(event.RequestedAt), toDatabaseTime(event.RecordedAt), event.CanonicalSchema, event.CanonicalQuality, nullableInt64(event.UncachedInputTokens), nullableInt64(event.CacheReadTokens), nullableInt64(event.CacheWriteTokens), nullableInt64(event.NonReasoningTokens), event.RequestServiceTier, event.ResponseServiceTier, pricingStatus, pricingReason, event.PriceInputPerToken, event.PriceOutputPerToken, event.PriceCacheRead, event.PriceCacheWrite, nullableInt64(cost))
	if err != nil {
		return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: insert billed usage: %w", classifyError(err)))
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return domain.UsageEvent{}, rollback(tx, err)
	}
	if affected == 1 {
		if errQuota := recordQuotaFeeTx(ctx, tx, event, cost, pricingStatus, pricingReason); errQuota != nil {
			return domain.UsageEvent{}, rollback(tx, errQuota)
		}
		var query string
		var args []any
		if cost == nil {
			query = `UPDATE billing_periods SET unknown_cost_events = unknown_cost_events + 1, revision = revision + 1 WHERE id = ?`
			args = []any{periodID}
		} else {
			query = `UPDATE billing_periods SET confirmed_nano_usd = confirmed_nano_usd + ?, revision = revision + 1 WHERE id = ?`
			args = []any{*cost, periodID}
		}
		upd, e := tx.ExecContext(ctx, query, args...)
		if e != nil {
			return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: update billed period: %w", classifyError(e)))
		}
		n, e := upd.RowsAffected()
		if e != nil || n != 1 {
			if e == nil {
				e = domain.ErrNotFound
			}
			return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: billing period missing: %w", e))
		}
		if cost == nil {
			_, e = tx.ExecContext(ctx, `UPDATE proxy_requests SET billing_status = 'unknown' WHERE request_id = ?`, event.RequestID)
		} else {
			_, e = tx.ExecContext(ctx, `UPDATE proxy_requests
				SET billing_status = CASE WHEN billing_status = 'unknown' OR EXISTS (
					SELECT 1 FROM usage_events WHERE request_id = ? AND pricing_status = 'unknown'
				) THEN 'unknown' ELSE 'priced' END,
				billed_nano_usd = COALESCE(billed_nano_usd, 0) + ?
				WHERE request_id = ?`, event.RequestID, *cost, event.RequestID)
		}
		if e != nil {
			return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: update request billing: %w", classifyError(e)))
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: commit billed usage: %w", classifyError(err))
	}
	event.BillingPeriodID = periodID
	event.CostNanoUSD = copyNullableInt64(cost)
	event.PricingStatus = pricingStatus
	event.PricingReason = pricingReason
	return event, nil
}

func populateUsageScope(ctx context.Context, tx *sql.Tx, event *domain.UsageEvent) error {
	err := tx.QueryRowContext(ctx, `SELECT assignment_id, account_ref_snapshot, safe_label_snapshot, provider_snapshot FROM proxy_request_auth_scopes WHERE request_id = ? AND auth_id = ?`, event.RequestID, event.AuthID).Scan(&event.AssignmentID, &event.AccountRefSnapshot, &event.SafeLabelSnapshot, &event.Provider)
	if err != nil {
		return scanError("read billed usage request scope", err)
	}
	return nil
}

// ListBillingPeriods returns periods ordered newest first.
func (s *Store) ListBillingPeriods(ctx context.Context, membershipID, carID string, from, to time.Time, limit int) (periods []domain.BillingPeriod, err error) {
	if err = s.ready(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		return nil, fmt.Errorf("sqlite store: invalid billing period limit: %w", domain.ErrInvalid)
	}
	fromValue, toValue := int64(0), int64(0)
	if !from.IsZero() {
		fromValue = toDatabaseTime(from)
	}
	if !to.IsZero() {
		toValue = toDatabaseTime(to)
	}
	rows, e := s.db.QueryContext(ctx, `SELECT id, membership_id, member_ref_snapshot, car_id, timezone, anchor_at, period_from, period_to, limit_nano_usd, confirmed_nano_usd, reset_baseline_nano_usd, unknown_cost_events, revision FROM billing_periods WHERE (? = '' OR membership_id = ?) AND (? = '' OR car_id = ?) AND (? = 0 OR period_to > ?) AND (? = 0 OR period_from < ?) ORDER BY period_from DESC, id DESC LIMIT ?`, membershipID, membershipID, carID, carID, fromValue, fromValue, toValue, toValue, limit)
	if e != nil {
		return nil, e
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var p domain.BillingPeriod
		var anchor sql.NullInt64
		var f, t int64
		var l sql.NullInt64
		if e := rows.Scan(&p.ID, &p.MembershipID, &p.MemberRefSnapshot, &p.CarID, &p.Timezone, &anchor, &f, &t, &l, &p.ConfirmedNanoUSD, &p.ResetBaselineNanoUSD, &p.UnknownCostEvents, &p.Revision); e != nil {
			return nil, e
		}
		p.AnchorAt = fromDatabaseTime(anchor.Int64)
		p.From = fromDatabaseTime(f)
		p.To = fromDatabaseTime(t)
		p.LimitNanoUSD = fromNullableInt64(l)
		periods = append(periods, p)
	}
	return periods, rows.Err()
}
