// Package store is the SQLite persistence layer.
//
// SQLite (pure-Go, no cgo) is deliberate: Floorline ships as a single binary
// plus a single database file, with no daemon to run alongside it.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Open connects to the database and applies the schema.
//
// WAL keeps the pollers writing while the bot reads; a busy timeout absorbs the
// brief contention that produces instead of surfacing SQLITE_BUSY.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single writer avoids lock churn; SQLite serialises writes anyway.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for the rare ad-hoc query.
func (s *Store) DB() *sql.DB { return s.db }

// tx runs fn inside a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// ---- key/value ----------------------------------------------------------

// GetKV reads a value, returning "" when the key is absent.
func (s *Store) GetKV(ctx context.Context, k string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetKV writes a value.
func (s *Store) SetKV(ctx context.Context, k, v string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}
