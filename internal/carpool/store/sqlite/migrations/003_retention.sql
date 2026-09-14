ALTER TABLE usage_events ADD COLUMN billing_period_id TEXT REFERENCES billing_periods(id);
CREATE INDEX usage_events_period ON usage_events(billing_period_id, requested_at);

CREATE TABLE retention_jobs (
    id TEXT PRIMARY KEY,
    operation TEXT NOT NULL CHECK (operation IN ('usage_details', 'closed_periods', 'reset_current_period')),
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
CREATE INDEX retention_jobs_requested ON retention_jobs(requested_at, id);
