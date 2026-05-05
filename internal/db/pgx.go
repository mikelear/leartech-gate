// Package db wraps pgxpool with sensible leartech defaults.
//
// One DB type platform-wide per golden-standard: PostgreSQL (with TimescaleDB
// extension where time-series is needed). No MongoDB.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a pgx pool with leartech defaults:
//   - min 2 idle conns (keep at least one warm for health probes)
//   - max 20 conns (typical service; bump if you hit pool exhaustion)
//   - 30s connect timeout, 10min idle timeout
//
// Returns an error if the pool can't reach the database — callers should
// treat this as fatal during startup.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	cfg.MinConns = 2
	cfg.MaxConns = 20
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.ConnConfig.ConnectTimeout = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return pool, nil
}
