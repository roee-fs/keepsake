package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acquireTimeout bounds only the wait to acquire a pooled connection, as psycopg_pool's does.
var acquireTimeout = 30 * time.Second

// SetAcquireTimeout overrides acquireTimeout for a test and returns a restorer.
func SetAcquireTimeout(d time.Duration) func() {
	orig := acquireTimeout
	acquireTimeout = d
	return func() { acquireTimeout = orig }
}

// nilTenant gives admin_read's uuid cast of an unset TenantGUC a value that matches no tenant.
var nilTenant = uuid.Nil.String()

// Store owns the pool and sets the tenant GUC, ported from 8f2af2e:src/keepsake/store/pool.py.
type Store struct {
	pool       *pgxpool.Pool
	searchPath string
}

// Open creates the pool for dsn, scoped to schema.
func Open(ctx context.Context, dsn, schema string) (*Store, error) {
	schema, err := ValidatedSchema(schema)
	if err != nil {
		return nil, err
	}
	poolSize, err := PoolSize()
	if err != nil {
		return nil, err
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = int32(poolSize)
	cfg.MinConns = 1
	// Every acquire, as pool.py's check_connection: pgxpool replaces a dead connection instead of failing the caller.
	cfg.ShouldPing = func(ctx context.Context, _ pgxpool.ShouldPingParams) bool { return true }

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, searchPath: schema + ", pg_catalog"}, nil
}

// OpenVerified opens the store and refuses it unless Verify passes.
func OpenVerified(ctx context.Context, dsn, schema string) (*Store, error) {
	s, err := Open(ctx, dsn, schema)
	if err != nil {
		return nil, err
	}
	if err := Verify(ctx, s, schema); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Stat snapshots the pool's connection counts.
func (s *Store) Stat() *pgxpool.Stat { return s.pool.Stat() }

// Close releases the pool's connections.
func (s *Store) Close() { s.pool.Close() }

// Healthy reports whether the pool can hand out a working connection; it never raises.
func (s *Store) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// The acquire pings, and replaces a dead connection, so it is the whole check.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false
	}
	conn.Release()
	return true
}

// Raw yields a read-only connection with no tenant scope, for startup checks only.
func (s *Store) Raw(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.tx(ctx, fmt.Sprintf("BEGIN READ ONLY; SELECT set_config('search_path', '%s', true)", s.searchPath), fn)
}

// Scope yields a connection scoped to tenant for the life of one transaction.
func (s *Store) Scope(ctx context.Context, tenant uuid.UUID, fn func(pgx.Tx) error) error {
	// okf.admin off explicitly: a role or DSN default of 'on' would widen every read.
	return s.tx(ctx, fmt.Sprintf(
		"BEGIN; SELECT set_config('search_path', '%s', true), set_config('%s', 'off', true), set_config('%s', '%s', true)",
		s.searchPath, AdminGUC, TenantGUC, tenant), fn)
}

// AdminScope yields a read-only connection reading every tenant; authentication happens above it.
func (s *Store) AdminScope(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.tx(ctx, fmt.Sprintf(
		"BEGIN READ ONLY; SELECT set_config('search_path', '%s', true), set_config('%s', 'on', true), set_config('%s', '%s', true)",
		s.searchPath, AdminGUC, TenantGUC, nilTenant), fn)
}

// tx runs fn in a transaction whose begin, built from validated values only, sets GUCs SET LOCAL in one round trip.
func (s *Store) tx(ctx context.Context, begin string, fn func(pgx.Tx) error) error {
	// The timeout covers only the acquire, as psycopg_pool's does, not the transaction.
	acquireCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	conn, err := s.pool.Acquire(acquireCtx)
	cancel()
	if err != nil {
		return &acquireError{err}
	}
	defer conn.Release()
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{BeginQuery: begin}, fn)
}

// acquireError marks an acquire failure, so IsUnavailable can tell it from fn's own deadline.
type acquireError struct{ err error }

func (e *acquireError) Error() string { return e.err.Error() }
func (e *acquireError) Unwrap() error { return e.err }
