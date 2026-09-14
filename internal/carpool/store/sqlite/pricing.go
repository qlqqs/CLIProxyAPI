package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/pricing"
)

// LoadPricingCatalog reads the last atomically activated price snapshot.
func (s *Store) LoadPricingCatalog(ctx context.Context) (pricing.CatalogRecord, error) {
	if errReady := s.ready(); errReady != nil {
		return pricing.CatalogRecord{}, errReady
	}
	var record pricing.CatalogRecord
	var loaded int64
	if err := s.db.QueryRowContext(ctx, `SELECT hash, source, contents, loaded_at FROM pricing_catalogs WHERE is_current=1 LIMIT 1`).Scan(&record.Hash, &record.Source, &record.Contents, &loaded); err != nil {
		if err == sql.ErrNoRows {
			return pricing.CatalogRecord{}, fmt.Errorf("sqlite store: pricing catalog is not cached: %w", domain.ErrNotFound)
		}
		return pricing.CatalogRecord{}, fmt.Errorf("sqlite store: load pricing catalog: %w", classifyError(err))
	}
	record.LoadedAt = fromDatabaseTime(loaded)
	return record, nil
}

// SavePricingCatalog atomically replaces the current validated snapshot.
func (s *Store) SavePricingCatalog(ctx context.Context, record pricing.CatalogRecord) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	if record.Hash == "" || record.Source == "" || len(record.Contents) == 0 || record.LoadedAt.IsZero() {
		return fmt.Errorf("sqlite store: incomplete pricing catalog: %w", domain.ErrInvalid)
	}
	tx, errBegin := s.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("sqlite store: begin pricing catalog: %w", classifyError(errBegin))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pricing_catalogs SET is_current=0 WHERE is_current=1`); err != nil {
		return rollback(tx, fmt.Errorf("sqlite store: retire pricing catalog: %w", classifyError(err)))
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO pricing_catalogs(hash, source, contents, loaded_at, valid_until, is_current, failure_reason) VALUES(?,?,?,?,NULL,1,'')`, record.Hash, record.Source, record.Contents, toDatabaseTime(record.LoadedAt)); err != nil {
		return rollback(tx, fmt.Errorf("sqlite store: save pricing catalog: %w", classifyError(err)))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store: commit pricing catalog: %w", classifyError(err))
	}
	return nil
}

// PricingCatalogUpdatedAt is kept small for callers that need a durable health hint.
func (s *Store) PricingCatalogUpdatedAt(ctx context.Context) (time.Time, error) {
	record, err := s.LoadPricingCatalog(ctx)
	return record.LoadedAt, err
}
