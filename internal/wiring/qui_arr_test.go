package wiring

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// One Arr row per Sonarr and Radarr on the box, with the release's address, the
// Arr's own key and qui's own test proving each one from qui's pod. The ids and
// key fingerprints are recorded, and a second run writes nothing at all.
func TestWiringRegistersQuisArrInstances(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	ctx := context.Background()

	first, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, Summary(first))
	}
	for _, inst := range h.config.Instances {
		step := stepByName(t, first, "qui/arr/"+inst.Name)
		if step.Result != "ok" || step.Detail != "created" {
			t.Fatalf("qui/arr/%s = %+v, want created", inst.Name, step)
		}
	}
	if len(f.quiArrRows) != len(h.config.Instances) {
		t.Fatalf("qui holds %d Arr rows for %d Arrs: %+v", len(f.quiArrRows), len(h.config.Instances), f.quiArrRows)
	}
	for _, inst := range h.config.Instances {
		row := quiArrRow(t, f, inst.Kind)
		if got, want := row.Str("base_url"), strings.TrimRight(inst.URL, "/")+inst.URLBase; got != want {
			t.Fatalf("%s base_url = %q, want the Arr's own address %q", inst.Name, got, want)
		}
		if row.Str("name") != inst.Name {
			t.Fatalf("%s row is named %q", inst.Name, row.Str("name"))
		}
		if !row.Bool("enabled") {
			t.Fatalf("%s row is disabled: qui would never use it", inst.Name)
		}
	}
	rec := ownedOf(t, h)
	for _, inst := range h.config.Instances {
		if rec.QuiArrInstanceID(inst.Name) == 0 {
			t.Fatalf("qui's %s row is not recorded: %+v", inst.Name, rec.Qui)
		}
		key, err := ReadSecret(inst.APIKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.QuiArrInstanceFingerprint(inst.Name); got != apps.SecretFingerprint(key) {
			t.Fatalf("qui's %s key fingerprint = %q, want the key it was proved with", inst.Name, got)
		}
	}
	// No key ever lands in owned.json, only its sha256.
	if strings.Contains(h.cmData["owned.json"], "sonarr-key") {
		t.Fatal("owned.json carries an Arr key")
	}

	writes := f.quiArrWrites
	second, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if second.Writes.Created != 0 || second.Writes.Updated != 0 {
		t.Fatalf("second run was not a no-op: %+v\n%s", second.Writes, Summary(second))
	}
	if f.quiArrWrites != writes {
		t.Fatalf("second run wrote qui's Arr rows %d times", f.quiArrWrites-writes)
	}
	if again := ownedOf(t, h); again.QuiArrInstanceID("sonarr") != rec.QuiArrInstanceID("sonarr") {
		t.Fatalf("the recorded row changed between runs: %+v -> %+v", rec.Qui, again.Qui)
	}
	if f.quiArrTested != 2*len(h.config.Instances) {
		t.Fatalf("tests = %d, want one per row per run: the test is the proof, not a write", f.quiArrTested)
	}
}

// A migrated box's qui already holds the source's Arr rows, and the migration
// has pointed each at the release (internal/migrate/qui rewrites
// arr_instances.base_url). A create there is refused 409 by the table's UNIQUE
// index, so the Job adopts the source's rows -- their ids are recorded, their
// names and their enabled flags are left as they were -- and only the key, which
// was the source Arr's, is written.
func TestWiringAdoptsAMigratedQuisArrInstances(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	// What the rewriter leaves: the release's address, the source's name, the
	// source's (encrypted, unreadable) key, and an id the migration did not
	// renumber.
	for i, inst := range h.config.Instances {
		f.quiArrRows = append(f.quiArrRows, apps.Item{
			"id": 40 + i, "type": inst.Kind, "name": "source " + inst.Name,
			"base_url": strings.TrimRight(inst.URL, "/") + inst.URLBase,
			"enabled":  true, "priority": 0, "timeout_seconds": 15,
		})
	}

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if len(f.quiArrRows) != len(h.config.Instances) {
		t.Fatalf("qui holds %d Arr rows for %d Arrs: a second row was created beside the source's",
			len(f.quiArrRows), len(h.config.Instances))
	}
	rec := ownedOf(t, h)
	for i, inst := range h.config.Instances {
		step := stepByName(t, res, "qui/arr/"+inst.Name)
		if step.Result != "ok" || step.Detail != "updated" {
			t.Fatalf("qui/arr/%s = %+v, want the source's row adopted and its key written", inst.Name, step)
		}
		if got := rec.QuiArrInstanceID(inst.Name); got != 40+i {
			t.Fatalf("qui's %s row recorded as id %d, want the source's %d", inst.Name, got, 40+i)
		}
		if row := quiArrRow(t, f, inst.Kind); row.Str("name") != "source "+inst.Name {
			t.Fatalf("%s row was renamed to %q: a name is the operator's", inst.Name, row.Str("name"))
		}
	}
}

// A row the operator disabled is left disabled: nothing dials it, so there is
// nothing to prove, and the receipt says which row it was.
func TestWiringLeavesADisabledQuisArrRowAlone(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	f.quiArrRows = []apps.Item{{
		"id": 3, "type": "sonarr", "name": "sonarr", "enabled": false,
		"base_url": strings.TrimRight(h.config.Instances[0].URL, "/") + h.config.Instances[0].URLBase,
	}}

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	step := stepByName(t, res, "qui/arr/sonarr")
	if step.Result != "ok" || !strings.Contains(step.Detail, "left disabled") {
		t.Fatalf("qui/arr/sonarr = %+v, want the disabled row left alone and said so", step)
	}
	if f.quiArrRows[0].Bool("enabled") {
		t.Fatal("the Job enabled a row the operator had disabled")
	}
}

// Lidarr is the one Arr qui has no instance type for, so a box that runs it gets
// a row saying so rather than silence -- and the run still finishes.
func TestWiringSkipsQuisLidarrInstance(t *testing.T) {
	h := newHarness(t)
	f := h.withApps(t)
	lidarr := httptest.NewServer(h.stack.arrHandler("lidarr", "/lidarr"))
	t.Cleanup(lidarr.Close)
	h.config.Instances = append(h.config.Instances, Instance{
		Name: "lidarr", Kind: "lidarr", URL: lidarr.URL, URLBase: "/lidarr",
		APIKeyFile: h.write(t, "lidarr.key", "lidarr-key"),
		Category:   "music",
		SavePath:   filepath.Join(h.dir, "data", "downloads/music"),
		RootFolder: filepath.Join(h.dir, "data", "media/music"),
	})
	h.runner.Config = h.config

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	step := stepByName(t, res, "qui/arr/lidarr")
	if step.Result != "skipped" || !strings.Contains(step.Detail, "no lidarr instance type") {
		t.Fatalf("qui/arr/lidarr = %+v, want it skipped with the reason", step)
	}
	for _, row := range f.quiArrRows {
		if row.Str("type") == "lidarr" {
			t.Fatalf("a lidarr row was written: %+v", row)
		}
	}
}
