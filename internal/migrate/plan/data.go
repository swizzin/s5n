package plan

import (
	"fmt"
	"path"
	"strings"
)

// The directories box's data contract names under the data root: qBittorrent
// saves under the first, the Arrs keep their root folders under the second
// (charts/s5n/values.yaml, "The data contract").
const (
	DownloadsDir = "downloads"
	MediaDir     = "media"
)

// Data is where the source kept its downloads and media, when box adopts them
// where they are. Data never moves between filesystems: hardlinks between a
// download and its imported copy survive only a rename, so the root becomes
// box's data path as it is, and the two directories under it are renamed to
// the names box's contract uses when they differ.
//
//	data:
//	  root: /srv/library
//	  downloads: Downloads      # renamed to downloads
//	  media: media              # already the contract's name
type Data struct {
	// Root is the host directory that becomes the seedbox's data path.
	Root string `yaml:"root"`
	// Downloads is the directory under Root the source downloaded into.
	Downloads string `yaml:"downloads"`
	// Media is the directory under Root the source kept its library in.
	Media string `yaml:"media"`
}

// Renames is what apply renames under Root, source name to contract name, in a
// fixed order. A directory already named as the contract wants is not listed.
func (d *Data) Renames() [][2]string {
	var out [][2]string
	for _, r := range [][2]string{{d.Downloads, DownloadsDir}, {d.Media, MediaDir}} {
		if r[0] != r[1] {
			out = append(out, r)
		}
	}
	return out
}

// checkData enforces that the renames are plain, single-segment and can't
// collide: a rename is the one write migrate makes to the data, so it has to
// be the obvious one.
func (p *Plan) checkData() []string {
	d := p.Data
	if d == nil {
		return nil
	}
	var problems []string
	if !path.IsAbs(d.Root) {
		problems = append(problems, fmt.Sprintf("data: root %q is not an absolute path", d.Root))
	} else {
		d.Root = path.Clean(d.Root)
		if d.Root == "/" {
			problems = append(problems, "data: root is /; name the directory that holds the downloads and the media")
		}
	}
	for _, f := range []struct{ key, val string }{{"downloads", d.Downloads}, {"media", d.Media}} {
		switch {
		case f.val == "":
			problems = append(problems, fmt.Sprintf("data: %s is missing; name the directory under root", f.key))
		case f.val == "." || f.val == ".." || strings.Contains(f.val, "/"):
			problems = append(problems, fmt.Sprintf("data: %s %q must be one directory directly under root", f.key, f.val))
		}
	}
	if d.Downloads != "" && d.Downloads == d.Media {
		problems = append(problems, fmt.Sprintf("data: downloads and media are both %q", d.Downloads))
	}
	// Renaming downloads to "media" or media to "downloads" would have the
	// first rename land on the second's source.
	if d.Downloads == MediaDir || d.Media == DownloadsDir {
		problems = append(problems, "data: downloads and media are swapped relative to box's names; rename one by hand first")
	}
	return problems
}
