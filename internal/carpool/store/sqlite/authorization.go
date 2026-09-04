package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func (s *Store) AuthorizeAndBeginProxyRequest(ctx context.Context, input domain.ProxyAuthorization) (domain.AuthorizationSnapshot, error) {
	if errReady := s.ready(); errReady != nil {
		return domain.AuthorizationSnapshot{}, errReady
	}
	reasonCode := strings.TrimSpace(input.PreflightReasonCode)
	if input.UserID == "" || input.APIKeyID == "" || (strings.TrimSpace(input.SourceFormat) == "" && reasonCode == "") {
		return domain.AuthorizationSnapshot{}, fmt.Errorf("sqlite store: incomplete proxy authorization: %w", domain.ErrInvalid)
	}
	if input.RequestID == "" {
		input.RequestID = uuid.NewString()
	}
	if input.StartedAt.IsZero() {
		input.StartedAt = s.currentTime()
	}

	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return domain.AuthorizationSnapshot{}, fmt.Errorf("sqlite store: begin proxy authorization: %w", classifyError(errBegin))
	}
	result := domain.AuthorizationSnapshot{}
	user, errUser := scanUser(tx.QueryRowContext(ctx, `
		SELECT id, user_ref, username, username_normalized, default_display_name, role, status,
		       password_hash, password_version, must_change_password, disabled_at,
		       created_at, updated_at
		FROM users WHERE id = ?
	`, input.UserID))
	if errUser != nil {
		return domain.AuthorizationSnapshot{}, rollback(tx, fmt.Errorf("sqlite store: authorize user: %w", errUser))
	}
	result.User = user
	key, errKey := scanAPIKey(tx.QueryRowContext(ctx, `
		SELECT key_id, user_id, name, secret_digest, created_at, expires_at,
		       last_used_at, revoked_at, revoke_reason
		FROM user_api_keys WHERE key_id = ?
	`, input.APIKeyID))
	if errKey != nil {
		return domain.AuthorizationSnapshot{}, rollback(tx, fmt.Errorf("sqlite store: authorize API key: %w", errKey))
	}
	if key.UserID != user.ID {
		return domain.AuthorizationSnapshot{}, rollback(tx, fmt.Errorf("sqlite store: API key principal mismatch: %w", domain.ErrInvalid))
	}
	result.APIKey = key

	if user.Role != domain.UserRolePassenger {
		reasonCode = "role_not_passenger"
	} else if user.Status != domain.UserStatusActive {
		reasonCode = "user_disabled"
	} else if key.RevokedAt != nil {
		reasonCode = "api_key_revoked"
	} else if key.ExpiresAt != nil && !key.ExpiresAt.After(input.StartedAt) {
		reasonCode = "api_key_expired"
	}

	membership, errMembership := scanMembership(tx.QueryRowContext(ctx, `
		SELECT id, member_ref, user_id, car_id, display_name, display_name_key,
		       started_at, ended_at, ended_reason, created_by_user_id
		FROM memberships WHERE user_id = ? AND ended_at IS NULL
	`, user.ID))
	switch {
	case errMembership == nil:
		result.Membership = &membership
	case errors.Is(errMembership, domain.ErrNotFound):
		if reasonCode == "" {
			reasonCode = "no_current_membership"
		}
	default:
		return domain.AuthorizationSnapshot{}, rollback(tx, fmt.Errorf("sqlite store: authorize membership: %w", errMembership))
	}

	if result.Membership != nil {
		car, errCar := scanCar(tx.QueryRowContext(ctx, `
			SELECT id, car_ref, name, description, seat_limit, status, version,
			       disabled_at, retired_at, created_at, updated_at
			FROM cars WHERE id = ?
		`, result.Membership.CarID))
		if errCar != nil {
			return domain.AuthorizationSnapshot{}, rollback(tx, fmt.Errorf("sqlite store: authorize car: %w", errCar))
		}
		result.Car = &car
		if reasonCode == "" && car.Status != domain.CarStatusActive {
			reasonCode = "car_inactive"
		}
	}

	if reasonCode == "" && result.Car != nil {
		runtimeAuthIDs := make(map[string]struct{}, len(input.RuntimeAuthIDs))
		for _, authID := range input.RuntimeAuthIDs {
			authID = strings.TrimSpace(authID)
			if authID != "" {
				runtimeAuthIDs[authID] = struct{}{}
			}
		}
		scopes, errScopes := listAuthorizationScopes(ctx, tx, input.RequestID, result.Car.ID, runtimeAuthIDs)
		if errScopes != nil {
			return domain.AuthorizationSnapshot{}, rollback(tx, errScopes)
		}
		result.Scopes = scopes
		if len(scopes) == 0 {
			reasonCode = "no_available_accounts"
		}
	}

	request := domain.ProxyRequest{
		RequestID:      input.RequestID,
		UserID:         user.ID,
		APIKeyID:       key.KeyID,
		SourceFormat:   strings.TrimSpace(input.SourceFormat),
		RequestedModel: strings.TrimSpace(input.RequestedModel),
		Stream:         input.Stream,
		StartedAt:      input.StartedAt.UTC(),
		ReasonCode:     reasonCode,
	}
	if result.Membership != nil {
		request.CarID = result.Membership.CarID
		request.MembershipID = result.Membership.ID
		request.MemberRefSnapshot = result.Membership.MemberRef
		request.DisplayNameSnapshot = result.Membership.DisplayName
	}
	if reasonCode != "" {
		completedAt := input.StartedAt.UTC()
		request.CompletedAt = &completedAt
		request.Outcome = domain.RequestOutcomeRejected
		request.StatusClass = "4xx"
		request.ScopeSize = 0
		result.Scopes = nil
	} else {
		request.Outcome = domain.RequestOutcomeInProgress
		request.ScopeSize = len(result.Scopes)
		request.ScopeHash = authorizationScopeHash(result.Scopes)
	}
	if errInsert := insertProxyRequestWithScopes(ctx, tx, request, result.Scopes); errInsert != nil {
		return domain.AuthorizationSnapshot{}, rollback(tx, errInsert)
	}
	if reasonCode != "" {
		audit := &domain.AuditEvent{
			OccurredAt: input.StartedAt.UTC(),
			RequestID:  input.RequestID,
			ActorType:  "user_api_key",
			ActorRef:   user.UserRef,
			Action:     "authorization_reject",
			TargetType: "proxy_request",
			TargetRef:  input.RequestID,
			Result:     "rejected",
			ReasonCode: reasonCode,
		}
		if errAudit := insertAudit(ctx, tx, audit, input.StartedAt.UTC()); errAudit != nil {
			return domain.AuthorizationSnapshot{}, rollback(tx, errAudit)
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return domain.AuthorizationSnapshot{}, fmt.Errorf("sqlite store: commit proxy authorization: %w", classifyError(errCommit))
	}
	result.Request = request
	if reasonCode != "" {
		return result, fmt.Errorf("%w: %s", domain.ErrAuthorizationRejected, reasonCode)
	}
	return result, nil
}

func listAuthorizationScopes(ctx context.Context, tx *sql.Tx, requestID, carID string, runtimeAuthIDs map[string]struct{}) (scopes []domain.ProxyRequestAuthScope, err error) {
	rows, errQuery := tx.QueryContext(ctx, `
		SELECT id, account_ref, auth_id, safe_label, provider_snapshot
		FROM car_auth_assignments
		WHERE car_id = ? AND ended_at IS NULL
		ORDER BY auth_id
	`, carID)
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: query authorization assignments: %w", classifyError(errQuery))
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("sqlite store: close authorization assignment rows: %w", errClose))
		}
	}()
	for rows.Next() {
		var assignmentID, accountRef, authID, safeLabel, provider string
		if errScan := rows.Scan(&assignmentID, &accountRef, &authID, &safeLabel, &provider); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan authorization assignment: %w", errScan)
		}
		if _, available := runtimeAuthIDs[authID]; !available {
			continue
		}
		scopes = append(scopes, domain.ProxyRequestAuthScope{
			RequestID:          requestID,
			AuthID:             authID,
			AssignmentID:       assignmentID,
			AccountRefSnapshot: accountRef,
			SafeLabelSnapshot:  safeLabel,
			ProviderSnapshot:   provider,
		})
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate authorization assignments: %w", errRows)
	}
	return scopes, nil
}

func authorizationScopeHash(scopes []domain.ProxyRequestAuthScope) string {
	authIDs := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		authIDs = append(authIDs, scope.AuthID)
	}
	sort.Strings(authIDs)
	hash := sha256.New()
	for _, authID := range authIDs {
		_, _ = hash.Write([]byte(authID))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func insertProxyRequestWithScopes(ctx context.Context, target execer, request domain.ProxyRequest, scopes []domain.ProxyRequestAuthScope) error {
	if _, errInsert := target.ExecContext(ctx, `
		INSERT INTO proxy_requests (
			request_id, user_id, api_key_id, car_id, membership_id,
			member_ref_snapshot, display_name_snapshot, scope_hash, scope_size,
			source_format, requested_model, stream, started_at, completed_at,
			outcome, status_class, reason_code, upstream_attempted
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, request.RequestID, request.UserID, request.APIKeyID, nullableString(request.CarID),
		nullableString(request.MembershipID), nullableString(request.MemberRefSnapshot),
		nullableString(request.DisplayNameSnapshot), nullableString(request.ScopeHash), request.ScopeSize,
		request.SourceFormat, request.RequestedModel, boolToInt(request.Stream),
		toDatabaseTime(request.StartedAt), nullableDatabaseTime(request.CompletedAt), request.Outcome,
		request.StatusClass, request.ReasonCode, boolToInt(request.UpstreamAttempted)); errInsert != nil {
		return fmt.Errorf("sqlite store: insert proxy request: %w", classifyError(errInsert))
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope.AuthID == "" || scope.AssignmentID == "" || scope.AccountRefSnapshot == "" || scope.SafeLabelSnapshot == "" || scope.ProviderSnapshot == "" {
			return fmt.Errorf("sqlite store: incomplete proxy request scope: %w", domain.ErrInvalid)
		}
		if _, duplicate := seen[scope.AuthID]; duplicate {
			return fmt.Errorf("sqlite store: duplicate auth in proxy request scope: %w", domain.ErrInvalid)
		}
		seen[scope.AuthID] = struct{}{}
		if _, errScope := target.ExecContext(ctx, `
			INSERT INTO proxy_request_auth_scopes (
				request_id, auth_id, assignment_id, account_ref_snapshot,
				safe_label_snapshot, provider_snapshot
			) VALUES (?, ?, ?, ?, ?, ?)
		`, request.RequestID, scope.AuthID, scope.AssignmentID, scope.AccountRefSnapshot,
			scope.SafeLabelSnapshot, scope.ProviderSnapshot); errScope != nil {
			return fmt.Errorf("sqlite store: insert proxy request scope: %w", classifyError(errScope))
		}
	}
	return nil
}
