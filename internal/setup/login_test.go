package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/authelia"
)

// The login's password has three sources and one rule: it is used once, it is
// never written to either file, and an empty answer never silently replaces a
// password that is already working.

// TestPasswordFileReachesTheLogin: --password-file is the unattended form, and
// the password reaches the login and nothing else.
func TestPasswordFileReachesTheLogin(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}
	opts.PasswordFile = filepath.Join(h.dir, "password")
	if err := os.WriteFile(opts.PasswordFile, []byte("a passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if len(h.logins) != 1 {
		t.Fatalf("the login hook ran %d times", len(h.logins))
	}
	login := h.logins[0]
	if login.Password != "a passphrase" || !login.NewPassword {
		t.Fatalf("the login got %q (new %v)", login.Password, login.NewPassword)
	}
	if login.Name != "alice" || login.Host != "box.example.com" {
		t.Fatalf("the login got %+v", login)
	}
	if res.GeneratedPassword != "" {
		t.Fatal("box reported a generated password when one was supplied")
	}
	for _, path := range []string{h.grantPath, h.valuesPath} {
		if h.exists(path) && strings.Contains(h.read(path), "a passphrase") {
			t.Fatalf("%s holds the password", path)
		}
	}
	if strings.Contains(h.out.String(), "a passphrase") {
		t.Fatal("init printed the password")
	}
}

// TestGeneratedPasswordIsReportedOnce: no answer means box makes one, reports
// it in the result for the caller's card, and writes it nowhere.
func TestGeneratedPasswordIsReportedOnce(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if !res.NewPassword || res.GeneratedPassword == "" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.GeneratedPassword) != authelia.GeneratedPasswordLength {
		t.Fatalf("generated password is %d characters", len(res.GeneratedPassword))
	}
	if h.logins[0].Password != res.GeneratedPassword {
		t.Fatal("the login did not get the password box reported")
	}
	for _, path := range []string{h.grantPath, h.valuesPath} {
		if h.exists(path) && strings.Contains(h.read(path), res.GeneratedPassword) {
			t.Fatalf("%s holds the generated password", path)
		}
	}
	if strings.Contains(h.out.String(), res.GeneratedPassword) {
		t.Fatal("init printed the password itself: only the caller's card may")
	}
}

// TestExistingPasswordIsKept: a re-run with no new password keeps the row, so
// pressing enter on a box that already has one does not rotate it.
func TestExistingPasswordIsKept(t *testing.T) {
	h := newHarness(t)
	h.decision.HasLogin = func(string) bool { return true }
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if res.NewPassword || res.GeneratedPassword != "" {
		t.Fatalf("result = %+v, want no new password", res)
	}
	if h.logins[0].NewPassword || h.logins[0].Password != "" {
		t.Fatalf("the login got %+v, want to keep what is there", h.logins[0])
	}
}

// TestPasswordFromStdin: `--password-file -` reads the standard input, which
// is how a pipeline supplies it without a temp file.
func TestPasswordFromStdin(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}
	opts.PasswordFile = "-"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("from-a-pipe\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = saved; r.Close() }()

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if h.logins[0].Password != "from-a-pipe" {
		t.Fatalf("the login got %q", h.logins[0].Password)
	}
}

// TestPasswordFileRefusesWhatItCannotUse: an empty file and a two-line file
// are refused by name before anything is written, because guessing which line
// was meant is how a box ends up with a password nobody knows.
func TestPasswordFileRefusesWhatItCannotUse(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"two lines": "one\ntwo\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			opts := h.unattended()
			opts.SetTransient = map[string]string{"apps.beta.token": "tok"}
			opts.PasswordFile = filepath.Join(h.dir, "password")
			if err := os.WriteFile(opts.PasswordFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := h.run(opts); err == nil {
				t.Fatal("init accepted a password file it cannot read one password from")
			}
			if len(h.logins) != 0 || len(h.applied) != 0 {
				t.Fatal("a refused run reached the hand-over")
			}
		})
	}
}

// TestLoginCarriesTheEnabledApps: the rules are generated from this document,
// so the app list init just decided has to be in it.
func TestLoginCarriesTheEnabledApps(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}
	opts.Apps = []string{"alpha", "beta"}
	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	var names []string
	for _, inst := range authelia.InstancesFromValues(h.logins[0].Values) {
		names = append(names, inst.Name)
	}
	if strings.Join(names, ",") != "alpha,beta" {
		t.Fatalf("instances = %v, want the apps the operator kept", names)
	}
}

// TestCancelledAtThePasswordWritesNothing: the login questions come before the
// files are written, like every other question.
func TestCancelledAtThePasswordWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.prompt.cancelAt = "secret"

	if _, err := h.run(h.options()); err == nil {
		t.Fatal("a cancelled password question did not stop init")
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("a cancelled run wrote a file")
	}
	if len(h.applied) != 0 {
		t.Fatal("a cancelled run reached the hand-over")
	}
	if !strings.Contains(h.out.String(), "oh heck, nvm") {
		t.Fatalf("init did not say it stopped:\n%s", h.out)
	}
}
