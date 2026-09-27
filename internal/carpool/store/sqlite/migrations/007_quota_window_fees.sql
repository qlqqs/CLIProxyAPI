-- Window allocations survive rollover and ordinary request-detail retention.
-- Explicit quota resets discard these projections and start a new epoch;
-- permanent billed_event_receipts and historical request amounts are unchanged.
CREATE TABLE member_quota_window_fees (
    id INTEGER PRIMARY KEY,
    membership_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('5h', '7d')),
    started_at INTEGER NOT NULL,
    confirmed_nano_usd INTEGER NOT NULL CHECK (confirmed_nano_usd >= 0),
    unknown_cost_events INTEGER NOT NULL CHECK (unknown_cost_events >= 0),
    reliable INTEGER NOT NULL CHECK (reliable IN (0, 1)),
    FOREIGN KEY (membership_id, kind) REFERENCES member_quota_windows(membership_id, kind) ON DELETE CASCADE
) STRICT;
CREATE INDEX member_quota_window_fees_order ON member_quota_window_fees(membership_id, kind, started_at, reliable DESC, id);
