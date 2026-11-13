package qbtconf

import (
	"strings"
	"testing"
)

const sampleConf = `[BitTorrent]
Session\Port=31111

[LegalNotice]
Accepted=false

[Meta]
MigrationVersion=12

[Preferences]
Connection\ResolvePeerCountries=true
General\Locale=en

[WebUI]
Address=*
Port=8080
ServerDomains=*
`

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(h, "correct horse battery staple") {
		t.Fatal("fresh hash does not verify")
	}
	if Verify(h, "wrong") {
		t.Fatal("wrong password verified")
	}
	for _, bad := range []string{"", "nocolon", "!!!:???", "AAAA:BBBB"} {
		if Verify(bad, "whatever") {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
}

func TestHashFormat(t *testing.T) {
	h, err := Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	salt, key, ok := strings.Cut(h, ":")
	if !ok {
		t.Fatalf("hash %q has no separator", h)
	}
	if len(salt) != 24 { // 16 bytes, base64 with padding
		t.Fatalf("salt %q is not 16 base64-encoded bytes", salt)
	}
	if len(key) != 88 { // 64 bytes
		t.Fatalf("key is not 64 base64-encoded bytes: %q", key)
	}
}

func TestParseRenderRoundTrip(t *testing.T) {
	got := string(Parse([]byte(sampleConf)).Render())
	if got != sampleConf {
		t.Fatalf("round trip changed the document:\n--- want ---\n%s\n--- got ---\n%s", sampleConf, got)
	}
}

func TestSeal(t *testing.T) {
	out, changed, err := Seal([]byte(sampleConf), "admin", "s3cret", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("sealing a config without credentials reported no change")
	}
	doc := string(out)
	for _, want := range []string{
		"[LegalNotice]\nAccepted=true",
		"Username=admin",
		`Password_PBKDF2="@ByteArray(`,
		"CSRFProtection=false",
		"HostHeaderValidation=false",
		"LocalHostAuth=false",
		"AuthSubnetWhitelistEnabled=true",
		"AuthSubnetWhitelist=" + subnetListValue(),
		"Port=8080",
		`Session\DefaultSavePath=/data/downloads`,
		`Session\UseCategoryPathsInManualMode=true`,
		"Session\\Port=31111", // untouched
		"MigrationVersion=12",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("sealed config is missing %q:\n%s", want, doc)
		}
	}
	// Exactly one section per name: sealing must not duplicate [WebUI].
	if n := strings.Count(doc, "[WebUI]"); n != 1 {
		t.Fatalf("sealed config has %d [WebUI] sections", n)
	}
	value, ok := Parse(out).Get("WebUI", "Password_PBKDF2")
	if !ok {
		t.Fatal("Password_PBKDF2 not written")
	}
	hash, err := ParsePasswordValue(value)
	if err != nil {
		t.Fatalf("parse stored value: %v", err)
	}
	if !Verify(hash, "s3cret") {
		t.Fatal("stored hash does not verify the sealed password")
	}

	// Second run: nothing to do, and the file is returned untouched.
	again, changed, err := Seal(out, "admin", "s3cret", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("re-sealing the same credentials reported a change")
	}
	if string(again) != doc {
		t.Fatal("re-sealing rewrote the file")
	}

	// A different password is a real change, and the old hash stops verifying.
	rotated, changed, err := Seal(out, "admin", "newpass", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("rotating the password reported no change")
	}
	rotatedValue, ok := Parse(rotated).Get("WebUI", "Password_PBKDF2")
	if !ok {
		t.Fatal("no Password_PBKDF2 in rotated config")
	}
	hash = mustParseValue(t, rotatedValue)
	if Verify(hash, "s3cret") {
		t.Fatal("old password still verifies after rotation")
	}
	if !Verify(hash, "newpass") {
		t.Fatal("new password does not verify after rotation")
	}
}

func mustParseValue(t *testing.T, v string) string {
	t.Helper()
	h, err := ParsePasswordValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSealCreatesMissingFile(t *testing.T) {
	out, changed, err := Seal(nil, "admin", "pw", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("sealing an absent config reported no change")
	}
	if !strings.HasPrefix(string(out), "[WebUI]\nUsername=admin\n") {
		t.Fatalf("unexpected document:\n%s", out)
	}
}

// preBypassConfig is a config a box from before the private-range bypass
// leaves behind: the login is required everywhere, the whitelist is off, and
// the credential the release Secret still holds is already sealed.
func preBypassConfig(t *testing.T, password string) (conf []byte, hash string) {
	t.Helper()
	hash, err := Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`[LegalNotice]
Accepted=true

[Preferences]
Session\DefaultSavePath=/data/downloads
Session\TempPath=/data/downloads/temp
Session\UseCategoryPathsInManualMode=true

[WebUI]
Address=*
AuthSubnetWhitelistEnabled=false
CSRFProtection=false
HostHeaderValidation=false
LocalHostAuth=false
Password_PBKDF2=` + PasswordValue(hash) + `
Port=8080
ServerDomains=*
Username=admin
`), hash
}

// The bypass is the release's login story, so sealing any config must leave
// the three keys exactly as docs/box.md decided: loopback skipped, the private
// ranges listed, and the list in the form QSettings reads back as a
// QStringList rather than one quoted string.
func TestSealBypassesTheLoginForPrivateAddresses(t *testing.T) {
	const password = "s3cret"
	want := strings.Join([]string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7"}, ", ")
	if got := subnetListValue(); got != want {
		t.Fatalf("subnetListValue() = %q, want %q", got, want)
	}

	old, storedHash := preBypassConfig(t, password)
	for name, existing := range map[string][]byte{
		"no file at all":                nil,
		"a file from before the bypass": old,
	} {
		out, changed, err := Seal(existing, "admin", password, 8080)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !changed {
			t.Fatalf("%s: sealing reported no change", name)
		}
		doc := string(out)
		if wantLine := "AuthSubnetWhitelist=" + want; !strings.Contains(doc, wantLine+"\n") {
			t.Fatalf("%s: sealed config has no unquoted %q:\n%s", name, wantLine, doc)
		}
		parsed := Parse(out)
		for key, wantValue := range map[string]string{
			"AuthSubnetWhitelistEnabled": "true",
			"AuthSubnetWhitelist":        want,
			"LocalHostAuth":              "false",
		} {
			if got, ok := parsed.Get("WebUI", key); !ok || got != wantValue {
				t.Fatalf("%s: WebUI/%s = %q (present %v), want %q", name, key, got, ok, wantValue)
			}
		}

		// Sealing is what an init container does on every pod start, so the
		// second pass over its own output must be a byte-for-byte no-op.
		again, changed, err := Seal(out, "admin", password, 8080)
		if err != nil {
			t.Fatalf("%s: re-seal: %v", name, err)
		}
		if changed || string(again) != doc {
			t.Fatalf("%s: re-sealing rewrote the file (changed=%v)", name, changed)
		}
	}

	// The stored credential is reused rather than re-minted: a new salt on
	// every start would rewrite the file forever.
	out, _, err := Seal(old, "admin", password, 8080)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := Parse(out).Get("WebUI", "Password_PBKDF2")
	if !ok {
		t.Fatal("sealed config lost Password_PBKDF2")
	}
	if !strings.Contains(stored, storedHash) {
		t.Fatalf("sealing replaced the stored hash: %q does not carry %q", stored, storedHash)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"admin":   "admin",
		"":        `""`,
		"a b":     `"a b"`,
		"@x":      `"@x"`,
		`a"b`:     `"a\"b"`,
		`a\b`:     `"a\\b"`,
		"plain-1": "plain-1",
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s, want %s", in, got, want)
		}
		if got := unquote(quote(in)); got != in {
			t.Errorf("unquote(quote(%q)) = %q", in, got)
		}
	}
}
