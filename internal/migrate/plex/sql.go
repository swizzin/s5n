package plex

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// dbColumns are the library columns that hold a path. Only these are read: a
// `SELECT *` would move megabytes of metadata across the shell's stdout for
// values this package never looks at.
var dbColumns = []struct{ table, column string }{
	{"section_locations", "root_path"},
	{"media_parts", "file"},
	{"media_streams", "url"},
	{"metadata_items", "guid"},
}

// readDB runs every SELECT in one script, so a Check is one execution of the
// shell rather than four. Each row is tagged with the table it came from; the
// table list in front of them is what tells a missing table (an install Plex
// never scanned) from an empty one.
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
		// The value is the rest of the line, so a path holding the separator
		// survives; only a newline inside a value could not, and no filesystem
		// path this runs on has one.
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
		// Without the last marker the read is incomplete: the shell exits on an
		// error it cannot recover from (a damaged database, a locked file) and
		// "nothing to change" would be a lie.
		return nil, nil, fmt.Errorf("plex: querying %s with `%s` did not finish: %v", file, sqliteBin, runErr)
	}
	// A statement for a table this install does not have prints an error and
	// the shell carries on, which is why the error alone is not fatal once the
	// whole script has run.
	for _, c := range dbColumns {
		if !tables[c.table] {
			missing = append(missing, c.table)
		}
	}
	return rows, missing, nil
}

// columnOf is the one column this package reads from a table, so a row knows
// what it is without repeating the name on every line of output.
func columnOf(table string) string {
	for _, c := range dbColumns {
		if c.table == table {
			return c.column
		}
	}
	return ""
}

// writeDB sends the UPDATEs as one script inside one transaction. `-bail` stops
// at the first error and the transaction rolls back on exit, so a database is
// either wholly rewritten or untouched -- a half-rewritten library would point
// half its rows at directories that do not exist.
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

// containerDBPath is the database's name inside the image, where the staged
// config is mounted at /config.
func containerDBPath(dir string) string {
	support, err := supportDirIn(dir)
	if err != nil {
		// run has already refused this configuration; keep the path sane for a
		// caller that ignored the error.
		return path.Join("/config", dbRel)
	}
	return path.Join("/config", support, dbRel)
}

// sqlQuote is a SQL string literal: single quotes doubled. The value is a path
// read out of the app's own file, never something an operator typed here, and
// doubling is the whole of SQLite's escaping for a literal.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
