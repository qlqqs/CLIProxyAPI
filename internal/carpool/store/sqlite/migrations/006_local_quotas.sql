UPDATE memberships SET five_hour_limit_nano_usd = 0 WHERE five_hour_limit_nano_usd IS NULL;
UPDATE memberships SET weekly_limit_nano_usd = 0 WHERE weekly_limit_nano_usd IS NULL;

CREATE TABLE member_quota_windows (
    membership_id TEXT NOT NULL REFERENCES memberships(id),
    kind TEXT NOT NULL CHECK (kind IN ('5h', '7d')),
    window_from INTEGER NOT NULL,
    reset_at INTEGER NOT NULL,
    confirmed_nano_usd INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_nano_usd >= 0),
    unknown_cost_events INTEGER NOT NULL DEFAULT 0 CHECK (unknown_cost_events >= 0),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (membership_id, kind),
    CHECK (reset_at > window_from)
) STRICT;

ALTER TABLE retention_jobs RENAME TO retention_jobs_legacy;
CREATE TABLE retention_jobs (
    id TEXT PRIMARY KEY,
    operation TEXT NOT NULL CHECK (operation IN ('usage_details', 'closed_periods', 'reset_current_period', 'reset_quota_windows')),
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed')),
    requested_at INTEGER NOT NULL,
    completed_at INTEGER,
    actor_ref TEXT NOT NULL,
    confirmation TEXT NOT NULL DEFAULT '',
    expected_count INTEGER NOT NULL DEFAULT 0 CHECK (expected_count >= 0),
    deleted_count INTEGER NOT NULL DEFAULT 0 CHECK (deleted_count >= 0),
    in_flight_count INTEGER NOT NULL DEFAULT 0 CHECK (in_flight_count >= 0),
    failure_reason TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO retention_jobs SELECT * FROM retention_jobs_legacy;
DROP TABLE retention_jobs_legacy;
CREATE INDEX retention_jobs_requested ON retention_jobs(requested_at, id);
