package autobrr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// usersTable is autobrr's account table (0_base_schema.sql:1-9 at v1.90.0);
// nothing else refers to a user row, so a rename keeping the id moves nothing.
const usersTable = `
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL,
    password TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (username)
);
`

// withUsers is the fixture with the account table and its rows.
func withUsers(rows string) string { return dbFixture + usersTable + rows }

// accountRows is the users table as the file has it now.
func accountRows(t *testing.T, dir string) map[int64]string {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, username FROM users`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTheAccountsNameBecomesBoxs: the init signs in as rewrite.WiringAccount, so
// the one row is renamed in place, keeping its id, and the init then writes the
// release Secret's password onto it.
func TestTheAccountsNameBecomesBoxs(t *testing.T) {
	ctx := context.Background()
	in := setup(t, withUsers(`INSERT INTO users (id, username, password) VALUES (7, 'source-login', 'a-hash');`))
	before := treeHash(t, in.Dir)

	rep, err := (rewriter{}).Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := []rewrite.Account{{From: "source-login", To: "s5n"}}
	if len(rep.Accounts) != 1 || rep.Accounts[0] != want[0] {
		t.Fatalf("Check reported accounts %+v, want %+v", rep.Accounts, want)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check wrote something: %s -> %s", before, after)
	}

	wrote, err := (rewriter{}).Write(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote.Accounts) != 1 || wrote.Accounts[0] != want[0] {
		t.Errorf("Write reported accounts %+v, want %+v: its report is held to the check before it", wrote.Accounts, want)
	}
	if got := accountRows(t, in.Dir); len(got) != 1 || got[7] != "s5n" {
		t.Errorf("the account table is %v, want id 7 named s5n: the row keeps its id", got)
	}

	// A second pass has nothing to rename.
	again, err := (rewriter{}).Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Accounts) != 0 {
		t.Errorf("after Write, Check still renames %+v", again.Accounts)
	}
}

// TestAnAccountAlreadyBoxsIsNotRenamed: reporting a rename would be a change that
// changes nothing.
func TestAnAccountAlreadyBoxsIsNotRenamed(t *testing.T) {
	in := setup(t, withUsers(`INSERT INTO users (id, username, password) VALUES (1, 's5n', 'a-hash');`))
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Accounts) != 0 {
		t.Errorf("an account already named s5n was reported as %+v", rep.Accounts)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := accountRows(t, in.Dir); len(got) != 1 || got[1] != "s5n" {
		t.Errorf("the account table is %v, want the one row unchanged", got)
	}
}

// TestNoAccountIsNothingToDo: a source that never made an account is the table
// with no rows (or a database from a version that had no table yet). The init
// container creates the account on the first start, so there is nothing here to
// rename and nothing to refuse.
func TestNoAccountIsNothingToDo(t *testing.T) {
	ctx := context.Background()
	for name, sql := range map[string]string{
		"an empty table":  withUsers(""),
		"no table at all": dbFixture,
	} {
		in := setup(t, sql)
		rep, err := (rewriter{}).Check(ctx, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rep.Accounts) != 0 {
			t.Errorf("%s: reported %+v, want nothing to rename", name, rep.Accounts)
		}
	}
}

// TestTwoAccountsAreRefused: autobrr holds one account, so a database with two
// is not a source this migration understands. It stops naming the count and the
// app, rather than pick one to keep or delete the other.
func TestTwoAccountsAreRefused(t *testing.T) {
	in := setup(t, withUsers(`
INSERT INTO users (id, username, password) VALUES (1, 'source-login', 'a-hash');
INSERT INTO users (id, username, password) VALUES (2, 'another-login', 'another-hash');`))
	_, err := rewriter{}.Check(context.Background(), in)
	if err == nil {
		t.Fatal("a database with two accounts was accepted")
	}
	for _, want := range []string{"2", "autobrr", "users"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	// The refusal wrote nothing: both rows are as the source left them.
	if got := accountRows(t, in.Dir); len(got) != 2 || got[1] != "source-login" || got[2] != "another-login" {
		t.Errorf("the refused run changed the accounts to %v", got)
	}
	// And the check itself is the only thing that ran: no write pass is
	// reached by apply with a refused report.
	if _, err := os.Stat(filepath.Join(in.Dir, dbName)); err != nil {
		t.Fatal(err)
	}
}
