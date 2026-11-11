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

// The login's two questions, and the three ways box gets a password.
//
// The name is a grant answer like the namespace: it lives in the grant, is
// offered back on a re-run, and wins over the flag only in the sense that a
// flag in guided mode is the starting answer. The password is not an answer at
// all: it is used once, hashed, and never written to a file, a log or either of
// the two documents init edits.

// askLogin resolves the one account. In an interactive run it is a question
// the operator can change; unattended, the flag, then the grant, then the
// invoking user's own account name.
func (d *Decision) askLogin(opts Options, base, grantNow map[string]any) (string, error) {
	def := strings.TrimSpace(text(prefill(base, grantNow, "login")))
	if given := strings.TrimSpace(opts.Login); given != "" {
		def = given
	}
	if def == "" {
		def = strings.TrimSpace(opts.DefaultLogin)
	}
	if opts.Unattended {
		if def == "" {
			return "", errors.New("--login is needed: the account the login page admits")
		}
		if err := authelia.ValidateLogin(def); err != nil {
			return "", fmt.Errorf("--login %q: %w", def, err)
		}
		return def, nil
	}
	return d.Prompt.Input(Input{
		Key:      "login",
		Prompt:   "login name",
		Help:     "the account this box's login page admits; it is also its unix account name on most boxes",
		Default:  def,
		Validate: authelia.ValidateLogin,
	})
}

// askPassword is the password for one run: read from --password-file, asked
// twice on a terminal, or generated when nothing is supplied.
//
// The second return says whether this run supplies a password at all. An empty
// answer on a box that already has a row means "keep it": re-running init must
// not log everyone out because somebody pressed enter. An empty answer on a box
// that has none means box makes one, and the third return is that password,
// for the caller to print once.
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

// readPasswordFile reads one password from a file, or from stdin when the path
// is "-". A file with more than one line is refused rather than guessing which
// line was meant, and the value is never echoed back in an error.
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

// withApps is the values document with the app list init just decided applied
// to it: the chart's defaults, the user's file and the grant, and then the
// enabled flag for every app the catalog knows. It is what the login's rule
// generation reads, and it needs no schema check: the instances it walks are
// grant keys, which a values file cannot set at all.
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
