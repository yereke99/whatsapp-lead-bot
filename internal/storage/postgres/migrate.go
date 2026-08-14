package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type migration struct {
	version  int
	name     string
	filename string
	body     string
	checksum string
}

// Migrate applies every pending migration found in fsys inside a single
// advisory lock so concurrent replicas cannot race each other on boot.
func (db *DB) Migrate(ctx context.Context, fsys fs.FS, log *slog.Logger) error {
	migrationsList, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if len(migrationsList) == 0 {
		return fmt.Errorf("no migrations found")
	}

	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	// Arbitrary but stable key; every replica of this service uses the same one.
	const lockKey int64 = 8123401928374651
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey)
	}()

	_, err = conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     integer PRIMARY KEY,
			name        text        NOT NULL,
			checksum    text        NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]string{}
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate applied migrations: %w", err)
	}

	for _, m := range migrationsList {
		if sum, ok := applied[m.version]; ok {
			if sum != m.checksum {
				return fmt.Errorf("migration %d (%s) was modified after being applied; create a new migration instead", m.version, m.name)
			}
			continue
		}

		start := time.Now()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, m.body); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			m.version, m.name, m.checksum,
		); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}

		if log != nil {
			log.Info("migration applied",
				slog.Int("version", m.version),
				slog.String("name", m.name),
				slog.Duration("took", time.Since(start)))
		}
	}

	return nil
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("scan migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := map[int]string{}

	for _, entry := range entries {
		base := path.Base(entry)
		parts := strings.SplitN(strings.TrimSuffix(base, ".sql"), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("migration %q must be named <version>_<name>.sql", base)
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("migration %q has a non-numeric version: %w", base, err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d (%s and %s)", version, prev, base)
		}
		seen[version] = base

		body, err := fs.ReadFile(fsys, entry)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", base, err)
		}
		sum := sha256.Sum256(body)

		out = append(out, migration{
			version:  version,
			name:     parts[1],
			filename: base,
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
