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
		"AuthSubnetWhitelistEnabled=false",
		"Port=8080",
		`Session\DefaultSavePath=/data/torrents`,
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
