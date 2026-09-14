ALTER TABLE memberships ADD COLUMN monthly_limit_nano_usd INTEGER CHECK (monthly_limit_nano_usd IS NULL OR monthly_limit_nano_usd >= 0);
ALTER TABLE memberships ADD COLUMN billing_timezone TEXT NOT NULL DEFAULT 'UTC';
ALTER TABLE memberships ADD COLUMN billing_anchor_at INTEGER;
UPDATE memberships SET billing_anchor_at = started_at WHERE billing_anchor_at IS NULL;

CREATE TABLE billing_periods (
    id TEXT PRIMARY KEY,
    membership_id TEXT NOT NULL REFERENCES memberships(id),
    member_ref_snapshot TEXT NOT NULL,
    car_id TEXT NOT NULL REFERENCES cars(id),
    timezone TEXT NOT NULL,
    anchor_at INTEGER NOT NULL,
    period_from INTEGER NOT NULL,
    period_to INTEGER NOT NULL,
    limit_nano_usd INTEGER CHECK (limit_nano_usd IS NULL OR limit_nano_usd >= 0),
    confirmed_nano_usd INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_nano_usd >= 0),
    reset_baseline_nano_usd INTEGER NOT NULL DEFAULT 0 CHECK (reset_baseline_nano_usd >= 0),
    unknown_cost_events INTEGER NOT NULL DEFAULT 0 CHECK (unknown_cost_events >= 0),
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
    UNIQUE (membership_id, period_from),
    CHECK (period_to > period_from)
) STRICT;
CREATE INDEX billing_periods_member_range ON billing_periods(membership_id, period_from, period_to);
CREATE INDEX billing_periods_car_range ON billing_periods(car_id, period_from, period_to);

ALTER TABLE proxy_requests ADD COLUMN billing_period_id TEXT REFERENCES billing_periods(id);
ALTER TABLE proxy_requests ADD COLUMN pricing_catalog_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_requests ADD COLUMN pricing_coverage_from INTEGER;
ALTER TABLE proxy_requests ADD COLUMN billing_status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE proxy_requests ADD COLUMN billed_nano_usd INTEGER CHECK (billed_nano_usd IS NULL OR billed_nano_usd >= 0);
CREATE INDEX proxy_requests_billing_period ON proxy_requests(billing_period_id, started_at);

ALTER TABLE usage_events ADD COLUMN canonical_schema INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_events ADD COLUMN canonical_quality TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN uncached_input_tokens INTEGER CHECK (uncached_input_tokens IS NULL OR uncached_input_tokens >= 0);
ALTER TABLE usage_events ADD COLUMN cache_read_tokens INTEGER CHECK (cache_read_tokens IS NULL OR cache_read_tokens >= 0);
ALTER TABLE usage_events ADD COLUMN cache_write_tokens INTEGER CHECK (cache_write_tokens IS NULL OR cache_write_tokens >= 0);
ALTER TABLE usage_events ADD COLUMN non_reasoning_tokens INTEGER CHECK (non_reasoning_tokens IS NULL OR non_reasoning_tokens >= 0);
ALTER TABLE usage_events ADD COLUMN request_service_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN response_service_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN pricing_status TEXT NOT NULL DEFAULT 'legacy_unpriced';
ALTER TABLE usage_events ADD COLUMN pricing_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN price_input_per_token TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN price_output_per_token TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN price_cache_read TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN price_cache_write TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN cost_nano_usd INTEGER CHECK (cost_nano_usd IS NULL OR cost_nano_usd >= 0);
CREATE INDEX usage_events_pricing_status ON usage_events(pricing_status, requested_at);

CREATE TABLE pricing_catalogs (
    hash TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    contents BLOB NOT NULL,
    loaded_at INTEGER NOT NULL,
    valid_until INTEGER,
    is_current INTEGER NOT NULL DEFAULT 0 CHECK (is_current IN (0, 1)),
    failure_reason TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE UNIQUE INDEX pricing_catalogs_current ON pricing_catalogs(is_current) WHERE is_current = 1;

CREATE TABLE carpool_billing_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    billing_enabled_at INTEGER NOT NULL,
    usage_retention_days INTEGER,
    pricing_catalog_hash TEXT,
    updated_at INTEGER NOT NULL,
    CHECK (usage_retention_days IS NULL OR usage_retention_days >= 0)
) STRICT;
INSERT INTO carpool_billing_settings(id, billing_enabled_at, updated_at)
SELECT 1, strftime('%s','now') * 1000000, strftime('%s','now') * 1000000
WHERE NOT EXISTS (SELECT 1 FROM carpool_billing_settings WHERE id = 1);
