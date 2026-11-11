package ttlconf

import "testing"

func TestSealWritesApiKeyAndEnablesAPIOnEmptyFile(t *testing.T) {
	out, changed := Seal(nil, "s5nseeded00000000000000000000001")
	if !changed {
		t.Fatal("want changed on an absent file")
	}
	doc := parse(out)
	if v := doc.get("General", "api_key"); v != "s5nseeded00000000000000000000001" {
		t.Fatalf("api_key = %q", v)
	}
	if v := doc.get("General", "api_enabled"); v != "1" {
		t.Fatalf("api_enabled = %q, want 1", v)
	}
}

func TestSealIsNoOpWhenAlreadySeeded(t *testing.T) {
	first, _ := Seal(nil, "key-1")
	second, changed := Seal(first, "key-1")
	if changed {
		t.Fatal("re-sealing the same key reported a change")
	}
	if string(second) != string(first) {
		t.Fatal("unchanged seal must return the original bytes untouched")
	}
}

func TestSealPreservesOtherSettingsAndRepairsDisabledAPI(t *testing.T) {
	// A real config.ini has many more keys; this proves Seal only ever
	// touches api_key/api_enabled and never reformats or drops the rest,
	// and that a user (or a prior unsafe write) disabling the API is
	// corrected, since the wiring Job depends on it.
	existing := []byte("[General]\n api_key = key-1\n api_enabled = 0\n cache_images = 1\n\n[PMS]\n pms_ip = plex\n")
	out, changed := Seal(existing, "key-1")
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
	out, changed := Seal(existing, "new-key")
	if !changed {
		t.Fatal("want changed: the key differs")
	}
	doc := parse(out)
	if v := doc.get("General", "api_key"); v != "new-key" {
		t.Fatalf("api_key = %q, want rotated", v)
	}
}
