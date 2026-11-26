package plex

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// dbColumns are the path columns; only these are read, since `SELECT *` would move
// megabytes across the shell's stdout.
var dbColumns = []struct{ table, column string }{
	{"section_locations", "root_path"},
	{"media_parts", "file"},
	{"media_streams", "url"},
	{"metadata_items", "guid"},
}

// readDB runs every SELECT in one script, each row tagged with its table; the
// table list tells a missing table (never scanned) from an empty one.
func readDB(ctx context.Context, in rewrite.Input, file string) (rows []dbRow, missing []string, err error) {
	var script strings.Builder
	script.WriteString("SELECT '@tables', name FROM sqlite_master WHERE type = 'table' AND name IN (")
	for i, c := range dbColumns {
		if i > 0 {
			script.WriteString(", ")
		}
		script.WriteString(sqlQuote(c.table))
	}
	script.WriteString(");\n")
	for _, c := range dbColumns {
		fmt.Fprintf(&script, "SELECT %s, id, %s FROM %s;\n", sqlQuote("@"+c.table), c.column, c.table)
	}
	script.WriteString("SELECT '@end', 1;\n")

	out, runErr := in.Run(ctx, in.App, []string{sqliteBin, containerDBPath(in.Dir)}, []byte(script.String()))

	tables := map[string]bool{}
	done := false
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		// The value is the rest of the line, so a path holding "|" survives; only a
		// newline could not, and no path on this filesystem has one.
		parts := strings.SplitN(line, "|", 3)
		switch {
		case parts[0] == "@tables":
			if len(parts) >= 2 && parts[1] != "" {
				tables[parts[1]] = true
			}
			continue
		case parts[0] == "@end":
			done = true
			continue
		case len(parts) != 3:
			continue
		}
		id, perr := strconv.ParseInt(parts[1], 10, 64)
		if perr != nil {
			return nil, nil, fmt.Errorf("plex: %s: `%s` answered %q, which is not an id: %w", file, sqliteBin, line, perr)
		}
		table := strings.TrimPrefix(parts[0], "@")
		rows = append(rows, dbRow{table: table, column: columnOf(table), id: id, value: parts[2]})
	}
	if !done {
		// Without the last marker the read is incomplete -- a damaged database or a
		// locked file -- and "nothing to change" would be a lie.
		return nil, nil, fmt.Errorf("plex: querying %s with `%s` did not finish: %v", file, sqliteBin, runErr)
	}
	// A missing table's statement prints an error and the shell carries on, so the
	// error alone is not fatal once the script has run.
	for _, c := range dbColumns {
		if !tables[c.table] {
			missing = append(missing, c.table)
		}
	}
	return rows, missing, nil
}

// columnOf is a table's one column, so a row knows what it is.
func columnOf(table string) string {
	for _, c := range dbColumns {
		if c.table == table {
			return c.column
		}
	}
	return ""
}

// writeDB sends the UPDATEs in one transaction. `-bail` stops at the first error
// and rolls back on exit, so the database is wholly rewritten or untouched.
func writeDB(ctx context.Context, in rewrite.Input, rows []dbRow) error {
	var script strings.Builder
	script.WriteString("BEGIN;\n")
	for _, r := range rows {
		fmt.Fprintf(&script, "UPDATE %s SET %s = %s WHERE id = %d;\n",
			r.table, r.column, sqlQuote(r.value), r.id)
	}
	script.WriteString("COMMIT;\n")
	if _, err := in.Run(ctx, in.App, []string{sqliteBin, "-bail", containerDBPath(in.Dir)}, []byte(script.String())); err != nil {
		return fmt.Errorf("plex: write the library database with `%s`: %w", sqliteBin, err)
	}
	return nil
}

// containerDBPath is the database's name inside the image, with the staged config
// at /config.
func containerDBPath(dir string) string {
	support, err := supportDirIn(dir)
	if err != nil {
		// run already refused this configuration; keep the path sane anyway.
		return path.Join("/config", dbRel)
	}
	return path.Join("/config", support, dbRel)
}

// sqlQuote is a SQL string literal, single quotes doubled; the value is a path
// from the app's own file, never operator input.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
