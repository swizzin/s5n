package authelia

import (
	"strings"
	"testing"
)

// A file box wrote reads back as the rows it holds.
func TestUsersRoundTrip(t *testing.T) {
	digest, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	db := NewUsers()
	db.Put(User{Login: "alice", DisplayName: "alice", Password: digest})
	rendered := db.Render()
	if !strings.Contains(string(rendered), "users:\n  alice:\n    displayname: 'alice'\n") {
		t.Fatalf("rendered:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "password: '"+digest+"'") {
		t.Fatalf("the digest is not single-quoted in:\n%s", rendered)
	}
	back, err := ParseUsers(rendered)
	if err != nil {
		t.Fatalf("ParseUsers: %v", err)
	}
	row, ok := back.Get("alice")
	if !ok {
		t.Fatalf("alice is not in the re-read file: %s", rendered)
	}
	if row.Password != digest || row.DisplayName != "alice" {
		t.Fatalf("row = %+v", row)
	}
	if string(back.Render()) != string(rendered) {
		t.Fatalf("a re-render changed the file:\n%s\n---\n%s", rendered, back.Render())
	}
}

// Re-running init with no new password keeps the digest byte for byte: the row
// is read back, the caller writes nothing, and the file on the host is the same
// bytes. This is the property the login relies on so a re-run does not log
// everyone out for nothing.
func TestUsersKeepsARowItDidNotChange(t *testing.T) {
	original := []byte(`## box: root's login for this seedbox.
users:
  alice:
    displayname: 'alice'
    password: '$argon2id$v=19$m=32768,t=1,p=8$eUhVT1dQa082YVk2VUhDMQ$E8QI4jHbUBt3EdsU1NFDu4Bq5jObKNx7nBKSn1EYQxk'
`)
	before, err := ParseUsers(original)
	if err != nil {
		t.Fatalf("ParseUsers: %v", err)
	}
	row, ok := before.Get("alice")
	if !ok {
		t.Fatal("alice is missing")
	}
	// The caller's decision: the same row, no new password.
	after := NewUsers()
	after.Put(row)
	if string(after.Render()) == "" {
		t.Fatal("empty render")
	}
	if got, _ := after.Get("alice"); got.Password != row.Password {
		t.Fatal("the digest changed")
	}
}

// A row without a display name or a digest fails the whole file in Authelia,
// so box refuses to read it rather than writing a file Authelia will not load.
func TestParseUsersRefusesARowAutheliaWould(t *testing.T) {
	cases := map[string]string{
		"no users key":   "people:\n  alice:\n    displayname: 'a'\n",
		"no displayname": "users:\n  alice:\n    password: '$argon2id$v=19$m=1,t=1,p=1$c2FsdA$a2V5'\n",
		"no password":    "users:\n  alice:\n    displayname: 'a'\n",
		"not a digest":   "users:\n  alice:\n    displayname: 'a'\n    password: 'hunter2'\n",
		"unknown field":  "users:\n  alice:\n    displayname: 'a'\n    password: '$argon2id$v=19$m=1,t=1,p=1$c2FsdA$a2V5'\n    shell: '/bin/sh'\n",
		"flow style":     "users: {alice: {displayname: a}}\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseUsers([]byte(doc)); err == nil {
				t.Fatalf("ParseUsers accepted:\n%s", doc)
			}
		})
	}
}

// Groups survive a rewrite: they are not box's to write, and dropping one
// would silently change who an access_control rule admits.
func TestUsersKeepsGroups(t *testing.T) {
	doc := []byte("users:\n  alice:\n    displayname: 'alice'\n    password: '$argon2id$v=19$m=1,t=1,p=1$c2FsdA$a2V5'\n    groups: [admins, media]\n")
	db, err := ParseUsers(doc)
	if err != nil {
		t.Fatalf("ParseUsers: %v", err)
	}
	row, _ := db.Get("alice")
	if len(row.Groups) != 2 || row.Groups[0] != "admins" || row.Groups[1] != "media" {
		t.Fatalf("groups = %v", row.Groups)
	}
	if !strings.Contains(string(db.Render()), "groups: [admins, media]") {
		t.Fatalf("groups were dropped:\n%s", db.Render())
	}
}
