// Package sqlitedb opens the SQLite databases the migrated apps keep, for the
// rewriters and for discovery. It is the one place the driver is chosen: a
// pure Go one, so box stays a static binary with no cgo.
//
// It is not for Plex. Plex's library database uses ICU tokenizers compiled
// into Plex's own SQLite, which no stock build has, so Plex is only ever
// written through the `Plex SQLite` shipped in the pinned Plex image.
package sqlitedb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// OpenReadOnly opens an existing database without any way to change it: the
// check pass and discovery read a copy, and a read that could write (a WAL
// checkpoint, a journal recovery) would make the copy differ from what was
// checked.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	return open(ctx, path, "ro")
}

// OpenReadWrite opens an existing database for the write pass. It never
// creates one: a rewriter that finds no database where it expected one has
// read the wrong directory.
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
	// One connection: a rewrite is one transaction, and a second connection
	// would only add a writer for the busy timeout to wait on.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// IntegrityCheck runs SQLite's own check and returns its findings, nil when
// the database is sound. A copy taken from a running app can be torn; this is
// what tells apply so before it rewrites one.
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
