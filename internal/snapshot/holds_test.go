package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// heldAt is when the fixtures' holds were written, in UTC.
var heldAt = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// oneHold is one complete hold as a file spells it, the fixture each refusal below
// is one edit away from.
const oneHold = `{"app":"sonarr","at":{"repository":"registry.example.com/media/sonarr","tag":"4.0.13"},"avoiding":{"repository":"registry.example.com/media/sonarr","tag":"4.0.14"},"snapshot":"0.4.0-0.5.0-20260304T050607Z","since":"2026-03-04T05:06:07Z","by":"update"}`

// oneDoc is a holds file holding that one hold.
const oneDoc = `{"holds":[` + oneHold + `]}`

// holdFor is one app's hold, with refs an update step would have written.
func holdFor(app string, since time.Time) Hold {
	return Hold{
		App:      app,
		At:       ImageRef{Repository: "registry.example.com/media/" + strings.Split(app, "-")[0], Tag: "4.0.13", Digest: "sha256:" + strings.Repeat("c", 64)},
		Avoiding: ImageRef{Repository: "registry.example.com/media/" + strings.Split(app, "-")[0], Tag: "4.0.14", Digest: "sha256:" + strings.Repeat("d", 64)},
		Snapshot: "0.4.0-0.5.0-20260304T050607Z",
		Since:    since,
		By:       ByUpdate,
	}
}

func TestSaveAndLoadHolds(t *testing.T) {
	configRoot := t.TempDir()
	// Written out of app order, as SaveHolds is given it in practice.
	want := []Hold{holdFor("sonarr-4k", heldAt), holdFor("radarr", heldAt)}
	if err := SaveHolds(configRoot, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(HoldsPath(configRoot))
	if err != nil {
		t.Fatal(err)
	}
	var doc holdsFile
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the file box wrote does not decode: %v\n%s", err, data)
	}
	if got := []string{doc.Holds[0].App, doc.Holds[1].App}; !reflect.DeepEqual(got, []string{"radarr", "sonarr-4k"}) {
		t.Fatalf("the file is in %v order, want app order: %s", got, data)
	}
	// An object with a holds list, never a bare array.
	if !strings.HasPrefix(string(data), "{\n  \"holds\": [\n") {
		t.Fatalf("the file does not start with the holds list:\n%s", data)
	}

	got, err := LoadHolds(configRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sorted := []Hold{holdFor("radarr", heldAt), holdFor("sonarr-4k", heldAt)}
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("loaded %+v, want %+v", got, sorted)
	}
}

// Since is one instant however a caller's clock is zoned, and the file says it the
// way every other time box records is said.
func TestSaveHoldsWritesTheTimeInUTC(t *testing.T) {
	configRoot := t.TempDir()
	zone := time.FixedZone("elsewhere", 3600)
	if err := SaveHolds(configRoot, []Hold{holdFor("sonarr", heldAt.In(zone))}); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(HoldsPath(configRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"since": "2026-03-04T05:06:07Z"`) {
		t.Fatalf("since is not written in UTC:\n%s", data)
	}
	got, err := LoadHolds(configRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got[0].Since.Equal(heldAt) {
		t.Fatalf("since = %s, want %s", got[0].Since, heldAt)
	}
	if got[0].Since.Location() != time.UTC {
		t.Fatalf("since is zoned %s, want UTC", got[0].Since.Location())
	}
}

// A box that holds nothing keeps no record of the fact, and reading that absence
// is not a failure.
func TestLoadHoldsWithoutAFile(t *testing.T) {
	holds, err := LoadHolds(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(holds) != 0 {
		t.Fatalf("loaded %+v from a box with no holds", holds)
	}
}

// A file box cannot understand is an error naming its path, never a guess: dropping
// a hold would move an app onto the image the rollback avoided.
func TestLoadHoldsRefuses(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "malformed json", body: `{"holds": [`, wantErr: "unexpected EOF"},
		{name: "not an object", body: `[]`, wantErr: "cannot unmarshal array"},
		{name: "an unknown field", body: strings.Replace(oneDoc, `"by":"update"`, `"by":"update","note":"a comment"`, 1), wantErr: `unknown field "note"`},
		{name: "two documents", body: oneDoc + oneDoc, wantErr: "more than the one document"},
		{name: "the same app twice", body: `{"holds":[` + oneHold + `,` + oneHold + `]}`, wantErr: "held twice"},
		{name: "an app name that is a path", body: replaceApp("sonarr/4k"), wantErr: "path separator"},
		{name: "an app name with a capital", body: replaceApp("Sonarr"), wantErr: "lowercase letters and digits"},
		{name: "an empty app name", body: replaceApp(""), wantErr: "is empty"},
		{name: "an empty segment", body: replaceApp("sonarr--4k"), wantErr: "empty segment"},
		{name: "no image it ran", body: strings.Replace(oneDoc, `"at":{"repository":"registry.example.com/media/sonarr","tag":"4.0.13"}`, `"at":{}`, 1), wantErr: "at: has no repository"},
		{name: "half an image it ran", body: strings.Replace(oneDoc, `"at":{"repository":"registry.example.com/media/sonarr","tag":"4.0.13"}`, `"at":{"repository":"registry.example.com/media/sonarr"}`, 1), wantErr: "at: has neither a tag nor a digest"},
		{name: "no image it avoids", body: strings.Replace(oneDoc, `"avoiding":{"repository":"registry.example.com/media/sonarr","tag":"4.0.14"}`, `"avoiding":{}`, 1), wantErr: "avoiding: has no repository"},
		{name: "no snapshot", body: strings.Replace(oneDoc, `"snapshot":"0.4.0-0.5.0-20260304T050607Z"`, `"snapshot":""`, 1), wantErr: "snapshot: is empty"},
		{name: "a writer box does not know", body: strings.Replace(oneDoc, `"by":"update"`, `"by":"timer"`, 1), wantErr: `by "timer" is neither`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configRoot := t.TempDir()
			path := HoldsPath(configRoot)
			write(t, path, tc.body, 0o640)
			holds, err := LoadHolds(configRoot)
			if err == nil {
				t.Fatalf("loaded %+v from a file box cannot understand", holds)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error does not name the file: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error does not name %q: %v", tc.wantErr, err)
			}
		})
	}
}

// replaceApp rewrites the fixture's app name, to test the name rule on its own.
func replaceApp(app string) string {
	return strings.Replace(oneDoc, `"app":"sonarr"`, `"app":"`+app+`"`, 1)
}

// The writer checks what it writes too, so a bad hold is refused where it was
// built rather than at the next read.
func TestSaveHoldsRefuses(t *testing.T) {
	bad := []struct {
		name    string
		holds   []Hold
		wantErr string
	}{
		{name: "an app name that is not one", holds: []Hold{holdFor("Sonarr", heldAt)}, wantErr: "lowercase letters and digits"},
		{name: "no image it ran", holds: []Hold{{App: "sonarr", Avoiding: holdFor("sonarr", heldAt).Avoiding, Snapshot: "x", By: ByUpdate}}, wantErr: "at: has no repository"},
		{name: "a writer box does not know", holds: []Hold{{App: "sonarr", At: holdFor("sonarr", heldAt).At, Avoiding: holdFor("sonarr", heldAt).Avoiding, Snapshot: "x", By: "later"}}, wantErr: `by "later" is neither`},
		{name: "one app held twice", holds: []Hold{holdFor("sonarr", heldAt), holdFor("sonarr", heldAt.Add(time.Hour))}, wantErr: "held twice"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			configRoot := t.TempDir()
			err := SaveHolds(configRoot, tc.holds)
			if err == nil {
				t.Fatal("the holds were written")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error does not name %q: %v", tc.wantErr, err)
			}
			if _, statErr := os.Lstat(HoldsPath(configRoot)); !os.IsNotExist(statErr) {
				t.Fatalf("a refused save left a file: %v", statErr)
			}
		})
	}
}

// The absence of the file is how a box with nothing held reads, so an empty list
// leaves no file behind.
func TestSaveHoldsWithoutHoldsRemovesTheFile(t *testing.T) {
	configRoot := t.TempDir()
	if err := SaveHolds(configRoot, []Hold{holdFor("sonarr", heldAt)}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Lstat(HoldsPath(configRoot)); err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	for _, empty := range [][]Hold{nil, {}} {
		if err := SaveHolds(configRoot, empty); err != nil {
			t.Fatalf("save empty: %v", err)
		}
		if _, err := os.Lstat(HoldsPath(configRoot)); !os.IsNotExist(err) {
			t.Fatalf("an empty list left the file behind: %v", err)
		}
		// And on a box that never had one, removing it is not a failure.
		if err := SaveHolds(configRoot, empty); err != nil {
			t.Fatalf("save empty over nothing: %v", err)
		}
	}
}

// One hold per app, replaced in place of the old one, in app order so the file it
// becomes is stable.
func TestWithHoldReplacesAndSorts(t *testing.T) {
	holds := []Hold{holdFor("sonarr", heldAt)}
	holds = WithHold(holds, holdFor("radarr", heldAt))
	if got := appOrder(holds); !reflect.DeepEqual(got, []string{"radarr", "sonarr"}) {
		t.Fatalf("holds are in %v order, want app order", got)
	}
	later := holdFor("sonarr", heldAt.Add(time.Hour))
	holds = WithHold(holds, later)
	if len(holds) != 2 {
		t.Fatalf("replacing a hold left %d holds: %+v", len(holds), holds)
	}
	if holds[1].App != "sonarr" || !holds[1].Since.Equal(heldAt.Add(time.Hour)) {
		t.Fatalf("sonarr's hold is %+v, want the one just written", holds[1])
	}
	// A hold on a box that holds nothing yet.
	one := WithHold(nil, holdFor("qui", heldAt))
	if len(one) != 1 || one[0].App != "qui" {
		t.Fatalf("WithHold on nothing = %+v", one)
	}
}

func TestWithoutHold(t *testing.T) {
	holds := []Hold{holdFor("radarr", heldAt), holdFor("sonarr", heldAt)}
	kept := WithoutHold(holds, "sonarr")
	if got := appOrder(kept); !reflect.DeepEqual(got, []string{"radarr"}) {
		t.Fatalf("WithoutHold left %v", kept)
	}
	if got := appOrder(WithoutHold(holds, "plex")); !reflect.DeepEqual(got, []string{"radarr", "sonarr"}) {
		t.Fatalf("removing an app with no hold changed the list: %v", got)
	}
	if got := WithoutHold(holds, "radarr"); len(got) != 1 || got[0].App != "sonarr" {
		t.Fatalf("WithoutHold(radarr) = %+v", got)
	}
	// The list it was given is not touched: callers keep the holds they read.
	if got := appOrder(holds); !reflect.DeepEqual(got, []string{"radarr", "sonarr"}) {
		t.Fatalf("WithoutHold changed its input: %v", got)
	}
}

func appOrder(holds []Hold) []string {
	out := make([]string, len(holds))
	for i, h := range holds {
		out[i] = h.App
	}
	return out
}

// The hold file is written where the seedbox's own box can write it, with the mode
// that lets that user read what root wrote.
func TestHoldsBelongToTheSeedbox(t *testing.T) {
	configRoot := t.TempDir()
	if os.Geteuid() == 0 {
		if err := os.Chown(configRoot, 1234, 1234); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveHolds(configRoot, []Hold{holdFor("sonarr", heldAt)}); err != nil {
		t.Fatalf("save: %v", err)
	}
	box := filepath.Join(configRoot, boxDir)
	info, err := os.Lstat(box)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != dirMode {
		t.Fatalf("%s = %s, want a directory at %o", box, info.Mode(), dirMode)
	}
	path := HoldsPath(configRoot)
	info, err = os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != treeFileMode {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), treeFileMode)
	}
	if os.Geteuid() == 0 {
		for _, p := range []string{box, path} {
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if uid, gid := owner(info); uid != 1234 || gid != 1234 {
				t.Fatalf("%s owned %d:%d, want the config root's 1234:1234", p, uid, gid)
			}
		}
	}
}

// What SaveHolds writes is what LoadHolds reads, for every field a hold carries:
// the contract between a rollback that writes a hold and the update that reads it.
func TestHoldsSurviveARewrite(t *testing.T) {
	configRoot := t.TempDir()
	want := Hold{
		App:      "sonarr-4k",
		At:       ImageRef{Repository: "registry.example.com/media/sonarr", Digest: "sha256:" + strings.Repeat("a", 64)},
		Avoiding: ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.14"},
		Snapshot: "0.4.0-0.5.0-20260304T050607Z",
		Since:    heldAt,
		By:       ByRollback,
	}
	if err := SaveHolds(configRoot, []Hold{want}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadHolds(configRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("loaded %+v, want %+v", got, want)
	}
}

// A file box may not read is an error and not "no holds", because the two lead to
// opposite decisions about an app.
func TestLoadHoldsRefusesAnUnreadableFile(t *testing.T) {
	configRoot := t.TempDir()
	// A directory where the file should be: readable path, unreadable content.
	if err := os.MkdirAll(HoldsPath(configRoot), 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := LoadHolds(configRoot)
	if err == nil {
		t.Fatal("a directory read as a holds file")
	}
	if !strings.Contains(err.Error(), HoldsPath(configRoot)) {
		t.Fatalf("error does not name the file: %v", err)
	}
}
