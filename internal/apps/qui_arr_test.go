package apps

import (
	"context"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// arrSpec is one Arr on the box, as the Job registers it in qui.
func arrSpec(name, kind, baseURL, key string) QuiArrSpec {
	return QuiArrSpec{Name: name, Type: kind, BaseURL: baseURL, APIKey: key}
}

// A first run creates one enabled row per Arr with the release's address and key,
// proved by qui's own test.
func TestQuiArrInstanceIsCreatedWithTheReleaseArr(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)

	spec := arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key")
	res, err := q.EnsureArrInstance(context.Background(), spec, 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || res.ID != 1 {
		t.Fatalf("create = %+v, want created id 1", res)
	}
	if len(f.arrCreated) != 1 {
		t.Fatalf("created %d rows, want 1", len(f.arrCreated))
	}
	body := f.arrCreated[0]
	for field, want := range map[string]string{
		"type": "sonarr", "name": "sonarr", "base_url": "http://sonarr:8989/sonarr", "api_key": "sonarr-key",
	} {
		if got := body.Str(field); got != want {
			t.Fatalf("create body %s = %q, want %q", field, got, want)
		}
	}
	if !body.Bool("enabled") {
		t.Fatal("the created row is disabled: qui would never use it for a lookup")
	}
	// qui's own default, internal/api/handlers/arr.go at v1.36.1.
	if body.Int("timeout_seconds") != 15 {
		t.Fatalf("timeout_seconds = %d, want 15", body.Int("timeout_seconds"))
	}
	if f.arrTested != 1 {
		t.Fatalf("tests = %d, want the created row proved once", f.arrTested)
	}
}

// A migrated qui holds the source's rows, already pointed at the release
// (internal/migrate/qui). Adopting them is the point: a create at the same
// (type, base_url) is refused 409 by the UNIQUE index.
func TestQuiArrInstanceAdoptsTheURLTheMigratorWrites(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	// The address the rewriter writes for a Sonarr endpoint: the release's
	// Service, the chart's port, and the Arr's own URL base.
	end := rewrite.Endpoint{Host: "sonarr", Port: 8989, URLBase: "/sonarr"}
	f.arrRows = []Item{{
		"id": 4, "type": "sonarr", "name": "Sonarr", "base_url": end.URL(),
		"enabled": true, "priority": 0, "timeout_seconds": 15, "last_test_status": "ok",
	}}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", end.URL(), "sonarr-key"), 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.ID != 4 {
		t.Fatalf("adopt = %+v, want the source's row 4", res)
	}
	if len(f.arrCreated) != 0 || len(f.arrRows) != 1 {
		t.Fatalf("created %d rows and hold %d, want the source's one row adopted", len(f.arrCreated), len(f.arrRows))
	}
	if got := f.arrRows[0].Str("name"); got != "Sonarr" {
		t.Fatalf("name = %q, want the operator's own", got)
	}
	if f.arrTested != 1 {
		t.Fatalf("tests = %d, want the adopted row proved once", f.arrTested)
	}
}

// A migration cannot carry the key: it is encrypted with the source's secret. An
// adopted row gets the key and nothing else.
func TestQuiArrInstanceAdoptedRowOnlyGetsItsKey(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{{
		"id": 4, "type": "sonarr", "name": "Sonarr4k", "base_url": "http://sonarr:8989/sonarr",
		"enabled": true, "priority": 7, "timeout_seconds": 30,
	}}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Change != Updated || res.ID != 4 {
		t.Fatalf("adopt = %+v, want the key written on id 4", res)
	}
	if len(f.arrUpdated) != 1 {
		t.Fatalf("PUT %d times, want once for the key", len(f.arrUpdated))
	}
	body := f.arrUpdated[0]
	for _, field := range []string{"name", "base_url", "enabled", "priority", "timeout_seconds"} {
		if _, ok := body[field]; ok {
			t.Fatalf("the update body carries %s: only the key is box's to write", field)
		}
	}
	if body.Str("api_key") != "sonarr-key" {
		t.Fatalf("update body api_key = %q", body.Str("api_key"))
	}
	row := f.arrRows[0]
	if row.Str("name") != "Sonarr4k" || row.Int("priority") != 7 || row.Int("timeout_seconds") != 30 {
		t.Fatalf("row = %v, want everything but the key left as it was", row)
	}
	if len(f.arrCreated) != 0 {
		t.Fatal("adoption created a second row")
	}
}

// A disabled row stays disabled, untested, and says so.
func TestQuiArrInstanceLeavesAnAdoptedRowDisabled(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{{
		"id": 2, "type": "radarr", "name": "Radarr", "base_url": "http://radarr:7878/radarr",
		"enabled": false,
	}}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("radarr", "radarr", "http://radarr:7878/radarr", "radarr-key"), 2, SecretFingerprint("radarr-key"))
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.Change != Unchanged || res.ID != 2 {
		t.Fatalf("adopt = %+v, want the disabled row left alone", res)
	}
	if f.arrRows[0].Bool("enabled") {
		t.Fatal("the Job enabled a row the operator had disabled")
	}
	if f.arrTested != 0 {
		t.Fatalf("tests = %d, want none: nothing dials a disabled row", f.arrTested)
	}
	if !strings.Contains(res.Note, "left disabled") {
		t.Fatalf("note = %q, want it to say the row is left disabled", res.Note)
	}
}

// The index is on the text, so a different host spelling is a second row: the lowest
// id is adopted and both named.
func TestQuiArrInstanceAdoptsTheLowestOfTwoSpellingsAndSaysSo(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{
		{"id": 9, "type": "sonarr", "name": "second", "base_url": "http://SONARR:8989/sonarr", "enabled": true},
		{"id": 5, "type": "sonarr", "name": "first", "base_url": "http://sonarr:8989/sonarr", "enabled": true},
	}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 0, "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res.ID != 5 {
		t.Fatalf("adopt = %+v, want the lowest id 5", res)
	}
	if len(f.arrCreated) != 0 || len(f.arrRows) != 2 {
		t.Fatalf("created %d rows and hold %d, want neither row touched", len(f.arrCreated), len(f.arrRows))
	}
	for _, want := range []string{"adopted id 5 of 5, 9"} {
		if !strings.Contains(res.Note, want) {
			t.Fatalf("note = %q, want it to name %q", res.Note, want)
		}
	}
}

// The other Arr at that address is a different row: the key is (type, base_url).
func TestQuiArrInstanceIgnoresTheOtherArrsRow(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{{
		"id": 3, "type": "radarr", "name": "radarr", "base_url": "http://sonarr:8989/sonarr", "enabled": true,
	}}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 0, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created {
		t.Fatalf("result = %+v, want a create: the row at that address is another type", res)
	}
	if f.arrRows[0].Int("id") != 3 || f.arrRows[0].Str("type") != "radarr" {
		t.Fatalf("rows = %v, want the Radarr row untouched", f.arrRows)
	}
}

// The recorded id is authoritative, rename or repoint included.
func TestQuiArrInstanceRecordedIDWins(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{
		{"id": 3, "type": "sonarr", "name": "renamed by hand", "base_url": "http://elsewhere:8989/sonarr", "enabled": true},
		{"id": 9, "type": "sonarr", "name": "sonarr", "base_url": "http://sonarr:8989/sonarr", "enabled": true},
	}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 3, SecretFingerprint("sonarr-key"))
	if err != nil {
		t.Fatalf("adopt the recorded id: %v", err)
	}
	if res.ID != 3 || res.Change != Unchanged {
		t.Fatalf("result = %+v, want the recorded id 3 untouched", res)
	}
	if len(f.arrCreated) != 0 || len(f.arrRows) != 2 {
		t.Fatalf("created %d rows and hold %d, want no third row", len(f.arrCreated), len(f.arrRows))
	}
}

// A gone recorded id adopts the row at the address and names the lost id.
func TestQuiArrInstanceRecordedGoneAdoptsByURL(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{{
		"id": 2, "type": "sonarr", "name": "Sonarr", "base_url": "http://sonarr:8989/sonarr", "enabled": true,
	}}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 7, "")
	if err != nil {
		t.Fatalf("adopt by address: %v", err)
	}
	if res.ID != 2 || res.Recreated {
		t.Fatalf("result = %+v, want id 2 adopted rather than recreated", res)
	}
	for _, want := range []string{"adopted id 2", "recorded 7 was gone"} {
		if !strings.Contains(res.Note, want) {
			t.Fatalf("note = %q, want it to name %q", res.Note, want)
		}
	}
	if len(f.arrCreated) != 0 {
		t.Fatal("adoption created a second row")
	}
}

// Nothing at the address either: the recorded row was deleted, so this run
// creates one and says which id went.
func TestQuiArrInstanceRecordedGoneCreates(t *testing.T) {
	_, q := newQuiFake(t)
	loginQui(t, q)

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 7, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Change != Created || !res.Recreated || res.PreviousID != 7 {
		t.Fatalf("result = %+v, want created with the previous id 7", res)
	}
}

// A second run on the recorded id and fingerprint writes nothing and still
// proves the row: the test is the proof, not a write.
func TestQuiArrInstanceSecondRunWritesNothing(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	ctx := context.Background()
	spec := arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key")

	first, err := q.EnsureArrInstance(ctx, spec, 0, "")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	writes := f.arrWrites
	second, err := q.EnsureArrInstance(ctx, spec, first.ID, SecretFingerprint(spec.APIKey))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Change != Unchanged {
		t.Fatalf("second run = %s, want unchanged", second.Change)
	}
	if f.arrWrites != writes {
		t.Fatalf("second run made %d writes", f.arrWrites-writes)
	}
	if f.arrTested != 2 {
		t.Fatalf("tests = %d, want one per run", f.arrTested)
	}
}

// qui never returns the key, so the fingerprint makes a rotation visible.
func TestQuiArrInstanceRotatedKeyIsWritten(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrRows = []Item{{
		"id": 1, "type": "sonarr", "name": "sonarr", "base_url": "http://sonarr:8989/sonarr", "enabled": true,
	}}

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "new-key"),
		1, SecretFingerprint("old-key"))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Change != Updated {
		t.Fatalf("result = %+v, want the rotated key written", res)
	}
	if len(f.arrUpdated) != 1 || f.arrUpdated[0].Str("api_key") != "new-key" {
		t.Fatalf("updates = %v, want one body carrying the new key", f.arrUpdated)
	}
}

// qui answers 200 with success:false when it cannot reach the Arr -- what a missing
// NetworkPolicy edge produces.
func TestQuiArrInstanceFailsWhenQuiCannotReachTheArr(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	f.arrTestFail = true

	res, err := q.EnsureArrInstance(context.Background(), arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key"), 0, "")
	if err == nil {
		t.Fatal("a row qui cannot reach passed its test")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want qui's own reason", err)
	}
	// The id is still reported: the caller records what exists, and the next run
	// finds it by id instead of creating a second one.
	if res.ID != 1 {
		t.Fatalf("result = %+v, want the created id", res)
	}
}

// The list never carries a key: the Job always writes the Secret's.
func TestQuiArrInstanceWritesTheSpecsKeyAndNeverReadsOneBack(t *testing.T) {
	f, q := newQuiFake(t)
	loginQui(t, q)
	ctx := context.Background()
	spec := arrSpec("sonarr", "sonarr", "http://sonarr:8989/sonarr", "sonarr-key")

	if _, err := q.EnsureArrInstance(ctx, spec, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := f.arrRows[0].Str("api_key"); got != "sonarr-key" {
		t.Fatalf("stored key = %q, want the spec's", got)
	}
	rows, err := q.ArrInstances(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := rows[0]["api_key"]; ok {
		t.Fatal("the list carries a key: qui never answers one, and the Job must not depend on one")
	}
}
