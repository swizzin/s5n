package wiring

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// DirResult is one directory the data contract needs, with the receipt line it
// is reported under and whether this run had to make it.
type DirResult struct {
	Name    string // data/dirs/<tree>/<category>
	Path    string
	Created bool
}

// EnsureDataDirs creates every instance's download and library directory, and
// runs before any application is asked anything.
//
// The order is the contract, not an optimisation. Sonarr and Radarr validate a
// root folder when it is added and refuse a path whose directory does not
// exist, so on a completely empty data path the root-folder step cannot succeed
// until /data/media/<category> is there — and nothing else in the install makes
// it. The hardlink proof was the only code creating these directories, and it
// runs after the Arrs, so a fresh install only worked on a host where something
// else had already prepared the paths.
//
// qBittorrent's category save paths are the other half of the same tree, so
// both are provisioned together, under the one /data mount: that is what keeps
// an import a hardlink instead of a copy.
//
// It is idempotent: an existing directory is reported, never rewritten or
// re-owned. The owner is the process — the chart runs the wiring Job as the
// release's puid:pgid — which is what `box init` tells the operator to expect.
// A path that is a file, or that cannot be created, stops the run with the path
// in the error; the directories made before it stay, which is safe because the
// next run is the same idempotent call and the data contract needs them anyway.
func EnsureDataDirs(instances []Instance) ([]DirResult, error) {
	var out []DirResult
	seen := map[string]bool{}
	for _, inst := range instances {
		for _, d := range []struct{ tree, path string }{
			{"downloads", inst.SavePath},
			{"media", inst.RootFolder},
		} {
			if seen[d.path] {
				continue
			}
			seen[d.path] = true
			res := DirResult{Name: "data/dirs/" + d.tree + "/" + inst.Category, Path: d.path}
			info, err := os.Stat(d.path)
			switch {
			case err == nil && !info.IsDir():
				return out, fmt.Errorf("create %s: it exists and is not a directory", d.path)
			case err == nil:
				out = append(out, res)
				continue
			case !errors.Is(err, fs.ErrNotExist):
				return out, fmt.Errorf("stat %s: %w", d.path, err)
			}
			if err := os.MkdirAll(d.path, 0o775); err != nil {
				return out, fmt.Errorf("create %s: %w", d.path, err)
			}
			res.Created = true
			out = append(out, res)
		}
	}
	return out, nil
}
