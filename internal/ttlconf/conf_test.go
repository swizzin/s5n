package ttlconf

import (
	"bytes"
	"strings"
	"testing"
)

const testKey = "s5nseeded00000000000000000000001"

func TestSealWritesApiKeyAndEnablesAPIOnEmptyFile(t *testing.T) {
	out, changed := Seal(nil, testKey, "/tautulli")
	if !changed {
		t.Fatal("want changed on an absent file")
	}
	doc := parse(out)
	if v := doc.get("General", "api_key"); v != testKey {
		t.Fatalf("api_key = %q", v)
	}
	if v := doc.get("General", "api_enabled"); v != "1" {
		t.Fatalf("api_enabled = %q, want 1", v)
	}
	if v := doc.get("General", "http_root"); v != "/tautulli" {
		t.Fatalf("http_root = %q, want the chart's prefix", v)
	}
}

func TestSealIsNoOpWhenAlreadySeeded(t *testing.T) {
	first, _ := Seal(nil, testKey, "/tautulli")
	second, changed := Seal(first, testKey, "/tautulli")
	if changed {
		t.Fatal("re-sealing the same key reported a change")
	}
	if string(second) != string(first) {
		t.Fatal("unchanged seal must return the original bytes untouched")
	}
}

func TestSealPreservesOtherSettingsAndRepairsDisabledAPI(t *testing.T) {
	// A real config.ini has many more keys; this proves Seal touches only its own
	// and never reformats the rest, and that a disabled API is corrected because
	// the wiring Job depends on it.
	existing := []byte("[General]\n api_key = key-1\n api_enabled = 0\n cache_images = 1\n\n[PMS]\n pms_ip = plex\n")
	out, changed := Seal(existing, "key-1", "/tautulli")
	if !changed {
		t.Fatal("want changed: api_enabled was 0")
	}
	doc := parse(out)
	if v := doc.get("General", "api_enabled"); v != "1" {
		t.Fatalf("api_enabled = %q, want repaired to 1", v)
	}
	if v := doc.get("General", "cache_images"); v != "1" {
		t.Fatalf("cache_images (untouched) = %q", v)
	}
	if v := doc.get("PMS", "pms_ip"); v != "plex" {
		t.Fatalf("PMS section (untouched) = %q", v)
	}
}

func TestSealRotatesAnExistingDifferentKey(t *testing.T) {
	existing := []byte("[General]\n api_key = old-key\n api_enabled = 1\n")
	out, changed := Seal(existing, "new-key", "/tautulli")
	if !changed {
		t.Fatal("want changed: the key differs")
	}
	doc := parse(out)
	if v := doc.get("General", "api_key"); v != "new-key" {
		t.Fatalf("api_key = %q, want rotated", v)
	}
}

// A non-empty http_password makes Tautulli register its auth tool and ask for a
// password (webstart.py:195-213 at v2.18.2), and a settings page or a migrated
// install can write one back. The login is the box's, so this is re-sealed like
// api_key.
func TestSealSwitchesTheAppsOwnLoginOff(t *testing.T) {
	existing := []byte("[General]\n api_key = key-1\n api_enabled = 1\n http_root = tautulli\n" +
		" http_username = someone\n http_password = \"hashed-or-not\"\n http_hash_password = 1\n" +
		" http_hashed_password = 1\n http_basic_auth = 1\n http_plex_admin = 1\n")
	out, changed := Seal(existing, "key-1", "/tautulli")
	if !changed {
		t.Fatal("want changed: the app's own login was configured")
	}
	doc := parse(out)
	for _, want := range []struct{ key, value string }{
		{"http_root", "/tautulli"},
		{"http_username", empty},
		{"http_password", empty},
		{"http_hash_password", "0"},
		{"http_hashed_password", "0"},
		{"http_basic_auth", "0"},
	} {
		if v := doc.get("General", want.key); v != want.value {
			t.Errorf("General/%s = %q, want %q", want.key, v, want.value)
		}
	}
	// http_plex_admin decides who may use Tautulli's own login, which is off;
	// box never changes a setting a person might want back.
	if v := doc.get("General", "http_plex_admin"); v != "1" {
		t.Errorf("http_plex_admin = %q, want it left alone", v)
	}
}

// The password key alone decides: a file with nothing else still comes out with
// a login nobody can use, because no password is never a password.
func TestSealSwitchesOffAPasswordWithNoUsername(t *testing.T) {
	existing := []byte("[General]\n api_key = key-1\n api_enabled = 1\n http_password = 'x'\n")
	out, _ := Seal(existing, "key-1", "/tautulli")
	if v := parse(out).get("General", "http_password"); v != empty {
		t.Fatalf("http_password = %q, want it cleared", v)
	}
}

// 2.19 put two more ways to ask for a login in front of an empty password; see
// docs/box.md "Tautulli" (plexpy/__init__.py:3121-3145 at v2.19.1,
// webstart.py:406-441).
func TestSealKeepsA219FileFromAskingForALogin(t *testing.T) {
	// What 2.19.1 writes for itself: every definition, the two switches at their
	// install defaults, no password.
	existing := []byte("[General]\n api_key = key-1\n api_enabled = 1\n http_root = tautulli\n" +
		" first_run_complete = 0\n http_username = \"\"\n http_password = \"\"\n" +
		" http_hash_password = 0\n http_hashed_password = 0\n http_basic_auth = 0\n\n" +
		"[Advanced]\n allow_public_no_password = 0\n config_version = 23\n")
	out, changed := Seal(existing, "key-1", "/tautulli")
	if !changed {
		t.Fatal("want changed: the file's two switches ask for a login")
	}
	doc := parse(out)
	for _, want := range []struct{ section, key, value string }{
		{"General", "first_run_complete", "1"},
		{"Advanced", "allow_public_no_password", "1"},
	} {
		if v := doc.get(want.section, want.key); v != want.value {
			t.Errorf("%s/%s = %q, want %q", want.section, want.key, v, want.value)
		}
	}
	// The app's own settings are untouched, and the new key did not swallow the
	// section after it.
	if v := doc.get("Advanced", "config_version"); v != "23" {
		t.Errorf("Advanced/config_version = %q, want it left alone", v)
	}
}

// An older file has no switch to turn, so the key is added: the app's default for
// it is the state that asks for a login.
func TestSealAddsTheSwitchesToAnOlderFile(t *testing.T) {
	existing := []byte("[General]\n api_key = key-1\n api_enabled = 1\n http_root = /tautulli\n http_password = \"\"\n")
	out, changed := Seal(existing, "key-1", "/tautulli")
	if !changed {
		t.Fatal("want changed: an older file has neither switch")
	}
	doc := parse(out)
	if v := doc.get("General", "first_run_complete"); v != "1" {
		t.Errorf("General/first_run_complete = %q, want it added as 1", v)
	}
	if v := doc.get("Advanced", "allow_public_no_password"); v != "1" {
		t.Errorf("Advanced/allow_public_no_password = %q, want it added as 1", v)
	}
}

// Tautulli writes this file itself and reads it back, so a reformat is a settings
// file the app no longer recognises.
func TestSealKeepsEveryOtherByte(t *testing.T) {
	existing := []byte("# written by Tautulli\n[General]\n api_key = key-1\n api_enabled = 1\n" +
		" http_root = /tautulli\n http_username = \"\"\n http_password = \"\"\n" +
		" http_hash_password = 0\n http_hashed_password = 0\n http_basic_auth = 0\n" +
		" first_run_complete = 1\n  # a comment after a key\n cache_images = 1\n\n[PMS]\n pms_ip = plextower\n\n" +
		"[Advanced]\n allow_public_no_password = 1\n")
	out, changed := Seal(existing, "key-1", "/tautulli")
	if changed {
		t.Fatalf("a file already sealed reported a change: %q", out)
	}
	if !bytes.Equal(out, existing) {
		t.Fatalf("the sealed file differs:\n%q\n%q", out, existing)
	}
	// And the same file with one owned value out of place changes nothing else.
	edited := []byte(strings.Replace(string(existing), " http_username = \"\"", " http_username = someone", 1))
	out, changed = Seal(edited, "key-1", "/tautulli")
	if !changed {
		t.Fatal("want changed: http_username was set")
	}
	if want := strings.Replace(string(existing), " http_username = someone", " http_username = \"\"", 1); string(out) != want {
		t.Fatalf("sealing one key rewrote more than it:\n%q\n%q", out, want)
	}
}
