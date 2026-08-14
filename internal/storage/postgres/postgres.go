// Package postgres owns the database pool and shared query helpers.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ayran/whatsapp-automation/internal/config"
)

// DB wraps the connection pool so callers depend on one type instead of pgx
// internals spread across every package.
type DB struct {
	Pool *pgxpool.Pool
}

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, which lets repository
// methods run inside or outside a transaction without duplication.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Connect opens the pool and verifies connectivity, retrying briefly so the
// service survives being started before Postgres finishes booting.
func Connect(ctx context.Context, cfg config.Database) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// Zero values are treated as "not configured" rather than passed through:
	// a zero MaxConnLifetime would mark every connection expired the moment it
	// is created, and the pool would never hand one out.
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	poolCfg.MaxConnLifetime = orDefault(cfg.MaxConnLifetime, time.Hour)
	poolCfg.MaxConnIdleTime = orDefault(cfg.MaxConnIdleTime, 30*time.Minute)
	poolCfg.HealthCheckPeriod = time.Minute

	// Every timestamp crossing this boundary is UTC. Campaign timezones are
	// applied deliberately in the scheduling and presentation layers.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	var pingErr error
	for attempt := 0; attempt < 10; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		pingErr = pool.Ping(pingCtx)
		cancel()
		if pingErr == nil {
			return &DB{Pool: pool}, nil
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}

	pool.Close()
	return nil, fmt.Errorf("ping database: %w", pingErr)
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func (db *DB) Close() {
	if db != nil && db.Pool != nil {
		db.Pool.Close()
	}
}

func (db *DB) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.Pool.Ping(ctx)
}

// InTx runs fn inside a transaction, rolling back on error or panic.
func (db *DB) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a Postgres unique constraint error,
// optionally narrowed to a specific constraint name.
func IsUniqueViolation(err error, constraint ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	for _, name := range constraint {
		if pgErr.ConstraintName == name {
			return true
		}
	}
	return false
}

// IsForeignKeyViolation reports whether err is a foreign key violation.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// IsNoRows reports whether err means the query returned nothing.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
