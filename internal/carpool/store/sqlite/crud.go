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

func (s *Store) GetUserByNormalizedUsername(ctx context.Context, username string) (domain.User, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.User{}, errReady
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return domain.User{}, fmt.Errorf("sqlite store: normalized username is required: %w", domain.ErrInvalid)
	}
	return scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, user_ref, username, username_normalized, default_display_name, role, status,
		       password_hash, password_version, must_change_password, disabled_at,
		       created_at, updated_at
		FROM users WHERE username_normalized = ?
	`, username))
}

func (s *Store) GetUserByRef(ctx context.Context, userRef string) (domain.User, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.User{}, errReady
	}
	if strings.TrimSpace(userRef) == "" {
		return domain.User{}, fmt.Errorf("sqlite store: user reference is required: %w", domain.ErrInvalid)
	}
	return scanUser(s.db.QueryRowContext(ctx, `
		SELECT id, user_ref, username, username_normalized, default_display_name, role, status,
		       password_hash, password_version, must_change_password, disabled_at,
		       created_at, updated_at
		FROM users WHERE user_ref = ?
	`, userRef))
}

func (s *Store) ListUsers(ctx context.Context, afterUserRef string, limit int) (users []domain.User, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	limit, errLimit := normalizeLimit(limit)
	if errLimit != nil {
		return nil, errLimit
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT id, user_ref, username, username_normalized, default_display_name, role, status,
		       password_hash, password_version, must_change_password, disabled_at,
		       created_at, updated_at
		FROM users
		WHERE (? = '' OR user_ref > ?)
		ORDER BY user_ref
		LIMIT ?
	`, afterUserRef, afterUserRef, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list users: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "user")()
	for rows.Next() {
		var user domain.User
		var disabledAt sql.NullInt64
		var createdAt, updatedAt int64
		if errScan := rows.Scan(&user.ID, &user.UserRef, &user.Username, &user.UsernameNormalized,
			&user.DefaultDisplayName, &user.Role, &user.Status, &user.PasswordHash,
			&user.PasswordVersion, &user.MustChangePassword, &disabledAt, &createdAt, &updatedAt); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan user: %w", errScan)
		}
		user.DisabledAt = fromNullableDatabaseTime(disabledAt)
		user.CreatedAt = fromDatabaseTime(createdAt)
		user.UpdatedAt = fromDatabaseTime(updatedAt)
		users = append(users, user)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate users: %w", errRows)
	}
	return users, nil
}

func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	var count int64
	if errCount := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&count); errCount != nil {
		return 0, fmt.Errorf("sqlite store: count users: %w", classifyError(errCount))
	}
	return count, nil
}

func (s *Store) UpdateUser(ctx context.Context, user domain.User, audit *domain.AuditEvent) (domain.User, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.User{}, errReady
	}
	if user.ID == "" {
		return domain.User{}, fmt.Errorf("sqlite store: user ID is required: %w", domain.ErrInvalid)
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.User{}, fmt.Errorf("sqlite store: begin user update: %w", classifyError(errBegin))
	}
	existing, errExisting := scanUser(tx.QueryRowContext(ctx, `
		SELECT id, user_ref, username, username_normalized, default_display_name, role, status,
		       password_hash, password_version, must_change_password, disabled_at,
		       created_at, updated_at
		FROM users WHERE id = ?
	`, user.ID))
	if errExisting != nil {
		return domain.User{}, rollback(tx, errExisting)
	}
	if user.Role != "" && user.Role != existing.Role {
		return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: user role is immutable: %w", domain.ErrConflict))
	}
	user.Role = existing.Role
	user.UserRef = existing.UserRef
	user.CreatedAt = existing.CreatedAt
	if user.UpdatedAt.IsZero() {
		user.UpdatedAt = s.currentTime()
	}
	prepared, errPrepare := s.prepareUser(user)
	if errPrepare != nil {
		return domain.User{}, rollback(tx, errPrepare)
	}
	if prepared.Status == domain.UserStatusDisabled {
		if prepared.DisabledAt == nil {
			disabledAt := prepared.UpdatedAt
			prepared.DisabledAt = &disabledAt
		}
	} else {
		prepared.DisabledAt = nil
	}
	if prepared.PasswordVersion < existing.PasswordVersion ||
		(prepared.PasswordHash != existing.PasswordHash && prepared.PasswordVersion == existing.PasswordVersion) {
		return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: password changes must increment password version: %w", domain.ErrConflict))
	}
	if _, errUpdate := tx.ExecContext(ctx, `
		UPDATE users
		SET username = ?, username_normalized = ?, default_display_name = ?, status = ?,
		    password_hash = ?, password_version = ?, must_change_password = ?,
		    disabled_at = ?, updated_at = ?
		WHERE id = ?
	`, prepared.Username, prepared.UsernameNormalized, prepared.DefaultDisplayName, prepared.Status,
		prepared.PasswordHash, prepared.PasswordVersion, boolToInt(prepared.MustChangePassword),
		nullableDatabaseTime(prepared.DisabledAt), toDatabaseTime(prepared.UpdatedAt), prepared.ID); errUpdate != nil {
		return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: update user: %w", classifyError(errUpdate)))
	}
	if prepared.Status == domain.UserStatusDisabled {
		if _, errSessions := tx.ExecContext(ctx, `
			UPDATE sessions SET revoked_at = ?, revoke_reason = 'user_disabled'
			WHERE user_id = ? AND revoked_at IS NULL
		`, toDatabaseTime(prepared.UpdatedAt), prepared.ID); errSessions != nil {
			return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: revoke disabled user sessions: %w", classifyError(errSessions)))
		}
		if _, errKeys := tx.ExecContext(ctx, `
			UPDATE user_api_keys SET revoked_at = ?, revoke_reason = 'user_disabled'
			WHERE user_id = ? AND revoked_at IS NULL
		`, toDatabaseTime(prepared.UpdatedAt), prepared.ID); errKeys != nil {
			return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: revoke disabled user API keys: %w", classifyError(errKeys)))
		}
	} else if prepared.PasswordVersion != existing.PasswordVersion {
		if _, errSessions := tx.ExecContext(ctx, `
			UPDATE sessions SET revoked_at = ?, revoke_reason = 'password_changed'
			WHERE user_id = ? AND revoked_at IS NULL
		`, toDatabaseTime(prepared.UpdatedAt), prepared.ID); errSessions != nil {
			return domain.User{}, rollback(tx, fmt.Errorf("sqlite store: revoke password-changed user sessions: %w", classifyError(errSessions)))
		}
	}
	if errAudit := insertAudit(ctx, tx, audit, prepared.UpdatedAt); errAudit != nil {
		return domain.User{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.User{}, fmt.Errorf("sqlite store: commit user update: %w", classifyError(errCommit))
	}
	return prepared, nil
}

func (s *Store) RevokeSession(ctx context.Context, sessionID, reason string, revokedAt time.Time, audits ...*domain.AuditEvent) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	if revokedAt.IsZero() {
		revokedAt = s.currentTime()
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("sqlite store: begin session revocation: %w", classifyError(errBegin))
	}
	result, errExec := tx.ExecContext(ctx, `
		UPDATE sessions SET revoked_at = ?, revoke_reason = ?
		WHERE id = ? AND revoked_at IS NULL
	`, toDatabaseTime(revokedAt), reason, sessionID)
	if errExec != nil {
		return rollback(tx, fmt.Errorf("sqlite store: revoke session: %w", classifyError(errExec)))
	}
	rows, errRows := affectedRows("revoke session", result)
	if errRows != nil {
		return rollback(tx, errRows)
	}
	if rows == 0 {
		var count int
		if errQuery := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE id = ?", sessionID).Scan(&count); errQuery != nil {
			return rollback(tx, fmt.Errorf("sqlite store: revoke session existence check: %w", classifyError(errQuery)))
		}
		if count == 0 {
			return rollback(tx, domain.ErrNotFound)
		}
	}
	if errAudit := insertAudits(ctx, tx, revokedAt, audits...); errAudit != nil {
		return rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("sqlite store: commit session revocation: %w", classifyError(errCommit))
	}
	return nil
}

func (s *Store) TouchSessionLastSeen(ctx context.Context, sessionID string, seenAt time.Time, minimumInterval time.Duration) (bool, error) {
	if errReady := s.ready(); errReady != nil {
		return false, errReady
	}
	if seenAt.IsZero() {
		seenAt = s.currentTime()
	}
	if minimumInterval < 0 {
		return false, fmt.Errorf("sqlite store: session touch interval cannot be negative: %w", domain.ErrInvalid)
	}
	cutoff := seenAt.Add(-minimumInterval)
	result, errExec := s.db.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ?
		WHERE id = ? AND revoked_at IS NULL AND expires_at > ? AND last_seen_at <= ?
	`, toDatabaseTime(seenAt), sessionID, toDatabaseTime(seenAt), toDatabaseTime(cutoff))
	if errExec != nil {
		return false, fmt.Errorf("sqlite store: touch session last seen: %w", classifyError(errExec))
	}
	rows, errRows := affectedRows("touch session last seen", result)
	return rows == 1, errRows
}

func (s *Store) ListAPIKeysForUser(ctx context.Context, userID, afterKeyID string, limit int) (keys []domain.APIKey, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	limit, errLimit := normalizeLimit(limit)
	if errLimit != nil {
		return nil, errLimit
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT key_id, user_id, name, secret_digest, created_at, expires_at,
		       last_used_at, revoked_at, revoke_reason
		FROM user_api_keys
		WHERE user_id = ? AND (? = '' OR key_id > ?)
		ORDER BY key_id
		LIMIT ?
	`, userID, afterKeyID, afterKeyID, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list user API keys: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "API key")()
	for rows.Next() {
		key, errScan := scanAPIKeyRows(rows)
		if errScan != nil {
			return nil, errScan
		}
		keys = append(keys, key)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate user API keys: %w", errRows)
	}
	return keys, nil
}

func (s *Store) CountAPIKeysForUser(ctx context.Context, userID string) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	var count int64
	if errCount := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_api_keys WHERE user_id = ?
	`, userID).Scan(&count); errCount != nil {
		return 0, fmt.Errorf("sqlite store: count user API keys: %w", classifyError(errCount))
	}
	return count, nil
}

func (s *Store) RevokeAPIKeysForUser(ctx context.Context, userID, reason string, revokedAt time.Time, audits ...*domain.AuditEvent) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	if revokedAt.IsZero() {
		revokedAt = s.currentTime()
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return 0, fmt.Errorf("sqlite store: begin user API key revocation: %w", classifyError(errBegin))
	}
	result, errExec := tx.ExecContext(ctx, `
		UPDATE user_api_keys SET revoked_at = ?, revoke_reason = ?
		WHERE user_id = ? AND revoked_at IS NULL
	`, toDatabaseTime(revokedAt), reason, userID)
	if errExec != nil {
		return 0, rollback(tx, fmt.Errorf("sqlite store: revoke user API keys: %w", classifyError(errExec)))
	}
	rows, errRows := affectedRows("revoke user API keys", result)
	if errRows != nil {
		return 0, rollback(tx, errRows)
	}
	if errAudit := insertAudits(ctx, tx, revokedAt, audits...); errAudit != nil {
		return 0, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return 0, fmt.Errorf("sqlite store: commit user API key revocation: %w", classifyError(errCommit))
	}
	return rows, nil
}

func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, keyID string, usedAt time.Time, minimumInterval time.Duration) (bool, error) {
	if errReady := s.ready(); errReady != nil {
		return false, errReady
	}
	if usedAt.IsZero() {
		usedAt = s.currentTime()
	}
	if minimumInterval < 0 {
		return false, fmt.Errorf("sqlite store: API key touch interval cannot be negative: %w", domain.ErrInvalid)
	}
	cutoff := usedAt.Add(-minimumInterval)
	result, errExec := s.db.ExecContext(ctx, `
		UPDATE user_api_keys SET last_used_at = ?
		WHERE key_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
		  AND (last_used_at IS NULL OR last_used_at <= ?)
	`, toDatabaseTime(usedAt), keyID, toDatabaseTime(usedAt), toDatabaseTime(cutoff))
	if errExec != nil {
		return false, fmt.Errorf("sqlite store: touch API key last used: %w", classifyError(errExec))
	}
	rows, errRows := affectedRows("touch API key last used", result)
	return rows == 1, errRows
}

func (s *Store) GetCarByRef(ctx context.Context, carRef string) (domain.Car, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Car{}, errReady
	}
	if strings.TrimSpace(carRef) == "" {
		return domain.Car{}, fmt.Errorf("sqlite store: car reference is required: %w", domain.ErrInvalid)
	}
	return scanCar(s.db.QueryRowContext(ctx, `
		SELECT id, car_ref, name, description, seat_limit, status, version,
		       disabled_at, retired_at, created_at, updated_at
		FROM cars WHERE car_ref = ?
	`, carRef))
}

func (s *Store) ListCars(ctx context.Context, afterCarRef string, limit int) (cars []domain.Car, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	limit, errLimit := normalizeLimit(limit)
	if errLimit != nil {
		return nil, errLimit
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT id, car_ref, name, description, seat_limit, status, version,
		       disabled_at, retired_at, created_at, updated_at
		FROM cars WHERE (? = '' OR car_ref > ?) ORDER BY car_ref LIMIT ?
	`, afterCarRef, afterCarRef, limit)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list cars: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "car")()
	for rows.Next() {
		car, errScan := scanCarRows(rows)
		if errScan != nil {
			return nil, errScan
		}
		cars = append(cars, car)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate cars: %w", errRows)
	}
	return cars, nil
}

func (s *Store) CountCars(ctx context.Context) (int64, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	var count int64
	if errCount := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cars").Scan(&count); errCount != nil {
		return 0, fmt.Errorf("sqlite store: count cars: %w", classifyError(errCount))
	}
	return count, nil
}

func (s *Store) UpdateCar(ctx context.Context, car domain.Car, audit *domain.AuditEvent) (domain.Car, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.Car{}, errReady
	}
	if car.ID == "" || strings.TrimSpace(car.Name) == "" || !car.Status.Valid() || (car.SeatLimit != nil && *car.SeatLimit <= 0) {
		return domain.Car{}, fmt.Errorf("sqlite store: invalid car update: %w", domain.ErrInvalid)
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.Car{}, fmt.Errorf("sqlite store: begin car update: %w", classifyError(errBegin))
	}
	existing, errExisting := scanCar(tx.QueryRowContext(ctx, `
		SELECT id, car_ref, name, description, seat_limit, status, version,
		       disabled_at, retired_at, created_at, updated_at
		FROM cars WHERE id = ?
	`, car.ID))
	if errExisting != nil {
		return domain.Car{}, rollback(tx, errExisting)
	}
	if existing.Status == domain.CarStatusRetired && car.Status != domain.CarStatusRetired {
		return domain.Car{}, rollback(tx, fmt.Errorf("sqlite store: retired car cannot be reactivated: %w", domain.ErrConflict))
	}
	var occupied int
	if errCount := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memberships WHERE car_id = ? AND ended_at IS NULL
	`, car.ID).Scan(&occupied); errCount != nil {
		return domain.Car{}, rollback(tx, fmt.Errorf("sqlite store: count car members: %w", classifyError(errCount)))
	}
	if car.SeatLimit != nil && occupied > *car.SeatLimit {
		return domain.Car{}, rollback(tx, fmt.Errorf("sqlite store: seat limit is below current occupancy: %w", domain.ErrConflict))
	}
	car.CarRef = existing.CarRef
	car.CreatedAt = existing.CreatedAt
	car.Version = existing.Version + 1
	if car.UpdatedAt.IsZero() {
		car.UpdatedAt = s.currentTime()
	}
	switch car.Status {
	case domain.CarStatusActive:
		car.DisabledAt = nil
	case domain.CarStatusDisabled:
		if car.DisabledAt == nil {
			disabledAt := car.UpdatedAt
			car.DisabledAt = &disabledAt
		}
	case domain.CarStatusRetired:
		if car.RetiredAt == nil {
			retiredAt := car.UpdatedAt
			car.RetiredAt = &retiredAt
		}
	}
	if _, errUpdate := tx.ExecContext(ctx, `
		UPDATE cars
		SET name = ?, description = ?, seat_limit = ?, status = ?, version = ?,
		    disabled_at = ?, retired_at = ?, updated_at = ?
		WHERE id = ?
	`, strings.TrimSpace(car.Name), car.Description, nullableInt(car.SeatLimit), car.Status,
		car.Version, nullableDatabaseTime(car.DisabledAt), nullableDatabaseTime(car.RetiredAt),
		toDatabaseTime(car.UpdatedAt), car.ID); errUpdate != nil {
		return domain.Car{}, rollback(tx, fmt.Errorf("sqlite store: update car: %w", classifyError(errUpdate)))
	}
	if errAudit := insertAudit(ctx, tx, audit, car.UpdatedAt); errAudit != nil {
		return domain.Car{}, rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.Car{}, fmt.Errorf("sqlite store: commit car update: %w", classifyError(errCommit))
	}
	return car, nil
}

func (s *Store) ListCurrentMembershipsByCar(ctx context.Context, carID string) (memberships []domain.Membership, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT id, member_ref, user_id, car_id, display_name, display_name_key,
		       started_at, ended_at, ended_reason, created_by_user_id
		FROM memberships WHERE car_id = ? AND ended_at IS NULL ORDER BY display_name_key, id
	`, carID)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list current car memberships: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "membership")()
	for rows.Next() {
		membership, errScan := scanMembershipRows(rows)
		if errScan != nil {
			return nil, errScan
		}
		memberships = append(memberships, membership)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate current car memberships: %w", errRows)
	}
	return memberships, nil
}

func (s *Store) EndMembership(ctx context.Context, membershipID, reason string, endedAt time.Time, audit *domain.AuditEvent) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	if membershipID == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("sqlite store: membership ID and end reason are required: %w", domain.ErrInvalid)
	}
	if endedAt.IsZero() {
		endedAt = s.currentTime()
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("sqlite store: begin membership end: %w", classifyError(errBegin))
	}
	var carID string
	var startedAt int64
	if errQuery := tx.QueryRowContext(ctx, `
		SELECT car_id, started_at FROM memberships WHERE id = ? AND ended_at IS NULL
	`, membershipID).Scan(&carID, &startedAt); errQuery != nil {
		return rollback(tx, scanError("read membership to end", errQuery))
	}
	if toDatabaseTime(endedAt) < startedAt {
		return rollback(tx, fmt.Errorf("sqlite store: membership end precedes start: %w", domain.ErrInvalid))
	}
	if _, errUpdate := tx.ExecContext(ctx, `
		UPDATE memberships SET ended_at = ?, ended_reason = ?
		WHERE id = ? AND ended_at IS NULL
	`, toDatabaseTime(endedAt), strings.TrimSpace(reason), membershipID); errUpdate != nil {
		return rollback(tx, fmt.Errorf("sqlite store: end membership: %w", classifyError(errUpdate)))
	}
	if errVersions := bumpCarVersions(ctx, tx, endedAt, carID); errVersions != nil {
		return rollback(tx, errVersions)
	}
	if errAudit := insertAudit(ctx, tx, audit, endedAt); errAudit != nil {
		return rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("sqlite store: commit membership end: %w", classifyError(errCommit))
	}
	return nil
}

func (s *Store) ListCurrentAuthAssignmentsByCar(ctx context.Context, carID string) (assignments []domain.AuthAssignment, err error) {
	if errReady := s.ready(); errReady != nil {
		return nil, errReady
	}
	rows, errQuery := s.db.QueryContext(ctx, `
		SELECT id, account_ref, car_id, auth_id, safe_label, safe_label_key,
		       provider_snapshot, started_at, ended_at, ended_reason, created_by_user_id
		FROM car_auth_assignments WHERE car_id = ? AND ended_at IS NULL ORDER BY safe_label_key, id
	`, carID)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: list current car auth assignments: %w", classifyError(errQuery))
	}
	defer closeRows(&err, rows, "auth assignment")()
	for rows.Next() {
		assignment, errScan := scanAuthAssignmentRows(rows)
		if errScan != nil {
			return nil, errScan
		}
		assignments = append(assignments, assignment)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate current car auth assignments: %w", errRows)
	}
	return assignments, nil
}

func (s *Store) GetAuthAssignmentByRef(ctx context.Context, accountRef string) (domain.AuthAssignment, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.AuthAssignment{}, errReady
	}
	if strings.TrimSpace(accountRef) == "" {
		return domain.AuthAssignment{}, fmt.Errorf("sqlite store: account reference is required: %w", domain.ErrInvalid)
	}
	return scanAuthAssignment(s.db.QueryRowContext(ctx, `
		SELECT id, account_ref, car_id, auth_id, safe_label, safe_label_key,
		       provider_snapshot, started_at, ended_at, ended_reason, created_by_user_id
		FROM car_auth_assignments WHERE account_ref = ?
	`, accountRef))
}

func (s *Store) EndAuthAssignment(ctx context.Context, assignmentID, reason string, endedAt time.Time, audit *domain.AuditEvent) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	if assignmentID == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("sqlite store: assignment ID and end reason are required: %w", domain.ErrInvalid)
	}
	if endedAt.IsZero() {
		endedAt = s.currentTime()
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("sqlite store: begin auth assignment end: %w", classifyError(errBegin))
	}
	var carID string
	var startedAt int64
	if errQuery := tx.QueryRowContext(ctx, `
		SELECT car_id, started_at FROM car_auth_assignments WHERE id = ? AND ended_at IS NULL
	`, assignmentID).Scan(&carID, &startedAt); errQuery != nil {
		return rollback(tx, scanError("read auth assignment to end", errQuery))
	}
	if toDatabaseTime(endedAt) < startedAt {
		return rollback(tx, fmt.Errorf("sqlite store: auth assignment end precedes start: %w", domain.ErrInvalid))
	}
	if _, errUpdate := tx.ExecContext(ctx, `
		UPDATE car_auth_assignments SET ended_at = ?, ended_reason = ?
		WHERE id = ? AND ended_at IS NULL
	`, toDatabaseTime(endedAt), strings.TrimSpace(reason), assignmentID); errUpdate != nil {
		return rollback(tx, fmt.Errorf("sqlite store: end auth assignment: %w", classifyError(errUpdate)))
	}
	if errVersions := bumpCarVersions(ctx, tx, endedAt, carID); errVersions != nil {
		return rollback(tx, errVersions)
	}
	if errAudit := insertAudit(ctx, tx, audit, endedAt); errAudit != nil {
		return rollback(tx, errAudit)
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("sqlite store: commit auth assignment end: %w", classifyError(errCommit))
	}
	return nil
}

func scanAPIKey(row *sql.Row) (domain.APIKey, error) {
	var key domain.APIKey
	var createdAt int64
	var expiresAt, lastUsedAt, revokedAt sql.NullInt64
	errScan := row.Scan(&key.KeyID, &key.UserID, &key.Name, &key.SecretDigest, &createdAt,
		&expiresAt, &lastUsedAt, &revokedAt, &key.RevokeReason)
	if errScan != nil {
		return domain.APIKey{}, scanError("get API key", errScan)
	}
	populateAPIKeyTimes(&key, createdAt, expiresAt, lastUsedAt, revokedAt)
	return key, nil
}

func scanAPIKeyRows(rows *sql.Rows) (domain.APIKey, error) {
	var key domain.APIKey
	var createdAt int64
	var expiresAt, lastUsedAt, revokedAt sql.NullInt64
	if errScan := rows.Scan(&key.KeyID, &key.UserID, &key.Name, &key.SecretDigest, &createdAt,
		&expiresAt, &lastUsedAt, &revokedAt, &key.RevokeReason); errScan != nil {
		return domain.APIKey{}, fmt.Errorf("sqlite store: scan API key: %w", errScan)
	}
	populateAPIKeyTimes(&key, createdAt, expiresAt, lastUsedAt, revokedAt)
	return key, nil
}

func populateAPIKeyTimes(key *domain.APIKey, createdAt int64, expiresAt, lastUsedAt, revokedAt sql.NullInt64) {
	key.CreatedAt = fromDatabaseTime(createdAt)
	key.ExpiresAt = fromNullableDatabaseTime(expiresAt)
	key.LastUsedAt = fromNullableDatabaseTime(lastUsedAt)
	key.RevokedAt = fromNullableDatabaseTime(revokedAt)
}

func scanCar(row *sql.Row) (domain.Car, error) {
	var car domain.Car
	var seatLimit, disabledAt, retiredAt sql.NullInt64
	var createdAt, updatedAt int64
	errScan := row.Scan(&car.ID, &car.CarRef, &car.Name, &car.Description, &seatLimit,
		&car.Status, &car.Version, &disabledAt, &retiredAt, &createdAt, &updatedAt)
	if errScan != nil {
		return domain.Car{}, scanError("get car", errScan)
	}
	populateCarTimes(&car, seatLimit, disabledAt, retiredAt, createdAt, updatedAt)
	return car, nil
}

func scanCarRows(rows *sql.Rows) (domain.Car, error) {
	var car domain.Car
	var seatLimit, disabledAt, retiredAt sql.NullInt64
	var createdAt, updatedAt int64
	if errScan := rows.Scan(&car.ID, &car.CarRef, &car.Name, &car.Description, &seatLimit,
		&car.Status, &car.Version, &disabledAt, &retiredAt, &createdAt, &updatedAt); errScan != nil {
		return domain.Car{}, fmt.Errorf("sqlite store: scan car: %w", errScan)
	}
	populateCarTimes(&car, seatLimit, disabledAt, retiredAt, createdAt, updatedAt)
	return car, nil
}

func populateCarTimes(car *domain.Car, seatLimit, disabledAt, retiredAt sql.NullInt64, createdAt, updatedAt int64) {
	car.SeatLimit = fromNullableInt(seatLimit)
	car.DisabledAt = fromNullableDatabaseTime(disabledAt)
	car.RetiredAt = fromNullableDatabaseTime(retiredAt)
	car.CreatedAt = fromDatabaseTime(createdAt)
	car.UpdatedAt = fromDatabaseTime(updatedAt)
}

func scanMembershipRows(rows *sql.Rows) (domain.Membership, error) {
	var membership domain.Membership
	var startedAt int64
	var endedAt sql.NullInt64
	if errScan := rows.Scan(&membership.ID, &membership.MemberRef, &membership.UserID,
		&membership.CarID, &membership.DisplayName, &membership.DisplayNameKey,
		&startedAt, &endedAt, &membership.EndedReason, &membership.CreatedByUserID); errScan != nil {
		return domain.Membership{}, fmt.Errorf("sqlite store: scan membership: %w", errScan)
	}
	membership.StartedAt = fromDatabaseTime(startedAt)
	membership.EndedAt = fromNullableDatabaseTime(endedAt)
	return membership, nil
}

func scanAuthAssignmentRows(rows *sql.Rows) (domain.AuthAssignment, error) {
	var assignment domain.AuthAssignment
	var startedAt int64
	var endedAt sql.NullInt64
	if errScan := rows.Scan(&assignment.ID, &assignment.AccountRef, &assignment.CarID,
		&assignment.AuthID, &assignment.SafeLabel, &assignment.SafeLabelKey,
		&assignment.ProviderSnapshot, &startedAt, &endedAt, &assignment.EndedReason,
		&assignment.CreatedByUserID); errScan != nil {
		return domain.AuthAssignment{}, fmt.Errorf("sqlite store: scan auth assignment: %w", errScan)
	}
	assignment.StartedAt = fromDatabaseTime(startedAt)
	assignment.EndedAt = fromNullableDatabaseTime(endedAt)
	return assignment, nil
}

func normalizeLimit(limit int) (int, error) {
	if limit == 0 {
		return 100, nil
	}
	if limit < 1 || limit > 500 {
		return 0, fmt.Errorf("sqlite store: page limit must be between 1 and 500: %w", domain.ErrInvalid)
	}
	return limit, nil
}

func closeRows(targetErr *error, rows *sql.Rows, name string) func() {
	return func() {
		if errClose := rows.Close(); errClose != nil {
			*targetErr = errors.Join(*targetErr, fmt.Errorf("sqlite store: close %s rows: %w", name, errClose))
		}
	}
}
