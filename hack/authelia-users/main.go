// Command authelia-users writes the users database box writes: one login, one
// argon2id digest, the same file `box init` puts on the login's claim.
//
// It exists for the kind e2e. box writes that file on the host, and the e2e's
// host path is inside the test node, so `--paths-in-node` makes box skip the
// write and the script has to seed it. Writing it through internal/authelia is
// what keeps the e2e's file the same format as box's: a change to the hash or
// the row shape changes this file, and the login in the cluster is running the
// result. It is a test tool, like hack/render-grant, and nothing product-facing
// calls it.
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

func main() {
	login := flag.String("login", "", "the account to write")
	password := flag.String("password-file", "", "read the password from this file, or stdin with -")
	out := flag.String("out", "", "write the users database here")
	flag.Parse()

	if *login == "" || *password == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: authelia-users --login <name> --password-file <path|-> --out <path>")
		os.Exit(2)
	}
	if err := authelia.ValidateLogin(*login); err != nil {
		fmt.Fprintln(os.Stderr, "authelia-users:", err)
		os.Exit(2)
	}
	var (
		raw []byte
		err error
	)
	if *password == "-" {
		raw, err = os.ReadFile("/dev/stdin")
	} else {
		raw, err = os.ReadFile(*password)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "authelia-users: read the password:", err)
		os.Exit(1)
	}
	plain := string(raw)
	for len(plain) > 0 && (plain[len(plain)-1] == '\n' || plain[len(plain)-1] == '\r') {
		plain = plain[:len(plain)-1]
	}
	if plain == "" {
		fmt.Fprintln(os.Stderr, "authelia-users: the password is empty")
		os.Exit(1)
	}
	digest, err := authelia.HashPassword(plain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "authelia-users:", err)
		os.Exit(1)
	}
	db := authelia.NewUsers()
	db.Put(authelia.User{Login: *login, DisplayName: *login, Password: digest})
	if err := os.WriteFile(*out, db.Render(), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "authelia-users:", err)
		os.Exit(1)
	}
}
