package wiring

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The first install runs against a data path nothing has prepared: the two
// trees under every category must be made before the Arr root-folder step, or
// that step is refused. This drives the function from an empty temp directory.
func TestEnsureDataDirsCreatesBothTrees(t *testing.T) {
	root := t.TempDir()
	insts := []Instance{
		{Name: "sonarr", Category: "tv", SavePath: filepath.Join(root, "downloads/tv"), RootFolder: filepath.Join(root, "media/tv")},
		{Name: "radarr", Category: "movies", SavePath: filepath.Join(root, "downloads/movies"), RootFolder: filepath.Join(root, "media/movies")},
	}
	dirs, err := EnsureDataDirs(insts)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(dirs) != 4 {
		t.Fatalf("got %d directories, want 4 (two per instance): %+v", len(dirs), dirs)
	}
	for i, d := range dirs {
		if !d.Created {
			t.Fatalf("%s was reported as existing on an empty path: %+v", d.Path, d)
		}
		info, err := os.Stat(d.Path)
		if err != nil {
			t.Fatalf("stat %s: %v", d.Path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", d.Path)
		}
		// The chart runs the wiring Job as the release's puid:pgid, so a
		// directory this process creates is owned by that uid:gid. init tells the
		// operator exactly that, so it cannot be a different owner here.
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: no stat_t", d.Path)
		}
		if int(st.Uid) != os.Geteuid() || int(st.Gid) != os.Getegid() {
			t.Fatalf("%s owned %d:%d, want the creating process %d:%d", d.Path, st.Uid, st.Gid, os.Geteuid(), os.Getegid())
		}
		// Owner write is what the Arr and qBittorrent need; a restrictive umask
		// may drop the group bits, so only the owner bits are asserted.
		if info.Mode().Perm()&0o700 != 0o700 {
			t.Fatalf("%s mode %v is not owner-writable", d.Path, info.Mode().Perm())
		}
		if want := []string{"data/dirs/downloads/tv", "data/dirs/media/tv", "data/dirs/downloads/movies", "data/dirs/media/movies"}[i]; d.Name != want {
			t.Fatalf("receipt name %q, want %q", d.Name, want)
		}
	}

	// A second run is the same call: everything is reported, nothing is touched.
	again, err := EnsureDataDirs(insts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, d := range again {
		if d.Created {
			t.Fatalf("%s reported as created on the second run", d.Path)
		}
	}
}

// A directory that is already there (the operator made it, or a previous run
// did) is left alone, and only the missing one is made.
func TestEnsureDataDirsReportsExistingAndCreatesMissing(t *testing.T) {
	root := t.TempDir()
	save := filepath.Join(root, "downloads/tv")
	if err := os.MkdirAll(save, 0o775); err != nil {
		t.Fatal(err)
	}
	dirs, err := EnsureDataDirs([]Instance{{Name: "sonarr", Category: "tv", SavePath: save, RootFolder: filepath.Join(root, "media/tv")}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(dirs) != 2 || dirs[0].Created || !dirs[1].Created {
		t.Fatalf("want downloads existing then media created, got %+v", dirs)
	}
}

// Two instances that share a category and its paths must not produce two
// receipt lines for one directory.
func TestEnsureDataDirsDeduplicatesSharedPaths(t *testing.T) {
	root := t.TempDir()
	shared := Instance{Name: "sonarr", Category: "tv", SavePath: filepath.Join(root, "downloads/tv"), RootFolder: filepath.Join(root, "media/tv")}
	other := shared
	other.Name = "sonarr-hd"
	other.Category = "tv-hd"
	dirs, err := EnsureDataDirs([]Instance{shared, other})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("got %d lines for two paths, want 2: %+v", len(dirs), dirs)
	}
}

// A library path that is a file, or that sits under one, fails hard and names
// the path: an Arr would otherwise be handed a root folder it cannot write.
func TestEnsureDataDirsFailsWithPathAndError(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, rootFolder := range map[string]string{
		"it is a file":       blocker,
		"a parent is a file": filepath.Join(blocker, "media/tv"),
	} {
		_, err := EnsureDataDirs([]Instance{{
			Name: "sonarr", Category: "tv",
			SavePath: filepath.Join(root, "downloads/tv"), RootFolder: rootFolder,
		}})
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if !strings.Contains(err.Error(), rootFolder) {
			t.Fatalf("%s: error does not name the path %s: %v", name, rootFolder, err)
		}
	}
}

// The hardlink proof must not create what it proves: with a missing directory
// it has to fail, not silently prepare the path (that is what hid the empty
// first install).
func TestCheckHardlinkDoesNotCreateDirectories(t *testing.T) {
	root := t.TempDir()
	save := filepath.Join(root, "downloads/tv")
	media := filepath.Join(root, "media/tv")
	check := CheckHardlink(save, media)
	if check.OK || check.Error == "" {
		t.Fatalf("proof passed without its directories: %+v", check)
	}
	for _, dir := range []string{save, media} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s was created by the proof", dir)
		}
	}
}

// The order in the run is the bug being fixed: on an empty data path the
// directories are made before the Arr root-folder step, and the run succeeds.
func TestWiringMakesDataDirsBeforeTheArrSteps(t *testing.T) {
	h := newHarness(t)
	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	for _, inst := range h.config.Instances {
		for _, dir := range []string{inst.SavePath, inst.RootFolder} {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("%s was not created by the run: %v", dir, err)
			}
		}
		step := stepByName(t, res, "data/dirs/downloads/"+inst.Category)
		if step.Result != "ok" || step.Detail != "created" {
			t.Fatalf("data/dirs/%s step = %+v, want ok created", inst.Category, step)
		}
	}

	firstDir, firstArr := -1, len(res.Steps)
	for i, s := range res.Steps {
		if strings.HasPrefix(s.Name, "data/dirs/") && firstDir < 0 {
			firstDir = i
		}
		if strings.HasSuffix(s.Name, "/rootfolder") && i < firstArr {
			firstArr = i
		}
	}
	if firstDir < 0 || firstArr >= len(res.Steps) {
		t.Fatalf("receipt has no directory or root-folder step:\n%s", Summary(res))
	}
	if firstDir > firstArr {
		t.Fatalf("a root folder was added before the directories existed:\n%s", Summary(res))
	}
}
