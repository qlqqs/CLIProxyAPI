package sqlite

import (
	"bytes"
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

func (s *Store) BootstrapAdmin(ctx context.Context, user domain.User, audit *domain.AuditEvent) (domain.User, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.User{}, errReady
	}
	prepared, errPrepare := s.prepareUser(user)
	if errPrepare != nil {
		return domain.User{}, errPrepare
	}
	if prepared.Role != domain.UserRoleAdmin {
		return domain.User{}, fmt.Errorf("sqlite store: bootstrap user must be an administrator: %w", domain.ErrInvalid)
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.User{}, fmt.Errorf("sqlite store: begin administrator bootstrap: %w", classifyError(errBegin))
	}
	result, errInsert := tx.ExecContext(ctx, `
		INSERT INTO users (
			id, user_ref, username, username_normalized, default_display_name, role, status,
			password_hash, password_version, must_change_password, disabled_at,
			created_at, updated_at
		)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM users WHERE role = 'carpool_admin')
	`, userValues(prepared)...)
	if errInsert != nil {
		return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: bootstrap administrator: %w", classifyError(errInsert)))
	}
	rowsAffected, errAffected := result.RowsAffected()
	if errAffected != nil {
		return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: inspect administrator bootstrap: %w", errAffected))
	}
	if rowsAffected != 1 {
		return domain.User{}, rollback(tx, domain.ErrAlreadyBootstrapped)
	}
	if errAudit := insertAudit(ctx, tx, audit, s.currentTime()); errAudit != nil {
		return domain.User{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.User{}, fmt.Errorf("sqlite store: commit administrator bootstrap: %w", classifyError(errCommit))
	}
	return prepared, nil
}

func (s *Store) CreateUser(ctx context.Context, user domain.User, audits ...*domain.AuditEvent) (domain.User, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.User{}, errReady
	}
	prepared, errPrepare := s.prepareUser(user)
	if errPrepare != nil {
		return domain.User{}, errPrepare
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.User{}, fmt.Errorf("sqlite store: begin user creation: %w", classifyError(errBegin))
	}
	if _, errExec := tx.ExecContext(ctx, `
		INSERT INTO users (
			id, user_ref, username, username_normalized, default_display_name, role, status,
			password_hash, password_version, must_change_password, disabled_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, userValues(prepared)...); errExec != nil {
		return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: create user: %w", classifyError(errExec)))
	}
	if errAudit := insertAudits(ctx, tx, prepared.CreatedAt, audits...); errAudit != nil {
		return domain.User{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.User{}, fmt.Errorf("sqlite store: commit user creation: %w", classifyError(errCommit))
	}
	return prepared, nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (domain.User, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.User{}, errReady
	}
	return scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, user_ref, username, username_normalized, default_display_name, role, status,
		       password_hash, password_version, must_change_password, disabled_at,
		       created_at, updated_at
		FROM users WHERE id = ?
	`, userID))
}

func (s *Store) CreateSession(ctx context.Context, session domain.Session, audits ...*domain.AuditEvent) (domain.Session, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Session{}, errReady
	}
	now := s.currentTime()
	if session.ID == "" {
		session.ID = uuid.NewString()
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.LastSeenAt.IsZero() {
		session.LastSeenAt = session.CreatedAt
	}
	if session.UserID == "" || len(session.TokenDigest) == 0 || session.CSRFToken == "" || session.PasswordVersion < 1 || session.ExpiresAt.IsZero() {
		return domain.Session{}, fmt.Errorf("sqlite store: incomplete session: %w", domain.ErrInvalid)
	}
	if !session.ExpiresAt.After(session.CreatedAt) {
		return domain.Session{}, fmt.Errorf("sqlite store: session expiry must follow creation: %w", domain.ErrInvalid)
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.Session{}, fmt.Errorf("sqlite store: begin session creation: %w", classifyError(errBegin))
	}
	result, errExec := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, user_id, token_digest, csrf_token, password_version, created_at,
			last_seen_at, expires_at, revoked_at, revoke_reason
		)
		SELECT ?, id, ?, ?, ?, ?, ?, ?, ?, ?
		FROM users
		WHERE id = ? AND status = 'active' AND password_version = ?
	`, session.ID, append([]byte(nil), session.TokenDigest...), session.CSRFToken,
		session.PasswordVersion, toDatabaseTime(session.CreatedAt), toDatabaseTime(session.LastSeenAt),
		toDatabaseTime(session.ExpiresAt), nullableDatabaseTime(session.RevokedAt), session.RevokeReason,
		session.UserID, session.PasswordVersion)
	if errExec != nil {
		return domain.Session{}, rollback(tx, fmt.Errorf("sqlite store: create session: %w", classifyError(errExec)))
	}
	rows, errRows := affectedRows("create session", result)
	if errRows != nil {
		return domain.Session{}, rollback(tx, errRows)
	}
	if rows != 1 {
		return domain.Session{}, rollback(tx, fmt.Errorf("sqlite store: session user is inactive or changed: %w", domain.ErrConflict))
	}
	if errAudit := insertAudits(ctx, tx, session.CreatedAt, audits...); errAudit != nil {
		return domain.Session{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.Session{}, fmt.Errorf("sqlite store: commit session creation: %w", classifyError(errCommit))
	}
	return session, nil
}

func (s *Store) GetSessionByDigest(ctx context.Context, digest []byte) (domain.Session, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Session{}, errReady
	}
	var session domain.Session
	var createdAt, lastSeenAt, expiresAt int64
	var revokedAt sql.NullInt64
	errScan := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, token_digest, csrf_token, password_version, created_at,
		       last_seen_at, expires_at, revoked_at, revoke_reason
		FROM sessions WHERE token_digest = ?
	`, digest).Scan(&session.ID, &session.UserID, &session.TokenDigest, &session.CSRFToken,
		&session.PasswordVersion, &createdAt, &lastSeenAt, &expiresAt, &revokedAt, &session.RevokeReason)
	if errScan != nil {
		return domain.Session{}, scanError("get session", errScan)
	}
	session.CreatedAt = fromDatabaseTime(createdAt)
	session.LastSeenAt = fromDatabaseTime(lastSeenAt)
	session.ExpiresAt = fromDatabaseTime(expiresAt)
	session.RevokedAt = fromNullableDatabaseTime(revokedAt)
	return session, nil
}

func (s *Store) RevokeSessionsForUser(ctx context.Context, userID, reason string, revokedAt time.Time) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	if revokedAt.IsZero() {
		revokedAt = s.currentTime()
	}
	result, errExec := s.db.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL
	`, toDatabaseTime(revokedAt), reason, userID)
	if errExec != nil {
		return 0, fmt.Errorf("sqlite store: revoke user sessions: %w", classifyError(errExec))
	}
	return affectedRows("revoke user sessions", result)
}

func (s *Store) CreateAPIKey(ctx context.Context, key domain.APIKey, audits ...*domain.AuditEvent) (domain.APIKey, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.APIKey{}, errReady
	}
	if key.KeyID == "" {
		key.KeyID = uuid.NewString()
	}
	if key.CreatedAt.IsZero() {
		key.CreatedAt = s.currentTime()
	}
	if key.UserID == "" || strings.TrimSpace(key.Name) == "" || len(key.SecretDigest) == 0 {
		return domain.APIKey{}, fmt.Errorf("sqlite store: incomplete API key: %w", domain.ErrInvalid)
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(key.CreatedAt) {
		return domain.APIKey{}, fmt.Errorf("sqlite store: API key expiry must follow creation: %w", domain.ErrInvalid)
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.APIKey{}, fmt.Errorf("sqlite store: begin API key creation: %w", classifyError(errBegin))
	}
	var role domain.UserRole
	var status domain.UserStatus
	if errUser := tx.QueryRowContext(ctx, "SELECT role, status FROM users WHERE id = ?", key.UserID).Scan(&role, &status); errUser != nil {
		return domain.APIKey{}, rollback(tx, scanError("read API key user", errUser))
	}
	if role != domain.UserRolePassenger || status != domain.UserStatusActive {
		return domain.APIKey{}, rollback(tx, fmt.Errorf("sqlite store: API keys require an active passenger: %w", domain.ErrConflict))
	}
	if _, errExec := tx.ExecContext(ctx, `
		INSERT INTO user_api_keys (
			key_id, token, user_id, name, secret_digest, created_at, expires_at,
			last_used_at, revoked_at, revoke_reason
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, key.KeyID, key.Token, key.UserID, strings.TrimSpace(key.Name), append([]byte(nil), key.SecretDigest...),
		toDatabaseTime(key.CreatedAt), nullableDatabaseTime(key.ExpiresAt), nullableDatabaseTime(key.LastUsedAt),
		nullableDatabaseTime(key.RevokedAt), key.RevokeReason); errExec != nil {
		return domain.APIKey{}, rollback(tx, fmt.Errorf("sqlite store: create API key: %w", classifyError(errExec)))
	}
	if errAudit := insertAudits(ctx, tx, key.CreatedAt, audits...); errAudit != nil {
		return domain.APIKey{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.APIKey{}, fmt.Errorf("sqlite store: commit API key creation: %w", classifyError(errCommit))
	}
	return key, nil
}

func (s *Store) GetAPIKey(ctx context.Context, keyID string) (domain.APIKey, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.APIKey{}, errReady
	}
	return scanAPIKey(s.db.QueryRowContext(ctx, `
		SELECT key_id, token, user_id, name, secret_digest, created_at, expires_at,
		       last_used_at, revoked_at, revoke_reason
		FROM user_api_keys WHERE key_id = ?
	`, keyID))
}

func (s *Store) RevokeAPIKey(ctx context.Context, keyID, reason string, revokedAt time.Time, audits ...*domain.AuditEvent) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	if revokedAt.IsZero() {
		revokedAt = s.currentTime()
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("sqlite store: begin API key revocation: %w", classifyError(errBegin))
	}
	result, errExec := tx.ExecContext(ctx, `
		UPDATE user_api_keys SET revoked_at = ?, revoke_reason = ?
		WHERE key_id = ? AND revoked_at IS NULL
	`, toDatabaseTime(revokedAt), reason, keyID)
	if errExec != nil {
		return rollback(tx, fmt.Errorf("sqlite store: revoke API key: %w", classifyError(errExec)))
	}
	rows, errRows := affectedRows("revoke API key", result)
	if errRows != nil {
		return rollback(tx, errRows)
	}
	if rows == 0 {
		var count int
		if errQuery := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_api_keys WHERE key_id = ?", keyID).Scan(&count); errQuery != nil {
			return rollback(tx, fmt.Errorf("sqlite store: revoke API key existence check: %w", classifyError(errQuery)))
		}
		if count == 0 {
			return rollback(tx, domain.ErrNotFound)
		}
	}
	if errAudit := insertAudits(ctx, tx, revokedAt, audits...); errAudit != nil {
		return rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("sqlite store: commit API key revocation: %w", classifyError(errCommit))
	}
	return nil
}

func (s *Store) CreateCar(ctx context.Context, car domain.Car, audits ...*domain.AuditEvent) (domain.Car, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Car{}, errReady
	}
	now := s.currentTime()
	if car.ID == "" {
		car.ID = uuid.NewString()
	}
	if car.CarRef == "" {
		car.CarRef = uuid.NewString()
	}
	car.Name = strings.TrimSpace(car.Name)
	if car.Name == "" {
		return domain.Car{}, fmt.Errorf("sqlite store: car name is required: %w", domain.ErrInvalid)
	}
	if car.Status == "" {
		car.Status = domain.CarStatusActive
	}
	if !car.Status.Valid() || (car.SeatLimit != nil && *car.SeatLimit <= 0) {
		return domain.Car{}, fmt.Errorf("sqlite store: invalid car: %w", domain.ErrInvalid)
	}
	car.Version = 1
	if car.CreatedAt.IsZero() {
		car.CreatedAt = now
	}
	if car.UpdatedAt.IsZero() {
		car.UpdatedAt = car.CreatedAt
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.Car{}, fmt.Errorf("sqlite store: begin car creation: %w", classifyError(errBegin))
	}
	if _, errExec := tx.ExecContext(ctx, `
		INSERT INTO cars (
			id, car_ref, name, description, seat_limit, status, version,
			disabled_at, retired_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, car.ID, car.CarRef, car.Name, car.Description, nullableInt(car.SeatLimit), car.Status,
		car.Version, nullableDatabaseTime(car.DisabledAt), nullableDatabaseTime(car.RetiredAt),
		toDatabaseTime(car.CreatedAt), toDatabaseTime(car.UpdatedAt)); errExec != nil {
		return domain.Car{}, rollback(tx, fmt.Errorf("sqlite store: create car: %w", classifyError(errExec)))
	}
	if errAudit := insertAudits(ctx, tx, car.CreatedAt, audits...); errAudit != nil {
		return domain.Car{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.Car{}, fmt.Errorf("sqlite store: commit car creation: %w", classifyError(errCommit))
	}
	return car, nil
}

func (s *Store) GetCar(ctx context.Context, carID string) (domain.Car, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Car{}, errReady
	}
	return scanCar(s.db.QueryRowContext(ctx, `
		SELECT id, car_ref, name, description, seat_limit, status, version,
		       disabled_at, retired_at, created_at, updated_at
		FROM cars WHERE id = ?
	`, carID))
}

func (s *Store) MoveMembership(ctx context.Context, move domain.MembershipMove) (domain.Membership, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Membership{}, errReady
	}
	membership := move.Membership
	now := s.currentTime()
	if membership.ID == "" {
		membership.ID = uuid.NewString()
	}
	if membership.MemberRef == "" {
		membership.MemberRef = uuid.NewString()
	}
	membership.DisplayName = strings.TrimSpace(membership.DisplayName)
	if membership.DisplayNameKey == "" {
		membership.DisplayNameKey = strings.ToLower(membership.DisplayName)
	}
	if membership.StartedAt.IsZero() {
		membership.StartedAt = now
	}
	if strings.TrimSpace(membership.BillingTimezone) == "" {
		membership.BillingTimezone = "UTC"
	}
	if membership.BillingAnchorAt.IsZero() {
		membership.BillingAnchorAt = membership.StartedAt
	}
	if membership.UserID == "" || membership.CarID == "" || membership.DisplayName == "" || membership.DisplayNameKey == "" || membership.CreatedByUserID == "" {
		return domain.Membership{}, fmt.Errorf("sqlite store: incomplete membership move: %w", domain.ErrInvalid)
	}
	if move.EndCurrentReason == "" {
		move.EndCurrentReason = "moved"
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.Membership{}, fmt.Errorf("sqlite store: begin membership move: %w", classifyError(errBegin))
	}
	var role domain.UserRole
	var userStatus domain.UserStatus
	if errUser := tx.QueryRowContext(ctx, "SELECT role, status FROM users WHERE id = ?", membership.UserID).Scan(&role, &userStatus); errUser != nil {
		return domain.Membership{}, rollback(tx, scanError("read membership user", errUser))
	}
	if role != domain.UserRolePassenger || userStatus != domain.UserStatusActive {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: membership requires an active passenger: %w", domain.ErrConflict))
	}
	var carStatus domain.CarStatus
	var seatLimit sql.NullInt64
	if errCar := tx.QueryRowContext(ctx, "SELECT status, seat_limit FROM cars WHERE id = ?", membership.CarID).Scan(&carStatus, &seatLimit); errCar != nil {
		return domain.Membership{}, rollback(tx, scanError("read membership car", errCar))
	}
	if carStatus != domain.CarStatusActive {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: target car is not active: %w", domain.ErrConflict))
	}
	var previousID, previousCarID string
	errCurrent := tx.QueryRowContext(ctx, `
		SELECT id, car_id FROM memberships WHERE user_id = ? AND ended_at IS NULL
	`, membership.UserID).Scan(&previousID, &previousCarID)
	if errCurrent != nil && !errors.Is(errCurrent, sql.ErrNoRows) {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: read current membership: %w", classifyError(errCurrent)))
	}
	if previousCarID == membership.CarID {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: passenger is already in target car: %w", domain.ErrConflict))
	}
	if previousID != move.ExpectedCurrentID {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: current membership changed: %w", domain.ErrConflict))
	}
	var occupied int64
	if errCount := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memberships WHERE car_id = ? AND ended_at IS NULL
	`, membership.CarID).Scan(&occupied); errCount != nil {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: count occupied seats: %w", classifyError(errCount)))
	}
	if seatLimit.Valid && occupied >= seatLimit.Int64 {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: target car has no available seats: %w", domain.ErrConflict))
	}
	if previousID != "" {
		if _, errEnd := tx.ExecContext(ctx, `
			UPDATE memberships SET ended_at = ?, ended_reason = ? WHERE id = ? AND ended_at IS NULL
		`, toDatabaseTime(membership.StartedAt), move.EndCurrentReason, previousID); errEnd != nil {
			return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: end current membership: %w", classifyError(errEnd)))
		}
	}
	if _, errInsert := tx.ExecContext(ctx, `
		INSERT INTO memberships (
		    id, member_ref, user_id, car_id, display_name, display_name_key,
		    started_at, ended_at, ended_reason, created_by_user_id,
		    monthly_limit_nano_usd, billing_timezone, billing_anchor_at,
		    five_hour_limit_nano_usd, weekly_limit_nano_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, '', ?, ?, ?, ?, ?, ?)
	`, membership.ID, membership.MemberRef, membership.UserID, membership.CarID,
		membership.DisplayName, membership.DisplayNameKey, toDatabaseTime(membership.StartedAt),
		membership.CreatedByUserID, nullableInt64(membership.MonthlyLimitNanoUSD), membership.BillingTimezone,
		toDatabaseTime(membership.BillingAnchorAt), quotaLimitValue(membership.FiveHourLimitNanoUSD),
		quotaLimitValue(membership.WeeklyLimitNanoUSD)); errInsert != nil {
		return domain.Membership{}, rollback(tx, fmt.Errorf("sqlite store: create membership: %w", classifyError(errInsert)))
	}
	if errVersions := bumpCarVersions(ctx, tx, now, membership.CarID, previousCarID); errVersions != nil {
		return domain.Membership{}, rollback(tx, errVersions)
	}
	if errAudit := insertAudit(ctx, tx, move.Audit, now); errAudit != nil {
		return domain.Membership{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.Membership{}, fmt.Errorf("sqlite store: commit membership move: %w", classifyError(errCommit))
	}
	return membership, nil
}

func (s *Store) CurrentMembership(ctx context.Context, userID string) (domain.Membership, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Membership{}, errReady
	}
	return scanMembership(s.db.QueryRowContext(ctx, `
		SELECT id, member_ref, user_id, car_id, display_name, display_name_key,
		       started_at, ended_at, ended_reason, created_by_user_id,
		       monthly_limit_nano_usd, billing_timezone, billing_anchor_at
		FROM memberships WHERE user_id = ? AND ended_at IS NULL
	`, userID))
}

func (s *Store) MoveAuthAssignment(ctx context.Context, move domain.AuthAssignmentMove) (domain.AuthAssignment, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.AuthAssignment{}, errReady
	}
	assignment := move.AuthAssignment
	now := s.currentTime()
	if assignment.ID == "" {
		assignment.ID = uuid.NewString()
	}
	if assignment.AccountRef == "" {
		assignment.AccountRef = uuid.NewString()
	}
	assignment.SafeLabel = strings.TrimSpace(assignment.SafeLabel)
	if assignment.SafeLabelKey == "" {
		assignment.SafeLabelKey = strings.ToLower(assignment.SafeLabel)
	}
	if assignment.StartedAt.IsZero() {
		assignment.StartedAt = now
	}
	if assignment.CarID == "" || assignment.AuthID == "" || assignment.SafeLabel == "" || assignment.SafeLabelKey == "" || assignment.ProviderSnapshot == "" || assignment.CreatedByUserID == "" {
		return domain.AuthAssignment{}, fmt.Errorf("sqlite store: incomplete auth assignment move: %w", domain.ErrInvalid)
	}
	if move.EndCurrentReason == "" {
		move.EndCurrentReason = "moved"
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.AuthAssignment{}, fmt.Errorf("sqlite store: begin auth assignment move: %w", classifyError(errBegin))
	}
	var carStatus domain.CarStatus
	if errCar := tx.QueryRowContext(ctx, "SELECT status FROM cars WHERE id = ?", assignment.CarID).Scan(&carStatus); errCar != nil {
		return domain.AuthAssignment{}, rollback(tx, scanError("read auth assignment car", errCar))
	}
	if carStatus != domain.CarStatusActive {
		return domain.AuthAssignment{}, rollback(tx, fmt.Errorf("sqlite store: target car is not active: %w", domain.ErrConflict))
	}
	var previousID, previousCarID string
	errCurrent := tx.QueryRowContext(ctx, `
		SELECT id, car_id FROM car_auth_assignments WHERE auth_id = ? AND ended_at IS NULL
	`, assignment.AuthID).Scan(&previousID, &previousCarID)
	if errCurrent != nil && !errors.Is(errCurrent, sql.ErrNoRows) {
		return domain.AuthAssignment{}, rollback(tx, fmt.Errorf("sqlite store: read current auth assignment: %w", classifyError(errCurrent)))
	}
	if previousCarID == assignment.CarID {
		return domain.AuthAssignment{}, rollback(tx, fmt.Errorf("sqlite store: auth is already assigned to target car: %w", domain.ErrConflict))
	}
	if previousID != move.ExpectedCurrentID {
		return domain.AuthAssignment{}, rollback(tx, fmt.Errorf("sqlite store: current auth assignment changed: %w", domain.ErrConflict))
	}
	if previousID != "" {
		if _, errEnd := tx.ExecContext(ctx, `
			UPDATE car_auth_assignments SET ended_at = ?, ended_reason = ?
			WHERE id = ? AND ended_at IS NULL
		`, toDatabaseTime(assignment.StartedAt), move.EndCurrentReason, previousID); errEnd != nil {
			return domain.AuthAssignment{}, rollback(tx, fmt.Errorf("sqlite store: end current auth assignment: %w", classifyError(errEnd)))
		}
	}
	if _, errInsert := tx.ExecContext(ctx, `
		INSERT INTO car_auth_assignments (
			id, account_ref, car_id, auth_id, safe_label, safe_label_key,
			provider_snapshot, started_at, ended_at, ended_reason, created_by_user_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?)
	`, assignment.ID, assignment.AccountRef, assignment.CarID, assignment.AuthID,
		assignment.SafeLabel, assignment.SafeLabelKey, assignment.ProviderSnapshot,
		toDatabaseTime(assignment.StartedAt), assignment.CreatedByUserID); errInsert != nil {
		return domain.AuthAssignment{}, rollback(tx, fmt.Errorf("sqlite store: create auth assignment: %w", classifyError(errInsert)))
	}
	if errVersions := bumpCarVersions(ctx, tx, now, assignment.CarID, previousCarID); errVersions != nil {
		return domain.AuthAssignment{}, rollback(tx, errVersions)
	}
	if errAudit := insertAudit(ctx, tx, move.Audit, now); errAudit != nil {
		return domain.AuthAssignment{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.AuthAssignment{}, fmt.Errorf("sqlite store: commit auth assignment move: %w", classifyError(errCommit))
	}
	return assignment, nil
}

func (s *Store) CurrentAuthAssignment(ctx context.Context, authID string) (domain.AuthAssignment, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.AuthAssignment{}, errReady
	}
	return scanAuthAssignment(s.db.QueryRowContext(ctx, `
		SELECT id, account_ref, car_id, auth_id, safe_label, safe_label_key,
		       provider_snapshot, started_at, ended_at, ended_reason, created_by_user_id
		FROM car_auth_assignments WHERE auth_id = ? AND ended_at IS NULL
	`, authID))
}

func (s *Store) BeginProxyRequest(ctx context.Context, request domain.ProxyRequest, scopes []domain.ProxyRequestAuthScope) (domain.ProxyRequest, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.ProxyRequest{}, errReady
	}
	if request.RequestID == "" {
		request.RequestID = uuid.NewString()
	}
	if request.StartedAt.IsZero() {
		request.StartedAt = s.currentTime()
	}
	if request.UserID == "" || request.APIKeyID == "" || request.SourceFormat == "" || !request.Outcome.Valid() {
		return domain.ProxyRequest{}, fmt.Errorf("sqlite store: incomplete proxy request: %w", domain.ErrInvalid)
	}
	switch request.Outcome {
	case domain.RequestOutcomeInProgress:
		if request.CompletedAt != nil || len(scopes) == 0 || request.ScopeSize != len(scopes) || request.CarID == "" || request.MembershipID == "" || request.ScopeHash == "" {
			return domain.ProxyRequest{}, fmt.Errorf("sqlite store: invalid authorized proxy request: %w", domain.ErrInvalid)
		}
	case domain.RequestOutcomeRejected:
		if len(scopes) != 0 || request.ScopeSize != 0 {
			return domain.ProxyRequest{}, fmt.Errorf("sqlite store: rejected request cannot have auth scope: %w", domain.ErrInvalid)
		}
		if request.CompletedAt == nil {
			completedAt := request.StartedAt
			request.CompletedAt = &completedAt
		}
	default:
		return domain.ProxyRequest{}, fmt.Errorf("sqlite store: request must begin in progress or rejected: %w", domain.ErrInvalid)
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.ProxyRequest{}, fmt.Errorf("sqlite store: begin proxy request: %w", classifyError(errBegin))
	}
	if errInsert := insertProxyRequestWithScopes(ctx, tx, request, scopes); errInsert != nil {
		return domain.ProxyRequest{}, rollback(tx, errInsert)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.ProxyRequest{}, fmt.Errorf("sqlite store: commit proxy request: %w", classifyError(errCommit))
	}
	return request, nil
}

func (s *Store) CompleteProxyRequest(ctx context.Context, completion domain.RequestCompletion) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	if completion.RequestID == "" || !completion.Outcome.Terminal() {
		return fmt.Errorf("sqlite store: invalid proxy request completion: %w", domain.ErrInvalid)
	}
	if completion.CompletedAt.IsZero() {
		completion.CompletedAt = s.currentTime()
	}
	result, errExec := s.db.ExecContext(ctx, `
		UPDATE proxy_requests
		SET requested_model = ?, stream = ?, completed_at = ?, outcome = ?,
		    status_class = ?, reason_code = ?, upstream_attempted = ?
		WHERE request_id = ? AND outcome = 'in_progress'
	`, completion.RequestedModel, boolToInt(completion.Stream), toDatabaseTime(completion.CompletedAt),
		completion.Outcome, completion.StatusClass, completion.ReasonCode,
		boolToInt(completion.UpstreamAttempted), completion.RequestID)
	if errExec != nil {
		return fmt.Errorf("sqlite store: complete proxy request: %w", classifyError(errExec))
	}
	rows, errRows := affectedRows("complete proxy request", result)
	if errRows != nil {
		return errRows
	}
	if rows == 1 {
		return nil
	}
	var outcome domain.RequestOutcome
	if errQuery := s.db.QueryRowContext(ctx, "SELECT outcome FROM proxy_requests WHERE request_id = ?", completion.RequestID).Scan(&outcome); errQuery != nil {
		return scanError("read completed proxy request", errQuery)
	}
	if outcome == completion.Outcome {
		return nil
	}
	return fmt.Errorf("sqlite store: proxy request already has outcome %q: %w", outcome, domain.ErrConflict)
}

func (s *Store) RecoverInterruptedRequests(ctx context.Context, recoveredAt time.Time) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	if recoveredAt.IsZero() {
		recoveredAt = s.currentTime()
	}
	result, errExec := s.db.ExecContext(ctx, `
		UPDATE proxy_requests
		SET outcome = 'incomplete', completed_at = ?, reason_code = 'process_interrupted'
		WHERE outcome = 'in_progress'
	`, toDatabaseTime(recoveredAt))
	if errExec != nil {
		return 0, fmt.Errorf("sqlite store: recover interrupted proxy requests: %w", classifyError(errExec))
	}
	return affectedRows("recover interrupted proxy requests", result)
}

func (s *Store) GetProxyRequest(ctx context.Context, requestID string) (domain.ProxyRequest, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.ProxyRequest{}, errReady
	}
	var request domain.ProxyRequest
	var carID, membershipID, memberRef, displayName, scopeHash, billingPeriodID, catalogHash, billingStatus sql.NullString
	var startedAt int64
	var completedAt, coverageFrom, billedNano sql.NullInt64
	errScan := s.db.QueryRowContext(ctx, `
		SELECT request_id, user_id, api_key_id, car_id, membership_id,
		       member_ref_snapshot, display_name_snapshot, scope_hash, scope_size,
		       source_format, requested_model, stream, started_at, completed_at,
		       outcome, status_class, reason_code, upstream_attempted,
		       billing_period_id, pricing_catalog_hash, pricing_coverage_from, billing_status, billed_nano_usd
		FROM proxy_requests WHERE request_id = ?
	`, requestID).Scan(&request.RequestID, &request.UserID, &request.APIKeyID, &carID,
		&membershipID, &memberRef, &displayName, &scopeHash, &request.ScopeSize,
		&request.SourceFormat, &request.RequestedModel, &request.Stream, &startedAt,
		&completedAt, &request.Outcome, &request.StatusClass, &request.ReasonCode,
		&request.UpstreamAttempted, &billingPeriodID, &catalogHash, &coverageFrom, &billingStatus, &billedNano)
	if errScan != nil {
		return domain.ProxyRequest{}, scanError("get proxy request", errScan)
	}
	request.CarID = carID.String
	request.MembershipID = membershipID.String
	request.MemberRefSnapshot = memberRef.String
	request.DisplayNameSnapshot = displayName.String
	request.ScopeHash = scopeHash.String
	request.BillingPeriodID = billingPeriodID.String
	request.PricingCatalogHash = catalogHash.String
	request.PricingCoverageFrom = fromNullableDatabaseTime(coverageFrom)
	request.BillingStatus = billingStatus.String
	request.BilledNanoUSD = fromNullableInt64(billedNano)
	request.StartedAt = fromDatabaseTime(startedAt)
	request.CompletedAt = fromNullableDatabaseTime(completedAt)
	return request, nil
}

func (s *Store) ListRequestScopes(ctx context.Context, requestID string) (scopes []domain.ProxyRequestAuthScope, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT request_id, auth_id, assignment_id, account_ref_snapshot,
		       safe_label_snapshot, provider_snapshot
		FROM proxy_request_auth_scopes WHERE request_id = ? ORDER BY auth_id
	`, requestID)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list proxy request scopes: %w", classifyError(errQuery))
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("sqlite store: close proxy request scope rows: %w", errClose))
		}
	}()
	for rows.Next() {
		var scope domain.ProxyRequestAuthScope
		if errScan := rows.Scan(&scope.RequestID, &scope.AuthID, &scope.AssignmentID,
			&scope.AccountRefSnapshot, &scope.SafeLabelSnapshot, &scope.ProviderSnapshot); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan proxy request scope: %w", errScan)
		}
		scopes = append(scopes, scope)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate proxy request scopes: %w", errRows)
	}
	return scopes, nil
}

func (s *Store) InsertUsageEvent(ctx context.Context, event domain.UsageEvent) (domain.UsageEvent, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.UsageEvent{}, errReady
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if event.RequestID == "" || event.AuthID == "" || event.Provider == "" {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: incomplete usage event: %w", domain.ErrInvalid)
	}
	if !event.UsageKnown && anyTokenPresent(event) {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: unknown usage cannot contain token values: %w", domain.ErrInvalid)
	}
	if anyNegativeToken(event) {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: usage token values cannot be negative: %w", domain.ErrInvalid)
	}
	if event.RequestedAt.IsZero() {
		event.RequestedAt = s.currentTime()
	}
	if event.RecordedAt.IsZero() {
		event.RecordedAt = s.currentTime()
	}
	if event.PricingStatus == "" {
		if event.UsageKnown {
			event.PricingStatus = "pending"
		} else {
			event.PricingStatus = "unknown"
		}
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: begin usage event: %w", classifyError(errBegin))
	}
	errScope := tx.QueryRowContext(ctx, `
		SELECT assignment_id, account_ref_snapshot, safe_label_snapshot, provider_snapshot
		FROM proxy_request_auth_scopes WHERE request_id = ? AND auth_id = ?
	`, event.RequestID, event.AuthID).Scan(&event.AssignmentID, &event.AccountRefSnapshot,
		&event.SafeLabelSnapshot, &event.Provider)
	if errScope != nil {
		return domain.UsageEvent{}, rollback(tx, scanError("read usage event request scope", errScope))
	}
	result, errInsert := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO usage_events (
			event_id, request_id, billing_period_id, event_seq, attempt_no, auth_id, assignment_id,
			account_ref_snapshot, safe_label_snapshot, provider, model, usage_known,
			input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,
			failed, status_class, requested_at, recorded_at,
			canonical_schema, canonical_quality, uncached_input_tokens, cache_read_tokens,
			cache_write_tokens, non_reasoning_tokens, request_service_tier, response_service_tier,
			pricing_status, pricing_reason, price_input_per_token, price_output_per_token,
			price_cache_read, price_cache_write, cost_nano_usd
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.EventID, event.RequestID, nullableString(event.BillingPeriodID), nullableInt64(event.EventSeq), nullableInt64(event.AttemptNo),
		event.AuthID, event.AssignmentID, event.AccountRefSnapshot, event.SafeLabelSnapshot,
		event.Provider, event.Model, boolToInt(event.UsageKnown), nullableInt64(event.InputTokens),
		nullableInt64(event.OutputTokens), nullableInt64(event.CachedTokens), nullableInt64(event.ReasoningTokens),
		nullableInt64(event.TotalTokens), boolToInt(event.Failed), event.StatusClass,
		toDatabaseTime(event.RequestedAt), toDatabaseTime(event.RecordedAt), event.CanonicalSchema, event.CanonicalQuality,
		nullableInt64(event.UncachedInputTokens), nullableInt64(event.CacheReadTokens), nullableInt64(event.CacheWriteTokens),
		nullableInt64(event.NonReasoningTokens), event.RequestServiceTier, event.ResponseServiceTier,
		event.PricingStatus, event.PricingReason, event.PriceInputPerToken, event.PriceOutputPerToken,
		event.PriceCacheRead, event.PriceCacheWrite, nullableInt64(event.CostNanoUSD))
	if errInsert != nil {
		return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: insert usage event: %w", classifyError(errInsert)))
	}
	rows, errRows := affectedRows("insert usage event", result)
	if errRows != nil {
		return domain.UsageEvent{}, rollback(tx, errRows)
	}
	if rows == 0 {
		var requestID, authID string
		if errExisting := tx.QueryRowContext(ctx, `
			SELECT request_id, auth_id FROM usage_events WHERE event_id = ?
		`, event.EventID).Scan(&requestID, &authID); errExisting != nil {
			return domain.UsageEvent{}, rollback(tx, scanError("read duplicate usage event", errExisting))
		}
		if requestID != event.RequestID || authID != event.AuthID {
			return domain.UsageEvent{}, rollback(tx, fmt.Errorf("sqlite store: usage event ID collision: %w", domain.ErrConflict))
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.UsageEvent{}, fmt.Errorf("sqlite store: commit usage event: %w", classifyError(errCommit))
	}
	return event, nil
}

func (s *Store) InsertAuditEvent(ctx context.Context, event domain.AuditEvent) (domain.AuditEvent, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.AuditEvent{}, errReady
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.currentTime()
	}
	if errInsert := insertAudit(ctx, s.db, &event, event.OccurredAt); errInsert != nil {
		return domain.AuditEvent{}, errInsert
	}
	return event, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) prepareUser(user domain.User) (domain.User, error) {
	now := s.currentTime()
	if user.ID == "" {
		user.ID = uuid.NewString()
	}
	if user.UserRef == "" {
		user.UserRef = uuid.NewString()
	}
	user.Username = strings.TrimSpace(user.Username)
	if user.UsernameNormalized == "" {
		user.UsernameNormalized = strings.ToLower(user.Username)
	}
	user.DefaultDisplayName = strings.TrimSpace(user.DefaultDisplayName)
	if user.Status == "" {
		user.Status = domain.UserStatusActive
	}
	if user.PasswordVersion == 0 {
		user.PasswordVersion = 1
	}
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if user.UpdatedAt.IsZero() {
		user.UpdatedAt = user.CreatedAt
	}
	if user.Username == "" || user.UsernameNormalized == "" || user.DefaultDisplayName == "" || user.PasswordHash == "" || !user.Role.Valid() || !user.Status.Valid() || user.PasswordVersion < 1 {
		return domain.User{}, fmt.Errorf("sqlite store: incomplete user: %w", domain.ErrInvalid)
	}
	return user, nil
}

func userValues(user domain.User) []any {
	return []any{
		user.ID, user.UserRef, user.Username, user.UsernameNormalized, user.DefaultDisplayName,
		user.Role, user.Status, user.PasswordHash, user.PasswordVersion,
		boolToInt(user.MustChangePassword), nullableDatabaseTime(user.DisabledAt),
		toDatabaseTime(user.CreatedAt), toDatabaseTime(user.UpdatedAt),
	}
}

func scanUser(row *sql.Row) (domain.User, error) {
	var user domain.User
	var disabledAt sql.NullInt64
	var createdAt, updatedAt int64
	errScan := row.Scan(&user.ID, &user.UserRef, &user.Username, &user.UsernameNormalized,
		&user.DefaultDisplayName, &user.Role, &user.Status, &user.PasswordHash,
		&user.PasswordVersion, &user.MustChangePassword, &disabledAt, &createdAt, &updatedAt)
	if errScan != nil {
		return domain.User{}, scanError("get user", errScan)
	}
	user.DisabledAt = fromNullableDatabaseTime(disabledAt)
	user.CreatedAt = fromDatabaseTime(createdAt)
	user.UpdatedAt = fromDatabaseTime(updatedAt)
	return user, nil
}

func scanMembership(row *sql.Row) (domain.Membership, error) {
	var membership domain.Membership
	var startedAt int64
	var endedAt, limit, anchor sql.NullInt64
	var timezone string
	errScan := row.Scan(&membership.ID, &membership.MemberRef, &membership.UserID,
		&membership.CarID, &membership.DisplayName, &membership.DisplayNameKey,
		&startedAt, &endedAt, &membership.EndedReason, &membership.CreatedByUserID,
		&limit, &timezone, &anchor)
	if errScan != nil {
		return domain.Membership{}, scanError("get current membership", errScan)
	}
	membership.StartedAt = fromDatabaseTime(startedAt)
	membership.EndedAt = fromNullableDatabaseTime(endedAt)
	membership.MonthlyLimitNanoUSD = fromNullableInt64(limit)
	membership.BillingTimezone = timezone
	if anchor.Valid {
		membership.BillingAnchorAt = fromDatabaseTime(anchor.Int64)
	}
	return membership, nil
}

func scanAuthAssignment(row *sql.Row) (domain.AuthAssignment, error) {
	var assignment domain.AuthAssignment
	var startedAt int64
	var endedAt sql.NullInt64
	errScan := row.Scan(&assignment.ID, &assignment.AccountRef, &assignment.CarID,
		&assignment.AuthID, &assignment.SafeLabel, &assignment.SafeLabelKey,
		&assignment.ProviderSnapshot, &startedAt, &endedAt, &assignment.EndedReason,
		&assignment.CreatedByUserID)
	if errScan != nil {
		return domain.AuthAssignment{}, scanError("get current auth assignment", errScan)
	}
	assignment.StartedAt = fromDatabaseTime(startedAt)
	assignment.EndedAt = fromNullableDatabaseTime(endedAt)
	return assignment, nil
}

func bumpCarVersions(ctx context.Context, tx *sql.Tx, now time.Time, carIDs ...string) error {
	seen := make(map[string]struct{}, len(carIDs))
	for _, carID := range carIDs {
		if carID == "" {
			continue
		}
		if _, ok := seen[carID]; ok {
			continue
		}
		seen[carID] = struct{}{}
		if _, errExec := tx.ExecContext(ctx, `
			UPDATE cars SET version = version + 1, updated_at = ? WHERE id = ?
		`, toDatabaseTime(now), carID); errExec != nil {
			return fmt.Errorf("sqlite store: update car version: %w", classifyError(errExec))
		}
	}
	return nil
}

func insertAudit(ctx context.Context, target execer, event *domain.AuditEvent, fallbackTime time.Time) error {
	if event == nil {
		return nil
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = fallbackTime
	}
	if event.ActorType == "" || event.ActorRef == "" || event.Action == "" || event.TargetType == "" || event.TargetRef == "" || event.Result == "" {
		return fmt.Errorf("sqlite store: incomplete audit event: %w", domain.ErrInvalid)
	}
	metadata := bytes.TrimSpace(event.MetadataJSON)
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	if !json.Valid(metadata) || metadata[0] != '{' {
		return fmt.Errorf("sqlite store: audit metadata must be a JSON object: %w", domain.ErrInvalid)
	}
	if _, errExec := target.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, occurred_at, request_id, actor_type, actor_ref, action,
			target_type, target_ref, result, reason_code, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ID, toDatabaseTime(event.OccurredAt), nullableString(event.RequestID), event.ActorType,
		event.ActorRef, event.Action, event.TargetType, event.TargetRef, event.Result,
		event.ReasonCode, string(metadata)); errExec != nil {
		return fmt.Errorf("sqlite store: insert audit event: %w", classifyError(errExec))
	}
	return nil
}

func insertAudits(ctx context.Context, target execer, fallbackTime time.Time, events ...*domain.AuditEvent) error {
	for _, event := range events {
		if errAudit := insertAudit(ctx, target, event, fallbackTime); errAudit != nil {
			return errAudit
		}
	}
	return nil
}

func scanError(operation string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("sqlite store: %s: %w", operation, domain.ErrNotFound)
	}
	return fmt.Errorf("sqlite store: %s: %w", operation, classifyError(err))
}

func affectedRows(operation string, result sql.Result) (int64, error) {
	rows, errRows := result.RowsAffected()
	if errRows != nil {
		return 0, fmt.Errorf("sqlite store: %s rows affected: %w", operation, errRows)
	}
	return rows, nil
}

func toDatabaseTime(value time.Time) int64 {
	return value.UTC().Truncate(time.Microsecond).UnixMicro()
}

func fromDatabaseTime(value int64) time.Time {
	return time.UnixMicro(value).UTC()
}

func nullableDatabaseTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return toDatabaseTime(*value)
}

func fromNullableDatabaseTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := fromDatabaseTime(value.Int64)
	return &result
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func fromNullableInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}

func fromNullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func anyTokenPresent(event domain.UsageEvent) bool {
	return event.InputTokens != nil || event.OutputTokens != nil || event.CachedTokens != nil ||
		event.ReasoningTokens != nil || event.TotalTokens != nil
}

func anyNegativeToken(event domain.UsageEvent) bool {
	for _, value := range []*int64{event.InputTokens, event.OutputTokens, event.CachedTokens, event.ReasoningTokens, event.TotalTokens} {
		if value != nil && *value < 0 {
			return true
		}
	}
	return false
}

func quotaLimitValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
