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
// The library's own directories are box's categories after a migration, so
// `folders` names the one source directory each category is: a source that
// called its films media/Movies ends up with media/movies, and the Arrs, Plex
// and everything else that stored the old name is rewritten to the new one.
// The downloads are not touched: a category there keeps its name and its
// torrents stay where they are.
//
//	data:
//	  root: /srv/library
//	  downloads: Downloads      # renamed to downloads
//	  media: media              # already the contract's name
//	  folders:
//	    - from: Movies          # media/Movies becomes media/movies
//	      to: movies
type Data struct {
	// Root is the host directory that becomes the seedbox's data path.
	Root string `yaml:"root"`
	// Downloads is the directory under Root the source downloaded into.
	Downloads string `yaml:"downloads"`
	// Media is the directory under Root the source kept its library in.
	Media string `yaml:"media"`
	// Folders is one entry per library directory the source had, mapped onto
	// the category the box instance's media lives in. A directory already
	// named as its category is listed too, and changes nothing.
	Folders []MediaFolder `yaml:"folders,omitempty"`
}

// MediaFolder is one library directory under Data.Media and the box category
// it becomes. From and To are both relative to the media directory, and at most
// one '/' deep, because a category may be a qBittorrent subcategory
// (4K/Movies): a rename moves one directory, never a tree.
//
// To is a category some app in the plan has (Plan.Apps[*].Category), which is
// what makes it the directory box's own release, its wiring and its Plex
// library all expect.
type MediaFolder struct {
	// From is the directory as the source named it, relative to Data.Media.
	From string `yaml:"from"`
	// To is the box category the directory becomes, relative to the media
	// directory.
	To string `yaml:"to"`
	// Why is one line saying why discovery settled this mapping when it is a
	// default: the root folders it had to choose between, and what it took.
	Why string `yaml:"why,omitempty"`
	// Default says discovery chose this mapping, and the operator has not
	// confirmed it. A default without a Why is a decision with no reason.
	Default bool `yaml:"default,omitempty"`
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

// MediaRenames is the library directories apply renames inside the media
// directory, as the pairs a summary prints: the source's name and the category
// the instance's media lives in. A directory already named as its category is
// not listed, which is what makes a source already in box's layout a no-op.
//
// They happen after Renames, so both sides are under the media directory's
// contract name whichever the source used.
func (d *Data) MediaRenames() [][2]string {
	var out [][2]string
	for _, f := range d.Folders {
		if f.From == "" || f.From == f.To {
			continue
		}
		out = append(out, [2]string{f.From, f.To})
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
	return append(problems, p.checkFolders()...)
}

// checkFolders enforces that the library mapping says one thing about one
// directory: one entry per source directory, one entry per category, and a
// category an app in the plan actually has.
//
// The mapping is a rename, and a rename is the only write migrate makes to the
// data, so it has to be the obvious one: two source directories becoming one
// category would have the second land on the first, and a category no instance
// has is a directory box would never mount or wire.
func (p *Plan) checkFolders() []string {
	d := p.Data
	if d == nil {
		return nil
	}
	var problems []string
	seenFrom := make(map[string]bool, len(d.Folders))
	seenTo := make(map[string]bool, len(d.Folders))
	for i := range d.Folders {
		f := &d.Folders[i]
		at := fmt.Sprintf("data.folders[%d]", i)
		f.From, f.To = path.Clean(f.From), path.Clean(f.To)
		for _, side := range []struct{ key, val string }{{"from", f.From}, {"to", f.To}} {
			switch {
			case side.val == "" || side.val == ".":
				problems = append(problems, fmt.Sprintf("%s: %s is missing; name the directory under the media directory", at, side.key))
			case path.IsAbs(side.val) || strings.HasPrefix(side.val, ".."):
				problems = append(problems, fmt.Sprintf("%s: %s %q must be a directory under the media directory", at, side.key, side.val))
			}
		}
		if f.From != "" {
			if seenFrom[f.From] {
				problems = append(problems, fmt.Sprintf("%s: from %q appears twice", at, f.From))
			}
			seenFrom[f.From] = true
		}
		if f.To != "" {
			if seenTo[f.To] {
				problems = append(problems, fmt.Sprintf("%s: two source directories become %q; one category is one directory", at, f.To))
			}
			seenTo[f.To] = true
			if !p.hasCategory(f.To) {
				problems = append(problems, fmt.Sprintf("%s: to %q is no app's category in this plan (it has %s), so nothing would be wired to it",
					at, f.To, orNone(p.categories())))
			}
		}
		problems = append(problems, whyProblems(at, f.Why, f.Default)...)
	}
	// A directory inside another one is two renames that cannot both happen:
	// moving the outer one first carries the inner one with it, and the inner
	// one's rename then names nothing.
	for i := range d.Folders {
		for j := range d.Folders {
			a, b := d.Folders[i].From, d.Folders[j].From
			if i == j || a == "" || b == "" || a == b {
				continue
			}
			if strings.HasPrefix(a, b+"/") {
				problems = append(problems, fmt.Sprintf("data.folders[%d]: from %q is inside data.folders[%d]'s %q, and both cannot be renamed", i, a, j, b))
			}
		}
	}
	return problems
}

// hasCategory reports whether one of the plan's apps keeps its library in this
// category.
func (p *Plan) hasCategory(category string) bool {
	for _, app := range p.Apps {
		if app.Category == category {
			return true
		}
	}
	return false
}

// categories is every category the plan's apps have, sorted, as a refusal lists
// them.
func (p *Plan) categories() []string {
	var out []string
	for _, name := range p.appNames() {
		if c := p.Apps[name].Category; c != "" {
			out = append(out, c)
		}
	}
	return out
}

// orNone renders a list, or the words for an empty one.
func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
