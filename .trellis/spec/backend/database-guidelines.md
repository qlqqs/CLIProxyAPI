# Database Guidelines

> Database patterns and conventions for this project.

---

## Overview

The project does not use an ORM, query builder, or migration framework. The
core persistence contract is the small `sdk/cliproxy/auth.Store` interface:

```go
type Store interface {
	List(context.Context) ([]*Auth, error)
	Save(context.Context, *Auth) (string, error)
	Delete(context.Context, string) error
}
```

The default token store is filesystem-backed (`sdk/auth/filestore.go`) and is
selected through the global registration in `sdk/auth/store_registry.go`.
PostgreSQL, Git, and S3-compatible object storage are optional integrations in
`internal/store/`; Redis/KV and the in-memory queue support home/runtime
features rather than replacing the auth-store contract. Treat persisted auth
JSON and config contents as sensitive data.

---

## Query Patterns

- PostgreSQL code uses `database/sql` with the pgx stdlib driver. Always pass
  the caller's `context.Context` to `QueryContext`, `QueryRowContext`,
  `ExecContext`, and `BeginTx`; do not create an unrelated timeout after an
  upstream connection has been established.
- Values are positional parameters (`$1`, `$2`, ...). Table and schema names
  are the only dynamic SQL fragments and must go through `fullTableName` and
  `quoteIdentifier` in `internal/store/postgresstore.go`.
- Check every query error, close rows on every path, and check `rows.Err()`
  after iteration. Handle close errors where relevant and wrap failures with
  operation context using `%w`.
- Normal config/auth writes are idempotent upserts. The config singleton uses
  the key `"config"`; auth records use a relative path/id and JSON content.
- `NewPostgresStore` trims and validates the DSN, opens the pgx-backed
  `database/sql` handle, and calls `PingContext` before returning. Preserve
  that fail-fast initialization contract and close the handle on startup
  errors.

For example, the auth upsert keeps values parameterized while allowing a
configured table name:

```go
query := fmt.Sprintf(`
	INSERT INTO %s (id, content, created_at, updated_at)
	VALUES ($1, $2, NOW(), NOW())
	ON CONFLICT (id)
	DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
`, s.fullTableName(s.cfg.AuthTable))
if _, errExec := s.db.ExecContext(ctx, query, relID, json.RawMessage(data)); errExec != nil {
	return fmt.Errorf("postgres store: upsert auth record: %w", errExec)
}
```

The default credential file store creates parent directories with mode `0700`
and new auth files with mode `0600`. The optional PostgreSQL, Git, and object
stores likewise use `0700` directories and `0600` auth or config files for
their local spool/mirror files that they explicitly create. Do not generalize
those modes to every config write: `config.SaveConfigPreserveComments` rewrites
an existing YAML file through `os.Create` and does not itself enforce mode
`0600`. Use that helper where applicable so YAML comments and ordering are not
discarded. Runtime cooldown state is kept separately as one `.cds` JSON
envelope per auth; records are grouped and sorted before writing, stale files
are removed, and replacement is done through a temporary file plus rename for
atomic visibility.

---

## Migrations

There is no migrations directory or migration runner. `PostgresStore.Bootstrap`
calls the idempotent `EnsureSchema`, which creates the configured schema (when
set) and these tables:

```sql
config_store (
    id TEXT PRIMARY KEY,
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
auth_store (
    id TEXT PRIMARY KEY,
    content JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
cooldown_store (
    auth_id TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    content JSONB NOT NULL,
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (auth_id, model)
)
```

If a schema change is needed, update `EnsureSchema` and the read/write code in
the same change, preserve compatibility with existing rows, and document any
manual production DDL. Do not invent a second migration mechanism.

---

## Naming Conventions

- Existing table names are lowercase `snake_case`: `config_store`,
  `auth_store`, and `cooldown_store` (custom names may be configured).
- Column names are lowercase `snake_case`; timestamps are `created_at` and
  `updated_at` and use PostgreSQL `TIMESTAMPTZ`.
- Auth/config payloads use `content`; auth and cooldown payloads are `JSONB`,
  while config text is stored as `TEXT`.
- Cooldown identity is the composite key `(auth_id, model)`. Deletions are
  timestamp-fenced tombstones (`deleted = TRUE`) rather than an unguarded
  delete, allowing concurrent state merges to converge.
- Never interpolate a value into SQL. `quoteIdentifier` is for identifiers only;
  it is not a substitute for parameters.

---

## Transactions

Cooldown saves use one transaction for all upserts and tombstones. Each write
is fenced by `updated_at`; failed statements call the rollback helper, and a
commit error is returned. Preserve this behavior when changing cooldown state:

```go
tx, errBegin := s.store.db.BeginTx(ctx, nil)
if errBegin != nil {
	return fmt.Errorf("postgres cooldown store: begin save: %w", errBegin)
}
// Execute timestamp-fenced upserts/tombstones on tx.
if errCommit := tx.Commit(); errCommit != nil {
	return fmt.Errorf("postgres cooldown store: commit save: %w", errCommit)
}
```

## Common Mistakes

Avoid these recurring failure modes:

- interpolating DSNs, IDs, or payloads into SQL (injection and quoting bugs);
- omitting `rows.Close`/`rows.Err`, context propagation, or transaction rollback;
- using wall-clock sleeps to test expiry/order; inject or manipulate timestamps
  instead (see `internal/store/postgres_cooldown_store_test.go`);
- deleting remote objects with a broad `os.RemoveAll` during watcher sync;
  object-store synchronization intentionally avoids that because a local
  watcher deletion could remove unrelated remote objects;
- allowing a watcher-originated deletion to remove a tracked Git auth record;
  `internal/store/gitstore.go` explicitly guards that path;
- bypassing the file-store permissions or writing auth JSON non-atomically.

Useful references are [PostgresStore](../../../internal/store/postgresstore.go),
[cooldown persistence tests](../../../internal/store/postgres_cooldown_store_test.go),
[Git store tests](../../../internal/store/gitstore_test.go), and
[file-store tests](../../../sdk/auth/filestore_test.go).
