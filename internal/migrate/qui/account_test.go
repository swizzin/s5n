package qui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// userTable is qui's account table at v1.35.0 (001_initial_schema.sql:2-9): one
// row, id 1 by the schema's CHECK. dashboard settings and filter views carry its
// user_id (026, 085), so a rename in place keeps them pointing at the account.
const userTable = `
CREATE TABLE user (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE dashboard_settings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL UNIQUE,
    section_visibility TEXT NOT NULL DEFAULT '[]'
);
`

// withUser is the fixture with the account table and its rows.
func withUser(t *testing.T, rows string) rewrite.Input {
	t.Helper()
	in := setup(t)
	exec(t, in.Dir, userTable+rows)
	return in
}

// userRows is the account table and the row pointing at it.
func userRows(t *testing.T, dir string) (string, []int64) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlitedb.OpenReadOnly(ctx, filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRowContext(ctx, `SELECT username FROM user ORDER BY id LIMIT 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT user_id FROM dashboard_settings ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return name, ids
}

// TestTheAccountsNameBecomesBoxs: the init signs in as rewrite.WiringAccount, so
// the one row is renamed in place and rows referring to it by id keep pointing at
// it; the init then writes the release Secret's password onto it.
func TestTheAccountsNameBecomesBoxs(t *testing.T) {
	ctx := context.Background()
	in := withUser(t, `INSERT INTO user (id, username, password_hash) VALUES (1, 'source-login', 'a-hash');
		INSERT INTO dashboard_settings (id, user_id) VALUES (1, 1);`)
	before := treeHash(t, in.Dir)

	rep, err := (rewriter{}).Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := rewrite.Account{From: "source-login", To: "s5n"}
	if len(rep.Accounts) != 1 || rep.Accounts[0] != want {
		t.Fatalf("Check reported accounts %+v, want %+v", rep.Accounts, want)
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check wrote something: %s -> %s", before, after)
	}

	wrote, err := (rewriter{}).Write(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote.Accounts) != 1 || wrote.Accounts[0] != want {
		t.Errorf("Write reported accounts %+v, want %+v: its report is held to the check before it", wrote.Accounts, want)
	}
	name, refs := userRows(t, in.Dir)
	if name != "s5n" {
		t.Errorf("the account is named %q, want s5n", name)
	}
	if len(refs) != 1 || refs[0] != 1 {
		t.Errorf("the dashboard settings refer to user %v, want the same row 1", refs)
	}

	again, err := (rewriter{}).Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Accounts) != 0 {
		t.Errorf("after Write, Check still renames %+v", again.Accounts)
	}
}

// TestAnAccountAlreadyBoxsIsNotRenamed: nothing to do.
func TestAnAccountAlreadyBoxsIsNotRenamed(t *testing.T) {
	in := withUser(t, `INSERT INTO user (id, username, password_hash) VALUES (1, 's5n', 'a-hash');`)
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
	if name, _ := userRows(t, in.Dir); name != "s5n" {
		t.Errorf("the account is named %q, want the one row unchanged", name)
	}
}

// TestNoAccountIsNothingToDo: the init gives an empty or absent table its account.
func TestNoAccountIsNothingToDo(t *testing.T) {
	ctx := context.Background()
	for name, sql := range map[string]string{
		"an empty table":  userTable,
		"no table at all": "",
	} {
		in := setup(t)
		if sql != "" {
			exec(t, in.Dir, sql)
		}
		rep, err := (rewriter{}).Check(ctx, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rep.Accounts) != 0 {
			t.Errorf("%s: reported %+v, want nothing to rename", name, rep.Accounts)
		}
	}
}

// TestTwoAccountsAreRefused: a two-row database is not one this migration
// understands, so it stops naming the count rather than pick one.
func TestTwoAccountsAreRefused(t *testing.T) {
	in := withUser(t, "")
	// The id CHECK is the app's guard; dropping it reproduces what a hand-edited
	// file or an older schema can be.
	exec(t, in.Dir, `DROP TABLE user;
		CREATE TABLE user (id INTEGER PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL);
		INSERT INTO user (id, username, password_hash) VALUES (1, 'source-login', 'a-hash'), (2, 'another-login', 'another-hash');`)
	_, err := rewriter{}.Check(context.Background(), in)
	if err == nil {
		t.Fatal("a database with two accounts was accepted")
	}
	for _, want := range []string{"2", "qui", "user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if name, _ := userRows(t, in.Dir); name != "source-login" {
		t.Errorf("the refused run renamed the account to %q", name)
	}
}
