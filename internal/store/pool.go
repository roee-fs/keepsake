package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// acquireTimeout bounds only the wait to acquire a pooled connection, mirroring
// psycopg_pool's default wait. A var, not a const, so tests can shrink it.
var acquireTimeout = 30 * time.Second

// nilTenant casts to uuid without raising and matches no tenant. tenant_isolation's
// USING clause casts TenantGUC to uuid whichever way Postgres plans admin_read's
// OR, and Postgres does not promise short-circuiting; an unset GUC reads as an
// empty string, which fails an uuid cast. This value gives that cast something to
// land on instead.
const nilTenant = "00000000-0000-0000-0000-000000000000"

// Store owns one pgx pool and the tenant scoping built on top of it: the GUC is
// set here and nowhere else. Ported from 2de90d2:src/keepsake/store/pool.py.
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
	// MaxConns, not just the default of 4: a pool that never grows past a
	// handful of connections caps the whole pod's concurrent writes there.
	cfg.MaxConns = int32(poolSize)
	cfg.MinConns = 1
	// A pooled connection does not notice the server going away, so a dead one
	// would otherwise be handed to the next acquirer as an unexplained transport
	// failure. Ping every acquire, matching pool.py's check=ConnectionPool.check_connection:
	// on a failed ping, pgxpool destroys that connection and tries another rather
	// than failing the caller's request (see pgxpool.Pool.Acquire's retry loop).
	cfg.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, searchPath: schema + ", pg_catalog"}, nil
}

// Close releases the pool's connections.
func (s *Store) Close() { s.pool.Close() }

// Healthy reports whether the pool can hand out a working connection right now.
// It answers rather than raises: the one thing a readiness probe must never do
// is fail to produce a verdict.
func (s *Store) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false
	}
	defer conn.Release()
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one) == nil
}

// Raw yields a connection with no tenant scope, read-only so it cannot become a
// write path into every tenant's rows at once. For startup checks only.
func (s *Store) Raw(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.tx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		"SELECT set_config('search_path', $1, true)", []any{s.searchPath}, fn)
}

// Scope yields a connection scoped to tenant for the life of one transaction.
func (s *Store) Scope(ctx context.Context, tenant uuid.UUID, fn func(pgx.Tx) error) error {
	return s.tx(ctx, pgx.TxOptions{},
		"SELECT set_config('search_path', $1, true), set_config($2, $3, true)",
		[]any{s.searchPath, TenantGUC, tenant.String()}, fn)
}

// AdminScope yields a read-only connection that reads every tenant, for the
// admin console only. Read-only, and the policy behind it is FOR SELECT: an
// admin has no write path into a tenant it did not name. okf.admin is
// self-asserted, so anything holding the app DSN can set it with or without
// this method; authentication happens above this method, never inside it.
func (s *Store) AdminScope(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.tx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		"SELECT set_config('search_path', $1, true), set_config($2, 'on', true), set_config($3, $4, true)",
		[]any{s.searchPath, AdminGUC, TenantGUC, nilTenant}, fn)
}

// tx acquires with a bounded wait, then runs setup and fn on the caller's own ctx
// for the rest of the transaction's life: psycopg_pool's timeout, which this
// mirrors, covers only the acquire, not the transaction an acquired connection
// then runs. set_config's is_local argument is SET LOCAL and, unlike SET, takes a
// parameter; a session-scoped value would outlive the transaction and be
// inherited by whoever next takes this connection from the pool.
func (s *Store) tx(ctx context.Context, opts pgx.TxOptions, setup string, args []any, fn func(pgx.Tx) error) error {
	acquireCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	conn, err := s.pool.Acquire(acquireCtx)
	cancel()
	if err != nil {
		return &acquireError{err}
	}
	defer conn.Release()

	return pgx.BeginTxFunc(ctx, conn, opts, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, setup, args...); err != nil {
			return err
		}
		return fn(tx)
	})
}

// acquireError marks an error as having occurred while acquiring a pooled
// connection, so IsUnavailable can tell a real acquire timeout from an unrelated
// deadline a caller's own fn happened to hit.
type acquireError struct{ err error }

func (e *acquireError) Error() string { return e.err.Error() }
func (e *acquireError) Unwrap() error { return e.err }
