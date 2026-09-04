CREATE TABLE users (
    id TEXT PRIMARY KEY,
    user_ref TEXT NOT NULL UNIQUE,
    username TEXT NOT NULL,
    username_normalized TEXT NOT NULL UNIQUE,
    default_display_name TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('carpool_admin', 'passenger')),
    status TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    password_hash TEXT NOT NULL,
    password_version INTEGER NOT NULL CHECK (password_version >= 1),
    must_change_password INTEGER NOT NULL CHECK (must_change_password IN (0, 1)),
    disabled_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE INDEX users_role_status ON users(role, status);

CREATE TRIGGER users_role_immutable
BEFORE UPDATE OF role ON users
WHEN NEW.role <> OLD.role
BEGIN
    SELECT RAISE(ABORT, 'user role is immutable');
END;

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    token_digest BLOB NOT NULL UNIQUE,
    csrf_token TEXT NOT NULL,
    password_version INTEGER NOT NULL CHECK (password_version >= 1),
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER,
    revoke_reason TEXT NOT NULL DEFAULT '',
    CHECK (last_seen_at >= created_at),
    CHECK (expires_at > created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
) STRICT;

CREATE INDEX sessions_user_active ON sessions(user_id, revoked_at, expires_at);

CREATE TABLE user_api_keys (
    key_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    name TEXT NOT NULL,
    secret_digest BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    last_used_at INTEGER,
    revoked_at INTEGER,
    revoke_reason TEXT NOT NULL DEFAULT '',
    CHECK (expires_at IS NULL OR expires_at > created_at),
    CHECK (last_used_at IS NULL OR last_used_at >= created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
) STRICT;

CREATE INDEX user_api_keys_user_active ON user_api_keys(user_id, revoked_at, expires_at);

CREATE TRIGGER user_api_keys_passenger_only_insert
BEFORE INSERT ON user_api_keys
WHEN (SELECT role FROM users WHERE id = NEW.user_id) <> 'passenger'
BEGIN
    SELECT RAISE(ABORT, 'only passengers may have API keys');
END;

CREATE TABLE cars (
    id TEXT PRIMARY KEY,
    car_ref TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    seat_limit INTEGER CHECK (seat_limit IS NULL OR seat_limit > 0),
    status TEXT NOT NULL CHECK (status IN ('active', 'disabled', 'retired')),
    version INTEGER NOT NULL CHECK (version >= 1),
    disabled_at INTEGER,
    retired_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE INDEX cars_status ON cars(status);

CREATE TABLE memberships (
    id TEXT PRIMARY KEY,
    member_ref TEXT NOT NULL UNIQUE,
    user_id TEXT NOT NULL REFERENCES users(id),
    car_id TEXT NOT NULL REFERENCES cars(id),
    display_name TEXT NOT NULL,
    display_name_key TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    ended_reason TEXT NOT NULL DEFAULT '',
    created_by_user_id TEXT NOT NULL REFERENCES users(id),
    CHECK (ended_at IS NULL OR ended_at >= started_at)
) STRICT;

CREATE UNIQUE INDEX memberships_one_current_car
ON memberships(user_id) WHERE ended_at IS NULL;

CREATE UNIQUE INDEX memberships_current_display_name
ON memberships(car_id, display_name_key) WHERE ended_at IS NULL;

CREATE INDEX memberships_car_history ON memberships(car_id, started_at, ended_at);
CREATE INDEX memberships_user_history ON memberships(user_id, started_at, ended_at);

CREATE TRIGGER memberships_passenger_only_insert
BEFORE INSERT ON memberships
WHEN (SELECT role FROM users WHERE id = NEW.user_id) <> 'passenger'
BEGIN
    SELECT RAISE(ABORT, 'only passengers may have memberships');
END;

CREATE TABLE car_auth_assignments (
    id TEXT PRIMARY KEY,
    account_ref TEXT NOT NULL UNIQUE,
    car_id TEXT NOT NULL REFERENCES cars(id),
    auth_id TEXT NOT NULL,
    safe_label TEXT NOT NULL,
    safe_label_key TEXT NOT NULL,
    provider_snapshot TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    ended_reason TEXT NOT NULL DEFAULT '',
    created_by_user_id TEXT NOT NULL REFERENCES users(id),
    CHECK (ended_at IS NULL OR ended_at >= started_at)
) STRICT;

CREATE UNIQUE INDEX car_auth_one_current_car
ON car_auth_assignments(auth_id) WHERE ended_at IS NULL;

CREATE UNIQUE INDEX car_auth_current_safe_label
ON car_auth_assignments(car_id, safe_label_key) WHERE ended_at IS NULL;

CREATE INDEX car_auth_car_history ON car_auth_assignments(car_id, started_at, ended_at);
CREATE INDEX car_auth_auth_history ON car_auth_assignments(auth_id, started_at, ended_at);

CREATE TABLE proxy_requests (
    request_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    api_key_id TEXT NOT NULL REFERENCES user_api_keys(key_id),
    car_id TEXT REFERENCES cars(id),
    membership_id TEXT REFERENCES memberships(id),
    member_ref_snapshot TEXT,
    display_name_snapshot TEXT,
    scope_hash TEXT,
    scope_size INTEGER NOT NULL CHECK (scope_size >= 0),
    source_format TEXT NOT NULL,
    requested_model TEXT NOT NULL,
    stream INTEGER NOT NULL CHECK (stream IN (0, 1)),
    started_at INTEGER NOT NULL,
    completed_at INTEGER,
    outcome TEXT NOT NULL CHECK (outcome IN ('in_progress', 'succeeded', 'failed', 'rejected', 'canceled', 'incomplete')),
    status_class TEXT NOT NULL DEFAULT '',
    reason_code TEXT NOT NULL DEFAULT '',
    upstream_attempted INTEGER NOT NULL CHECK (upstream_attempted IN (0, 1)),
    CHECK (
        (outcome = 'in_progress' AND completed_at IS NULL) OR
        (outcome <> 'in_progress' AND completed_at IS NOT NULL)
    ),
    CHECK (
        outcome = 'rejected' OR
        (car_id IS NOT NULL AND membership_id IS NOT NULL AND
         member_ref_snapshot IS NOT NULL AND display_name_snapshot IS NOT NULL AND
         scope_hash IS NOT NULL AND scope_size > 0)
    )
) STRICT;

CREATE INDEX proxy_requests_car_started ON proxy_requests(car_id, started_at);
CREATE INDEX proxy_requests_user_started ON proxy_requests(user_id, started_at);
CREATE INDEX proxy_requests_membership_started ON proxy_requests(membership_id, started_at);
CREATE INDEX proxy_requests_api_key_started ON proxy_requests(api_key_id, started_at);
CREATE INDEX proxy_requests_outcome_completed ON proxy_requests(outcome, completed_at);
CREATE INDEX proxy_requests_terminal_completed
ON proxy_requests(completed_at, request_id) WHERE outcome <> 'in_progress';

CREATE TABLE proxy_request_auth_scopes (
    request_id TEXT NOT NULL REFERENCES proxy_requests(request_id) ON DELETE CASCADE,
    auth_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL REFERENCES car_auth_assignments(id),
    account_ref_snapshot TEXT NOT NULL,
    safe_label_snapshot TEXT NOT NULL,
    provider_snapshot TEXT NOT NULL,
    PRIMARY KEY (request_id, auth_id),
    UNIQUE (request_id, auth_id, assignment_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX proxy_request_auth_scopes_assignment
ON proxy_request_auth_scopes(assignment_id, request_id);

CREATE TRIGGER proxy_request_auth_scopes_match_car
BEFORE INSERT ON proxy_request_auth_scopes
WHEN NOT EXISTS (
    SELECT 1
    FROM proxy_requests p
    JOIN car_auth_assignments a ON a.id = NEW.assignment_id
    WHERE p.request_id = NEW.request_id
      AND p.outcome = 'in_progress'
      AND p.car_id = a.car_id
      AND a.auth_id = NEW.auth_id
)
BEGIN
    SELECT RAISE(ABORT, 'request scope does not match request car and assignment');
END;

CREATE TABLE usage_events (
    event_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES proxy_requests(request_id),
    event_seq INTEGER,
    attempt_no INTEGER,
    auth_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL REFERENCES car_auth_assignments(id),
    account_ref_snapshot TEXT NOT NULL,
    safe_label_snapshot TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    usage_known INTEGER NOT NULL CHECK (usage_known IN (0, 1)),
    input_tokens INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0),
    cached_tokens INTEGER CHECK (cached_tokens IS NULL OR cached_tokens >= 0),
    reasoning_tokens INTEGER CHECK (reasoning_tokens IS NULL OR reasoning_tokens >= 0),
    total_tokens INTEGER CHECK (total_tokens IS NULL OR total_tokens >= 0),
    failed INTEGER NOT NULL CHECK (failed IN (0, 1)),
    status_class TEXT NOT NULL DEFAULT '',
    requested_at INTEGER NOT NULL,
    recorded_at INTEGER NOT NULL,
    FOREIGN KEY (request_id, auth_id, assignment_id)
        REFERENCES proxy_request_auth_scopes(request_id, auth_id, assignment_id),
    CHECK (
        usage_known = 1 OR
        (input_tokens IS NULL AND output_tokens IS NULL AND cached_tokens IS NULL AND
         reasoning_tokens IS NULL AND total_tokens IS NULL)
    )
) STRICT;

CREATE INDEX usage_events_request_requested ON usage_events(request_id, requested_at);
CREATE INDEX usage_events_assignment_requested ON usage_events(assignment_id, requested_at);
CREATE INDEX usage_events_requested ON usage_events(requested_at, event_id);

CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    occurred_at INTEGER NOT NULL,
    request_id TEXT,
    actor_type TEXT NOT NULL,
    actor_ref TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_ref TEXT NOT NULL,
    result TEXT NOT NULL,
    reason_code TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    CHECK (json_valid(metadata_json))
) STRICT;

CREATE INDEX audit_events_occurred ON audit_events(occurred_at, id);
CREATE INDEX audit_events_action_occurred ON audit_events(action, occurred_at);
CREATE INDEX audit_events_request ON audit_events(request_id);
