package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func (s *Store) GetMemberQuotaLimits(ctx context.Context, membershipID string) (domain.MemberQuotaLimits, error) {
	if err := s.ready(); err != nil {
		return domain.MemberQuotaLimits{}, err
	}
	return memberQuotaLimits(ctx, s.db, membershipID)
}

type quotaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func memberQuotaLimits(ctx context.Context, q quotaQuerier, membershipID string) (domain.MemberQuotaLimits, error) {
	var month, five, week, concurrent sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT m.monthly_limit_nano_usd, m.five_hour_limit_nano_usd, m.weekly_limit_nano_usd, u.concurrency_limit FROM memberships m LEFT JOIN user_concurrency_limits u ON u.user_id=m.user_id WHERE m.id=?`, membershipID).Scan(&month, &five, &week, &concurrent)
	if err != nil {
		return domain.MemberQuotaLimits{}, scanError("read member quota limits", err)
	}
	return domain.MemberQuotaLimits{MonthlyNanoUSD: fromNullableInt64(month), FiveHourNanoUSD: fromNullableInt64(five), WeeklyNanoUSD: fromNullableInt64(week), UserConcurrency: quotaConcurrencyPointer(concurrent)}, nil
}

func quotaConcurrencyPointer(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func (s *Store) SetMemberLimits(ctx context.Context, membershipID string, update domain.MemberLimitsUpdate, audit *domain.AuditEvent) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(membershipID) == "" || !(update.MonthlySet || update.FiveHourSet || update.WeeklySet || update.UserConcurrencySet) {
		return domain.ErrInvalid
	}
	for _, v := range []struct {
		set   bool
		value *int64
	}{{update.MonthlySet, update.MonthlyNanoUSD}, {update.FiveHourSet, update.FiveHourNanoUSD}, {update.WeeklySet, update.WeeklyNanoUSD}} {
		if v.set && v.value != nil && *v.value < 0 {
			return domain.ErrInvalid
		}
	}
	if update.UserConcurrencySet && update.UserConcurrency != nil && *update.UserConcurrency <= 0 {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(err)
	}
	var userID string
	if err = tx.QueryRowContext(ctx, `SELECT user_id FROM memberships WHERE id=? AND ended_at IS NULL`, membershipID).Scan(&userID); err != nil {
		return rollback(tx, scanError("read limit membership", err))
	}
	for _, v := range []struct {
		set    bool
		column string
		value  *int64
	}{{update.MonthlySet, "monthly_limit_nano_usd", update.MonthlyNanoUSD}, {update.FiveHourSet, "five_hour_limit_nano_usd", update.FiveHourNanoUSD}, {update.WeeklySet, "weekly_limit_nano_usd", update.WeeklyNanoUSD}} {
		if v.set {
			if _, err = tx.ExecContext(ctx, `UPDATE memberships SET `+v.column+`=? WHERE id=?`, nullableInt64(v.value), membershipID); err != nil {
				return rollback(tx, classifyError(err))
			}
		}
	}
	now := s.currentTime()
	if update.MonthlySet {
		if _, err = tx.ExecContext(ctx, `UPDATE billing_periods SET limit_nano_usd=?, revision=revision+1 WHERE membership_id=? AND period_from<=? AND period_to>?`, nullableInt64(update.MonthlyNanoUSD), membershipID, toDatabaseTime(now), toDatabaseTime(now)); err != nil {
			return rollback(tx, classifyError(err))
		}
	}
	if update.UserConcurrencySet {
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_concurrency_limits(user_id,concurrency_limit) VALUES (?,?) ON CONFLICT(user_id) DO UPDATE SET concurrency_limit=excluded.concurrency_limit`, userID, update.UserConcurrency); err != nil {
			return rollback(tx, classifyError(err))
		}
	}
	if err = insertAudit(ctx, tx, audit, now); err != nil {
		return rollback(tx, err)
	}
	return classifyError(tx.Commit())
}

func (s *Store) GetUserConcurrency(ctx context.Context, userID string) (*int, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var value sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT l.concurrency_limit FROM users u LEFT JOIN user_concurrency_limits l ON l.user_id=u.id WHERE u.id=?`, userID).Scan(&value)
	if err != nil {
		return nil, scanError("read user concurrency", err)
	}
	return quotaConcurrencyPointer(value), nil
}

func (s *Store) GetAccountConcurrency(ctx context.Context, authID string) (*int, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var value sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT concurrency_limit FROM account_concurrency_limits WHERE auth_id=?`, authID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, scanError("read account concurrency", err)
	}
	return quotaConcurrencyPointer(value), nil
}

func (s *Store) SetAccountConcurrency(ctx context.Context, authID string, limit *int, audit *domain.AuditEvent) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(authID) == "" || (limit != nil && *limit <= 0) {
		return fmt.Errorf("sqlite store: invalid account concurrency: %w", domain.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO account_concurrency_limits(auth_id,concurrency_limit) VALUES (?,?) ON CONFLICT(auth_id) DO UPDATE SET concurrency_limit=excluded.concurrency_limit`, authID, limit); err != nil {
		return rollback(tx, classifyError(err))
	}
	if err = insertAudit(ctx, tx, audit, s.currentTime()); err != nil {
		return rollback(tx, err)
	}
	return classifyError(tx.Commit())
}
