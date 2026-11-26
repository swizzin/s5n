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

// EnsureDataDirs creates every instance's download and library directory, before any
// application is asked anything.
//
// The order is the contract: Sonarr and Radarr refuse a root folder whose directory
// does not exist, so an empty data path needs /data/media/<category> first and
// nothing else makes it. Both trees are provisioned together under the one /data
// mount, which is what keeps an import a hardlink.
//
// It is idempotent: an existing directory is reported, never rewritten or re-owned.
// A path that is a file, or cannot be created, stops the run; the directories made
// before it stay, because the next run is the same idempotent call.
func EnsureDataDirs(instances []Instance) ([]DirResult, error) {
	var out []DirResult
	seen := map[string]bool{}
	for _, inst := range instances {
		// The library's category and the download category, which a migrated box
		// keeps from its source.
		for _, d := range []struct{ tree, name, path string }{
			{"downloads", downloadCategory(inst), inst.SavePath},
			{"media", inst.Category, inst.RootFolder},
		} {
			if seen[d.path] {
				continue
			}
			seen[d.path] = true
			res := DirResult{Name: "data/dirs/" + d.tree + "/" + d.name, Path: d.path}
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
