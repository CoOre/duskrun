// Package sqlite is the metadata store and durable job queue, backed by
// modernc.org/sqlite (pure-Go, CGo-free — keeps the single static binary).
//
// Concurrency model (see the roadmap §03). SQLite allows exactly one writer at
// a time, and modernc can raise SQLITE_BUSY under concurrent access, so we split
// into two pools:
//
//   - write pool: MaxOpenConns(1) + _txlock=immediate. Every write tx therefore
//     takes the write lock up front (BEGIN IMMEDIATE), avoiding the read→write
//     upgrade deadlock, and serialising writers cleanly.
//   - read pool: many conns; WAL lets readers run concurrently with the writer.
//
// busy_timeout is set on both so brief contention retries instead of erroring.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/duskrun/duskrun/migrations"
)

// Store owns the read and write connection pools.
type Store struct {
	write *sql.DB // serialised writer (MaxOpenConns=1, immediate tx)
	read  *sql.DB // concurrent readers (WAL)
}

const busyTimeoutMS = 5000

// Open initialises WAL mode, applies migrations, and returns a ready Store.
// path is the SQLite file (e.g. "duskrun.db"); use ":memory:" only in tests
// with care — the two pools would see different in-memory DBs.
func Open(ctx context.Context, path string) (*Store, error) {
	// Shared pragmas. _txlock=immediate only matters on the write pool but is
	// harmless on reads.
	dsn := func(txImmediate bool) string {
		p := []string{
			"_pragma=journal_mode(WAL)",
			"_pragma=foreign_keys(1)",
			fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeoutMS),
			"_pragma=synchronous(NORMAL)",
		}
		q := "file:" + path + "?" + strings.Join(p, "&")
		if txImmediate {
			q += "&_txlock=immediate"
		}
		return q
	}

	write, err := sql.Open("sqlite", dsn(true))
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	write.SetMaxOpenConns(1) // the single-writer guarantee

	read, err := sql.Open("sqlite", dsn(false))
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}

	s := &Store{write: write, read: read}
	if err := s.write.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	var errs []error
	if s.read != nil {
		errs = append(errs, s.read.Close())
	}
	if s.write != nil {
		errs = append(errs, s.write.Close())
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// migrate applies embedded *.sql files in lexical order, tracking applied ones
// in a schema_migrations table. Each file runs in its own immediate tx.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.write.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`,
	); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var seen int
		if err := s.write.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name,
		).Scan(&seen); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if seen > 0 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.write.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, unixepoch())`, name,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
