package db

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SanitizeUrl drops query params only Prisma understands, so ari's
// DATABASE_URL works verbatim (pgx would forward them to Postgres, which
// rejects e.g. "schema" as an unrecognized configuration parameter).
func SanitizeUrl(databaseUrl string) string {
	u, err := url.Parse(databaseUrl)
	if err != nil {
		return databaseUrl
	}
	q := u.Query()
	for _, param := range []string{"schema", "connection_limit", "pool_timeout", "connect_timeout", "socket_timeout", "pgbouncer"} {
		q.Del(param)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func NewPool(ctx context.Context, databaseUrl string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(SanitizeUrl(databaseUrl))
	if err != nil {
		return nil, err
	}

	// pgx defaults MaxConns to max(4, NumCPU), which on a 2-CPU pod is FOUR - and
	// SanitizeUrl strips connection_limit/pool_timeout, so the URL cannot raise it
	// either. Four is far too few to share between the HTTP handlers, the outbound
	// worker, the job queue and seven sweeps, and pgxpool.Acquire takes no
	// timeout: contention presents as an unbounded hang rather than an error. Set
	// it explicitly instead of inheriting a CPU-derived default that silently
	// shrinks the service on a small node.
	if cfg.MaxConns < 20 {
		cfg.MaxConns = 20
	}
	// Something upstream drops idle sessions at roughly an hour (the recurring
	// "outbound listener disconnected: unexpected EOF"). Recycle under that so the
	// pool retires connections itself instead of handing out dead ones, and keep a
	// background health check so a broken connection surfaces between sweeps
	// rather than when a decision needs delivering.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint) // 23505 is Postgres unique_violation
	}
	return false
}

// InTx runs fn in a transaction, rolling back on error or panic.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
