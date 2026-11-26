// Command authelia-users writes the users database box writes: one argon2id digest per
// login, the same file `box init` puts on the login's claim.
//
// It exists for the kind e2e: box writes that file on the host, and the e2e's host path is
// inside the test node, so --paths-in-node makes box skip the write. Writing it through
// internal/authelia keeps the format identical, so a hash or row-shape change reaches the
// login in the cluster. A test tool; nothing product-facing calls it.
//
// --login and --password-file are given once per account, in that order, so the e2e can
// seed the second account the authorization policy must refuse.
//
// Usage:
//
//	go run ./hack/authelia-users --login alice --password-file /path --out /path/users_database.yml
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/authelia"
)

// list is one repeatable flag: given twice, it holds two entries in the order
// they were given.
type list []string

func (l *list) String() string { return fmt.Sprint(*l) }

func (l *list) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func main() {
	var logins, passwords list
	flag.Var(&logins, "login", "the account to write; repeat for another account")
	flag.Var(&passwords, "password-file", "that account's password file, or - for stdin")
	out := flag.String("out", "", "write the users database here")
	flag.Parse()

	if len(logins) == 0 || len(logins) != len(passwords) || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: authelia-users --login <name> --password-file <path|-> [--login <name> --password-file <path|->] --out <path>")
		os.Exit(2)
	}
	db := authelia.NewUsers()
	for i, login := range logins {
		if err := authelia.ValidateLogin(login); err != nil {
			fmt.Fprintln(os.Stderr, "authelia-users:", err)
			os.Exit(2)
		}
		plain, err := readPassword(passwords[i])
		if err != nil {
			fmt.Fprintln(os.Stderr, "authelia-users:", err)
			os.Exit(1)
		}
		digest, err := authelia.HashPassword(plain)
		if err != nil {
			fmt.Fprintln(os.Stderr, "authelia-users:", err)
			os.Exit(1)
		}
		db.Put(authelia.User{Login: login, DisplayName: login, Password: digest})
	}
	if err := os.WriteFile(*out, db.Render(), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "authelia-users:", err)
		os.Exit(1)
	}
}

// readPassword reads one account's password and drops the newline a file or a
// pipe leaves behind, so the digest is over exactly what the e2e will send.
func readPassword(path string) (string, error) {
	if path == "-" {
		path = "/dev/stdin"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the password: %w", err)
	}
	plain := string(raw)
	for len(plain) > 0 && (plain[len(plain)-1] == '\n' || plain[len(plain)-1] == '\r') {
		plain = plain[:len(plain)-1]
	}
	if plain == "" {
		return "", fmt.Errorf("the password is empty")
	}
	return plain, nil
}
