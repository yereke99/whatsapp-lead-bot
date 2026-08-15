package sqlite

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

// Migrate applies every pending migration found in fsys.
//
// Each migration runs in its own transaction together with the row that
// records it, so a failure leaves the schema exactly where it was. SQLite
// permits one writer at a time, and every transaction here takes the write
// lock on BEGIN, so two processes starting at once serialise rather than race.
func (db *DB) Migrate(ctx context.Context, fsys fs.FS, log *slog.Logger) error {
	migrationsList, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if len(migrationsList) == 0 {
		return fmt.Errorf("no migrations found")
	}

	_, err = db.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			checksum   TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (now())
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]string{}
	rows, err := db.sql.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate applied migrations: %w", err)
	}

	for _, m := range migrationsList {
		if checksum, ok := applied[m.version]; ok {
			if checksum != m.checksum {
				return fmt.Errorf("migration %d (%s) was modified after being applied; create a new migration instead", m.version, m.name)
			}
			continue
		}

		start := time.Now()
		if err := db.applyMigration(ctx, m); err != nil {
			return err
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

func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES (?1, ?2, ?3)",
		m.version, m.name, m.checksum,
	); err != nil {
		return fmt.Errorf("record migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.version, err)
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
