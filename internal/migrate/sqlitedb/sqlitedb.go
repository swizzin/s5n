// Package sqlitedb opens the migrated apps' SQLite databases, the one place the
// driver is chosen (pure Go, so box stays static with no cgo). Plex is not here:
// its library uses ICU tokenizers only Plex's own SQLite has.
package sqlitedb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// OpenReadOnly opens an existing database with no way to change it: a read that
// could write (a WAL checkpoint, a journal recovery) would make the copy differ
// from what was checked.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	return open(ctx, path, "ro")
}

// OpenReadWrite opens an existing database for the write pass, never creating
// one: no database where one was expected means the wrong directory.
func OpenReadWrite(ctx context.Context, path string) (*sql.DB, error) {
	return open(ctx, path, "rw")
}

func open(ctx context.Context, path, mode string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	q := url.Values{}
	q.Set("mode", mode)
	q.Add("_pragma", "busy_timeout(5000)")
	if mode == "rw" {
		q.Add("_pragma", "foreign_keys(0)")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: a rewrite is one transaction, and a second would only add
	// a writer for the busy timeout.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// IntegrityCheck runs SQLite's own check, returning nil when the database is
// sound: a copy taken from a running app can be torn.
func IntegrityCheck(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s != "ok" {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}
