// Package sqlite owns the database handle and the shared query helpers.
//
// The whole application state lives in one SQLite file, so there is no server
// to provision, no credentials to rotate and a backup is a file copy.
//
// Repositories talk to SQLite through a thin compatibility layer instead of
// database/sql directly. The layer keeps three things the repository code was
// written against:
//
//   - $1 style placeholders, rewritten to SQLite's equivalent ?1 form;
//   - time.Time crossing the boundary in both directions, stored as a
//     fixed-width UTC string that sorts chronologically as text;
//   - now() and gen_random_uuid(), registered as SQLite functions so column
//     defaults and UPDATE statements read the same as they always did.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlitedriver "modernc.org/sqlite"

	"github.com/ayran/whatsapp-automation/internal/config"
)

// ErrNoRows is returned when a query that expected one row found none.
var ErrNoRows = sql.ErrNoRows

// DB wraps the database handle so callers depend on one type instead of
// database/sql internals spread across every package.
type DB struct {
	sql  *sql.DB
	path string
}

// Querier is satisfied by both *DB and *Tx, which lets repository methods run
// inside or outside a transaction without duplication.
type Querier interface {
	Exec(ctx context.Context, query string, args ...any) (Result, error)
	Query(ctx context.Context, query string, args ...any) (*Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) *Row
}

// Result reports how many rows a statement touched.
type Result struct{ rows int64 }

func (r Result) RowsAffected() int64 { return r.rows }

// executor is the part of database/sql that *sql.DB and *sql.Tx share.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Connect opens the database file, applies the pragmas the application relies
// on and verifies the handle works.
func Connect(ctx context.Context, cfg config.Database) (*DB, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, errors.New("database path is empty")
	}

	// A fresh checkout has no data directory; creating it here means the first
	// run works without a setup step.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	handle, err := sql.Open("sqlite", dsn(path, cfg))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite allows one writer at a time. WAL lets readers run alongside that
	// writer, and the pool stays small because extra connections only add lock
	// contention rather than throughput.
	maxConns := int(cfg.MaxConns)
	if maxConns <= 0 {
		maxConns = 8
	}
	handle.SetMaxOpenConns(maxConns)
	handle.SetMaxIdleConns(maxConns)
	handle.SetConnMaxLifetime(orDefault(cfg.MaxConnLifetime, time.Hour))
	handle.SetConnMaxIdleTime(orDefault(cfg.MaxConnIdleTime, 30*time.Minute))

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := handle.PingContext(pingCtx); err != nil {
		handle.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}

	db := &DB{sql: handle, path: path}

	// Fail loudly here rather than on the first foreign key violation: a
	// connection that silently dropped the pragma would corrupt referential
	// integrity instead of rejecting the write.
	var foreignKeys int
	if err := handle.QueryRowContext(pingCtx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		handle.Close()
		return nil, fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if foreignKeys != 1 {
		handle.Close()
		return nil, errors.New("foreign key enforcement is off")
	}

	return db, nil
}

// dsn builds the connection string. The pragmas run on every new connection in
// the pool, not just the first.
func dsn(path string, cfg config.Database) string {
	busy := cfg.BusyTimeout
	if busy <= 0 {
		busy = 10 * time.Second
	}

	params := url.Values{}
	// Readers no longer block the writer, and a reader no longer blocks itself
	// out of the database while the scheduler is committing.
	params.Add("_pragma", "journal_mode(WAL)")
	// Wait for a busy lock instead of returning SQLITE_BUSY immediately.
	params.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()))
	params.Add("_pragma", "foreign_keys(ON)")
	// WAL already survives a process crash; NORMAL only risks the last commits
	// on a machine-level power loss and removes an fsync per transaction.
	params.Add("_pragma", "synchronous(NORMAL)")
	// Let SQLite keep a page cache worth having (negative means KiB).
	params.Add("_pragma", "cache_size(-16000)")
	// Every transaction takes the write lock up front. Without this, a
	// transaction that reads before it writes can deadlock against another
	// writer and gets SQLITE_BUSY immediately, ignoring busy_timeout.
	params.Set("_txlock", "immediate")

	return "file:" + path + "?" + params.Encode()
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// Path reports the database file location, for logs and the health endpoint.
func (db *DB) Path() string { return db.path }

// SQL exposes the raw handle for the rare caller that needs database/sql.
func (db *DB) SQL() *sql.DB { return db.sql }

func (db *DB) Close() {
	if db != nil && db.sql != nil {
		db.sql.Close()
	}
}

func (db *DB) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.sql.PingContext(ctx)
}

func (db *DB) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	return exec(ctx, db.sql, query, args...)
}

func (db *DB) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	return query_(ctx, db.sql, query, args...)
}

func (db *DB) QueryRow(ctx context.Context, q string, args ...any) *Row {
	return queryRow(ctx, db.sql, q, args...)
}

// Tx is a transaction handle. It satisfies Querier, so a repository method can
// take either the pool or an open transaction.
type Tx struct{ tx *sql.Tx }

func (t *Tx) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	return exec(ctx, t.tx, query, args...)
}

func (t *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	return query_(ctx, t.tx, query, args...)
}

func (t *Tx) QueryRow(ctx context.Context, q string, args ...any) *Row {
	return queryRow(ctx, t.tx, q, args...)
}

// InTx runs fn inside a transaction, rolling back on error or panic.
func (db *DB) InTx(ctx context.Context, fn func(tx Querier) error) error {
	sqlTx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	tx := &Tx{tx: sqlTx}

	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := sqlTx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}

	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func exec(ctx context.Context, e executor, q string, args ...any) (Result, error) {
	bound, err := bindArgs(args)
	if err != nil {
		return Result{}, err
	}
	res, err := e.ExecContext(ctx, rewritePlaceholders(q), bound...)
	if err != nil {
		return Result{}, err
	}
	// SQLite always knows the count; a driver that does not is not an error
	// worth failing the write over.
	n, err := res.RowsAffected()
	if err != nil {
		return Result{}, nil
	}
	return Result{rows: n}, nil
}

func query_(ctx context.Context, e executor, q string, args ...any) (*Rows, error) {
	bound, err := bindArgs(args)
	if err != nil {
		return nil, err
	}
	rows, err := e.QueryContext(ctx, rewritePlaceholders(q), bound...)
	if err != nil {
		return nil, err
	}
	return &Rows{rows: rows}, nil
}

func queryRow(ctx context.Context, e executor, q string, args ...any) *Row {
	rows, err := query_(ctx, e, q, args...)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{rows: rows}
}

// Rows iterates a result set, translating stored values back into the types
// the domain models declare.
type Rows struct{ rows *sql.Rows }

func (r *Rows) Next() bool                 { return r.rows.Next() }
func (r *Rows) Err() error                 { return r.rows.Err() }
func (r *Rows) Close()                     { _ = r.rows.Close() }
func (r *Rows) Scan(dest ...any) error     { return scanRow(r.rows, dest) }
func (r *Rows) Columns() ([]string, error) { return r.rows.Columns() }

// Row is a single-row result. Scan reports ErrNoRows when the query matched
// nothing, matching database/sql's QueryRow contract.
type Row struct {
	rows *Rows
	err  error
}

func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()

	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	return r.rows.Err()
}

// IsNoRows reports whether err means the query returned nothing.
func IsNoRows(err error) bool { return errors.Is(err, ErrNoRows) }

// SQLite result codes for the constraint failures the application handles.
const (
	codeConstraintUnique     = 2067
	codeConstraintPrimaryKey = 1555
	codeConstraintForeignKey = 787
)

// uniqueConstraintMatches maps the schema's index names to the text SQLite puts
// in the error.
//
// SQLite names the index only when the index is over an expression; for plain
// columns it reports "table.column" instead. Callers should not have to know
// which kind each index is, so the translation lives here next to the schema.
var uniqueConstraintMatches = map[string][]string{
	"admins_email_key":                {"index 'admins_email_key'"},
	"campaigns_name_key":              {"index 'campaigns_name_key'"},
	"message_templates_name_key":      {"index 'message_templates_name_key'"},
	"tags_name_key":                   {"index 'tags_name_key'"},
	"messages_external_id_key":        {"messages.external_id"},
	"campaign_triggers_unique_active": {"campaign_triggers.normalized_keyword, campaign_triggers.match_mode"},
	"campaign_steps_order_key":        {"campaign_steps.campaign_id, campaign_steps.order_index"},
	"scheduled_messages_unique_step": {
		"scheduled_messages.enrollment_id, scheduled_messages.campaign_step_id, scheduled_messages.run_number",
	},
	"webhook_events_provider_dedupe_key": {"webhook_events.provider, webhook_events.dedupe_key"},
	"campaign_contacts_unique":           {"campaign_contacts.campaign_id, campaign_contacts.contact_id"},
	"message_template_versions_unique":   {"message_template_versions.template_id, message_template_versions.version"},
}

// IsUniqueViolation reports whether err is a unique constraint error,
// optionally narrowed to a specific index.
func IsUniqueViolation(err error, constraint ...string) bool {
	code, ok := resultCode(err)
	if !ok || (code != codeConstraintUnique && code != codeConstraintPrimaryKey) {
		return false
	}
	if len(constraint) == 0 {
		return true
	}

	msg := err.Error()
	for _, name := range constraint {
		for _, fragment := range uniqueConstraintMatches[name] {
			if strings.Contains(msg, fragment) {
				return true
			}
		}
		// Unmapped names still work when SQLite happens to report the index.
		if strings.Contains(msg, "index '"+name+"'") {
			return true
		}
	}
	return false
}

// IsForeignKeyViolation reports whether err is a foreign key violation.
func IsForeignKeyViolation(err error) bool {
	code, ok := resultCode(err)
	return ok && code == codeConstraintForeignKey
}

func resultCode(err error) (int, bool) {
	var sqliteErr *sqlitedriver.Error
	if !errors.As(err, &sqliteErr) {
		return 0, false
	}
	return sqliteErr.Code(), true
}
