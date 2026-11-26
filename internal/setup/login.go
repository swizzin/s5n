package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/catalog"
)

// The login's two questions: the name is a grant answer offered back on a re-run, the
// password used once and hashed, never written to a file, a log or either document.

// SeedboxNamespace is s5n-<login>, beside box's own s5n-system and s5n-gateway
// (docs/box.md, "Scopes: root and users"). Root's seedbox is not special.
func SeedboxNamespace(login string) string { return "s5n-" + login }

// NamespaceFor is SeedboxNamespace, refused when the login cannot make one: box's own
// namespaces are not a seedbox's, and the result must be a DNS-1123 label. Only a
// first run with no --namespace derives one; a re-run keeps the grant's.
func NamespaceFor(login string) (string, error) {
	ns := SeedboxNamespace(login)
	switch ns {
	case "s5n-system", "s5n-gateway":
		return "", fmt.Errorf("login %q: a seedbox's namespace is s5n-<login>, and %s is box's own", login, ns)
	}
	if err := validNamespace(ns); err != nil {
		return "", fmt.Errorf("login %q: a seedbox's namespace is s5n-<login>, and %q is not one: %w", login, ns, err)
	}
	return ns, nil
}

// ValidateLogin is Authelia's login shape plus a namespace box can create. Where the
// namespace comes from elsewhere (the grant, or --namespace), only the shape applies;
// see askLogin.
func ValidateLogin(login string) error {
	if err := authelia.ValidateLogin(login); err != nil {
		return err
	}
	_, err := NamespaceFor(login)
	return err
}

// askLogin resolves the one account: the flag, the grant, the OS account that owns
// the box's files (puid), then whoever ran the command. The login and the file owner
// are different things, so the default follows the files and the operator can say
// otherwise.
func (d *Decision) askLogin(opts Options, base, grantNow map[string]any, puid int, derives bool) (string, error) {
	// The login names the namespace only when this run derives one; otherwise it is
	// only Authelia's account.
	validate := authelia.ValidateLogin
	if derives {
		validate = ValidateLogin
	}
	def := strings.TrimSpace(text(prefill(base, grantNow, "login")))
	if given := strings.TrimSpace(opts.Login); given != "" {
		def = given
	}
	if def == "" && d.UserLookup != nil {
		if name, ok := d.UserLookup(puid); ok {
			def = strings.TrimSpace(name)
		}
	}
	if def == "" {
		def = strings.TrimSpace(opts.DefaultLogin)
	}
	// A flow running init on a host with no box holds this answer already: it is a
	// decision, not a starting answer, so the question is not asked again.
	if opts.Adopting && strings.TrimSpace(opts.Login) != "" {
		given := strings.TrimSpace(opts.Login)
		if err := validate(given); err != nil {
			return "", fmt.Errorf("--login %q: %w", given, err)
		}
		return given, nil
	}
	if opts.Unattended {
		if def == "" {
			return "", errors.New("--login is needed: the account the login page admits")
		}
		if err := validate(def); err != nil {
			return "", fmt.Errorf("--login %q: %w", def, err)
		}
		return def, nil
	}
	return d.Prompt.Input(Input{
		Key:      "login",
		Prompt:   "login name",
		Help:     "the account this box's login page admits; it does not have to be the account that owns the files",
		Default:  def,
		Validate: validate,
		Record:   "web login",
	})
}

// askPassword is the password for one run: --password-file, asked twice, or generated.
// An empty answer on a box with a row keeps it, so a re-run does not log everyone out;
// with none, box makes one and returns it to print once.
func (d *Decision) askPassword(opts Options, login string) (password string, newPassword bool, generated string, err error) {
	if opts.PasswordFile != "" {
		password, err := readPasswordFile(opts.PasswordFile)
		if err != nil {
			return "", false, "", err
		}
		return password, true, "", nil
	}

	have := false
	if d.HasLogin != nil {
		have = d.HasLogin(login)
	}
	if opts.Unattended {
		if have {
			return "", false, "", nil
		}
		password, err := authelia.GeneratePassword()
		if err != nil {
			return "", false, "", err
		}
		return password, true, password, nil
	}

	help := "empty lets box make one, and it is shown once at the end"
	if have {
		help = "empty keeps the password this box already has"
	}
	for attempt := 0; attempt < 3; attempt++ {
		first, err := d.Prompt.Secret(Secret{Key: "login.password", Prompt: "password", Help: help, Optional: true})
		if err != nil {
			return "", false, "", err
		}
		if first == "" {
			break
		}
		again, err := d.Prompt.Secret(Secret{Key: "login.password", Prompt: "password again", Optional: true})
		if err != nil {
			return "", false, "", err
		}
		if first != again {
			d.report().Line("the two passwords do not match; try again")
			continue
		}
		return first, true, "", nil
	}
	if have {
		return "", false, "", nil
	}
	password, err = authelia.GeneratePassword()
	if err != nil {
		return "", false, "", err
	}
	return password, true, password, nil
}

// readPasswordFile reads one password from a file, or stdin for "-". A file with more
// than one line is refused rather than guessed at, and the value never appears in an
// error.
func readPasswordFile(path string) (string, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the password: %w", err)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if strings.TrimSpace(value) == "" {
		return "", errors.New("--password-file is empty: give it one password, or leave the flag off and box makes one")
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", errors.New("--password-file holds more than one line: give it one password")
	}
	return value, nil
}

// withApps is the values document with init's app list applied: the enabled flag for
// every app the catalog knows. The login's rule generation reads it, and it needs no
// schema check: the instances it walks are grant keys a values file cannot set.
func withApps(base map[string]any, apps []string, cat *catalog.Catalog) map[string]any {
	entries, _ := base["apps"].(map[string]any)
	for _, name := range cat.Names() {
		entry, ok := entries[name].(map[string]any)
		if !ok {
			continue
		}
		entry["enabled"] = contains(apps, name)
	}
	return base
}
