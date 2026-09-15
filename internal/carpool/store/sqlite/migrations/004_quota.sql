ALTER TABLE memberships ADD COLUMN five_hour_limit_nano_usd INTEGER CHECK (five_hour_limit_nano_usd IS NULL OR five_hour_limit_nano_usd >= 0);
ALTER TABLE memberships ADD COLUMN weekly_limit_nano_usd INTEGER CHECK (weekly_limit_nano_usd IS NULL OR weekly_limit_nano_usd >= 0);

CREATE TABLE user_concurrency_limits (
    user_id TEXT PRIMARY KEY REFERENCES users(id),
    concurrency_limit INTEGER CHECK (concurrency_limit IS NULL OR concurrency_limit > 0)
) STRICT;
CREATE TABLE account_concurrency_limits (
    auth_id TEXT PRIMARY KEY,
    concurrency_limit INTEGER CHECK (concurrency_limit IS NULL OR concurrency_limit > 0)
) STRICT;

CREATE TABLE quota_windows (
    id INTEGER PRIMARY KEY,
    auth_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('5h', '7d')),
    window_from INTEGER NOT NULL,
    reset_at INTEGER NOT NULL,
    observed_at INTEGER NOT NULL,
    CHECK (reset_at > window_from)
) STRICT;
CREATE INDEX quota_windows_account ON quota_windows(auth_id, kind, id DESC);

-- These narrow accounting facts deliberately have no request-detail foreign key.
-- Ordinary retention must neither delete fees nor forget event deduplication.
CREATE TABLE quota_request_fees (
    request_id TEXT NOT NULL,
    auth_id TEXT NOT NULL,
    membership_id TEXT NOT NULL REFERENCES memberships(id),
    started_at INTEGER NOT NULL,
    confirmed_nano_usd INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_nano_usd >= 0),
    unknown_cost_events INTEGER NOT NULL DEFAULT 0 CHECK (unknown_cost_events >= 0),
    five_hour_window_id INTEGER REFERENCES quota_windows(id),
    weekly_window_id INTEGER REFERENCES quota_windows(id),
    PRIMARY KEY (request_id, auth_id)
) STRICT;
CREATE INDEX quota_request_fees_pending ON quota_request_fees(auth_id, started_at);
CREATE INDEX quota_request_fees_five_hour ON quota_request_fees(five_hour_window_id, membership_id);
CREATE INDEX quota_request_fees_weekly ON quota_request_fees(weekly_window_id, membership_id);
CREATE TABLE billed_event_receipts (
    event_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL,
    cost_nano_usd INTEGER CHECK (cost_nano_usd IS NULL OR cost_nano_usd >= 0),
    pricing_status TEXT NOT NULL,
    pricing_reason TEXT NOT NULL
) STRICT;
-- Seed only deduplication for existing events, never invent historic short fees.
INSERT INTO billed_event_receipts(event_id, request_id, cost_nano_usd, pricing_status, pricing_reason)
SELECT event_id, request_id, cost_nano_usd, pricing_status, pricing_reason FROM usage_events;
