package qui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// userTable is qui's own account table at v1.35.0
// (internal/database/migrations/001_initial_schema.sql:2-9): exactly one row,
// and the row's id is 1 by the schema's own CHECK. api_keys and sessions carry
// no user column, and the two tables that do carry a user_id -- dashboard
// settings and filter views -- refer to it by that value (026 and 085), so a
// rename in place keeps every one of them pointing at the one account.
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

// withUser is the fixture set up with qui's account table in it, holding rows.
func withUser(t *testing.T, rows string) rewrite.Input {
	t.Helper()
	in := setup(t)
	exec(t, in.Dir, userTable+rows)
	return in
}

// userRows is the account table and the row that points at it, as the file has
// them now.
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

// TestTheAccountsNameBecomesBoxs: qui allows one account, and the chart's init
// container signs in as box's own (rewrite.WiringAccount) on every start. The
// source's account is the one row there is, so the migration renames it in
// place, and the rows that refer to it by id keep referring to it: the app's own
// settings stay the migrated user's, and the init writes the release Secret's
// password onto the account as on any start.
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

// TestAnAccountAlreadyBoxsIsNotRenamed: a source that already signs in as box's
// account needs nothing.
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

// TestNoAccountIsNothingToDo: a source whose user table is empty (or whose
// database predates the table) is one the init container gives an account on the
// first start, so there is nothing here to rename.
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

// TestTwoAccountsAreRefused: qui's schema allows one row and the app refuses a
// second, so a database holding two is not one this migration understands. It
// stops naming the count and the app rather than pick one.
func TestTwoAccountsAreRefused(t *testing.T) {
	in := withUser(t, "")
	// The CHECK on the id is the app's own guard; this fixture drops it to
	// reproduce a database that has two rows anyway, which is what a hand-edited
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
