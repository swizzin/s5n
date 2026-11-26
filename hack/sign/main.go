// Command sign signs a box release's checksums, generates the key pair that signs them, and
// checks a signature.
//
// box verifies a release's signature before it replaces itself as root, so the signing key
// is the one thing between a substituted release and a host. The private half is the
// release's secret (BOX_SIGNING_KEY) and the public half is compiled into box
// (internal/selfupdate/signing.pub). It uses internal/selfupdate's signing functions, so a
// signature made here is one box accepts by construction.
//
// A release is named in its signature as well as by its tag, so both signing and verifying
// take the version: one release's assets copied under another's tag are refused.
//
// Signing refuses a seed whose public half is not the committed key -- a release would
// publish and every box built from it would stop being able to update itself, discovered
// by a user -- and an empty signing.pub in its own words. It prints the public key and
// never the private one.
//
// Usage:
//
//	sign keygen <private-file>    write a new seed (0600, refusing an existing file) and
//	                              print the public key for signing.pub
//	sign public <private-file>    print the public key of a seed already kept
//	sign sign <version> <file>    sign <file> for <version>, writing <file>.sig
//	sign verify <version> <file> <key-file>
//	                              check <file>.sig against a public key
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/selfupdate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:], os.Stdout)
	case "public":
		err = publicKey(os.Args[2:], os.Stdout)
	case "sign":
		err = sign(os.Args[2:], os.Stdout)
	case "verify":
		err = verify(os.Args[2:], os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sign keygen <private-file> | sign public <private-file> | sign sign <version> <file> | sign verify <version> <file> <public-key-file>")
	os.Exit(2)
}

// keygen writes a new signing seed and prints its public half. The file is created
// exclusively: replacing a key would strand every box built with the old public half.
func keygen(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("keygen needs one path: the file to write the private seed to")
	}
	path := args[0]
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generating a key: %w", err)
	}

	// O_EXCL also refuses a symbolic link, which a seed must not be written through.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: refusing to overwrite a signing key", path)
		}
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fail := func(err error) error {
		file.Close()
		os.Remove(path)
		return err
	}
	if _, err := fmt.Fprintln(file, base64.StdEncoding.EncodeToString(private.Seed())); err != nil {
		return fail(fmt.Errorf("writing %s: %w", path, err))
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintln(out, base64.StdEncoding.EncodeToString(public))
	return nil
}

// publicKey prints the public half of a seed already kept, so signing.pub can be written
// again without generating a new key.
func publicKey(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("public needs one path: the file holding the private seed")
	}
	text, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("reading %s: %w", args[0], err)
	}
	seed, err := selfupdate.ParseSeed(string(text))
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	fmt.Fprintln(out, base64.StdEncoding.EncodeToString(seed.Public().(ed25519.PublicKey)))
	return nil
}

// signingPubPath is the committed public key, read from the repository root the command is
// run from (a test points it at a key it generated).
var signingPubPath = filepath.Join("internal", "selfupdate", "signing.pub")

// checkSigningKey refuses a seed whose public half is not what this release's boxes verify
// with, and an empty committed key in its own words. Naming both keys says whether the seed
// or the committed key is the mistake.
func checkSigningKey(seed ed25519.PrivateKey) error {
	committed, err := os.ReadFile(signingPubPath)
	if err != nil {
		return fmt.Errorf("reading %s (sign from the repository root): %w", signingPubPath, err)
	}
	text := strings.TrimSpace(string(committed))
	if text == "" {
		return fmt.Errorf("%s is empty: commit the public key first, then sign a release", signingPubPath)
	}
	key, err := selfupdate.ParsePublicKey(text)
	if err != nil {
		return fmt.Errorf("%s: %w", signingPubPath, err)
	}
	derived := seed.Public().(ed25519.PublicKey)
	if !derived.Equal(key) {
		derivedText := base64.StdEncoding.EncodeToString(derived)
		return fmt.Errorf("BOX_SIGNING_KEY signs as %s, but %s is %s: a release signed with this seed is one no box built from this commit can verify. Sign with the seed %s belongs to, or commit %s as the new public key",
			derivedText, signingPubPath, text, signingPubPath, derivedText)
	}
	return nil
}

// sign writes <file>.sig: the base64 signature over the release's tag and the file's bytes.
// The seed comes from the environment so it never appears in a process list.
func sign(args []string, out io.Writer) error {
	if len(args) != 2 {
		return errors.New("sign needs two arguments: the release version and the file to sign, whose signature is written to <file>.sig")
	}
	version, path := args[0], args[1]
	seedText := os.Getenv("BOX_SIGNING_KEY")
	if strings.TrimSpace(seedText) == "" {
		return errors.New("BOX_SIGNING_KEY is empty: box verifies every release's signature, so a release must not publish without one")
	}
	seed, err := selfupdate.ParseSeed(seedText)
	if err != nil {
		return fmt.Errorf("BOX_SIGNING_KEY: %w", err)
	}
	if err := checkSigningKey(seed); err != nil {
		return err
	}
	sums, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	sig := selfupdate.SignSums(seed, version, sums)
	if err := os.WriteFile(path+".sig", sig, 0o644); err != nil {
		return fmt.Errorf("writing %s.sig: %w", path, err)
	}
	fmt.Fprintf(out, "signed %s for box-v%s\n", filepath.Base(path), version)
	return nil
}

// verify checks a signature the way box will, for the version it will check it as.
func verify(args []string, out io.Writer) error {
	if len(args) != 3 {
		return errors.New("verify needs three arguments: the release version, the signed file and the public key")
	}
	version, file, keyFile := args[0], args[1], args[2]
	keyText, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", keyFile, err)
	}
	key, err := selfupdate.ParsePublicKey(string(keyText))
	if err != nil {
		return fmt.Errorf("%s: %w", keyFile, err)
	}
	signed, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	signature, err := os.ReadFile(file + ".sig")
	if err != nil {
		return fmt.Errorf("reading %s.sig: %w", file, err)
	}
	if err := selfupdate.VerifySums(key, version, signed, signature); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	fmt.Fprintf(out, "%s: signature ok for box-v%s\n", filepath.Base(file), version)
	return nil
}
