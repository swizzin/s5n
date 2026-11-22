package plan

import (
	"strings"
	"testing"
)

// foldersPlan is a plan for a source whose library is called the names its
// Arrs and Plex gave it: box's own layout for the same seedbox is movies,
// movies-4k, tv and tv-4k, so every one of them is a rename.
const foldersPlan = `version: 2
data:
  root: /srv/library
  downloads: Downloads
  media: library
  folders:
    - from: Movies
      to: movies
    - from: 4K/Movies
      to: movies-4k
    - from: TV
      to: tv
apps:
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    category: movies
    downloadCategory: Movies
    paths:
      - from: /library/Movies
        to: /data/media/movies
  radarr-4k:
    kind: kubernetes
    source: radarr4k
    config: /srv/apps/radarr4k
    category: movies-4k
    downloadCategory: 4K/Movies
    paths:
      - from: /library/4K/Movies
        to: /data/media/movies-4k
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    category: tv
    paths:
      - from: /library/TV
        to: /data/media/tv
`

// TestDataMediaRenames: the library's own directories are renamed onto the
// categories the plan's apps have, and the pair is what a summary prints.
func TestDataMediaRenames(t *testing.T) {
	p := mustParse(t, foldersPlan)
	got := p.Data.MediaRenames()
	want := [][2]string{{"Movies", "movies"}, {"4K/Movies", "movies-4k"}, {"TV", "tv"}}
	if len(got) != len(want) {
		t.Fatalf("MediaRenames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MediaRenames() = %v, want %v", got, want)
		}
	}
	// The two contract directories are renamed as they always were, and the
	// library is one of them: the folders are steps inside it.
	if renames := p.Data.Renames(); len(renames) != 2 || renames[1] != [2]string{"library", "media"} {
		t.Errorf("Renames() = %v, want Downloads -> downloads and library -> media", renames)
	}
}

// TestDataMediaRenamesAreAFirstRunNoOp: a source already in box's layout needs
// no folder renamed, which is what makes a re-run and a box-named source
// change nothing on disk.
func TestDataMediaRenamesAreAFirstRunNoOp(t *testing.T) {
	p := mustParse(t, `version: 2
data:
  root: /srv/library
  downloads: downloads
  media: media
  folders:
    - from: movies
      to: movies
apps:
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    category: movies
`)
	if got := p.Data.Renames(); len(got) != 0 {
		t.Fatalf("a box-named source is renamed: %v", got)
	}
	if got := p.Data.MediaRenames(); len(got) != 0 {
		t.Fatalf("a box-named library is renamed: %v", got)
	}
}

// TestParseRefusesBadMediaFolders: the mapping is a rename of the operator's
// library, so every way it could move the wrong directory is a refusal that
// names the entry.
func TestParseRefusesBadMediaFolders(t *testing.T) {
	base := "version: 2\napps:\n  radarr:\n    kind: kubernetes\n    source: radarr\n    config: /srv/r\n    category: movies\n" +
		"data:\n  root: /srv/library\n  downloads: Downloads\n  media: library\n"
	tests := []struct{ name, extra, want string }{
		{"a source that is not under the media directory", "folders: [{from: /Movies, to: movies}]\n", "must be a directory under the media directory"},
		{"a source that leaves it", "folders: [{from: ../Other, to: movies}]\n", "must be a directory under the media directory"},
		{"a category that is not under it", "folders: [{from: Movies, to: /data/media/movies}]\n", "must be a directory under the media directory"},
		{"no source", "folders: [{to: movies}]\n", "from is missing"},
		{"no category", "folders: [{from: Movies}]\n", "to is missing"},
		{"one source twice", "folders: [{from: Movies, to: movies}, {from: Movies, to: movies-4k}]\n", `from "Movies" appears twice`},
		{"two sources onto one category", "folders: [{from: Movies, to: movies}, {from: Films, to: movies}]\n", "two source directories become \"movies\""},
		{"a category no instance has", "folders: [{from: Movies, to: films}]\n", `to "films" is no app's category`},
		{"a source inside another", "folders: [{from: 4K, to: movies}, {from: 4K/Movies, to: movies-4k}]\n", `"4K/Movies" is inside`},
		{"a default with no reason", "folders: [{from: Movies, to: movies, default: true}]\n", "needs a why"},
		{"a reason across two lines", "folders: [{from: Movies, to: movies, why: \"a\\nb\"}]\n", "must be one line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(base + "  " + strings.TrimSuffix(tt.extra, "\n")))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestParseAcceptsTwoNestedSources: the two 4K instances' libraries sit under
// one parent, and neither is inside the other, so both are renames.
func TestParseAcceptsTwoNestedSources(t *testing.T) {
	_, err := Parse([]byte(`version: 2
apps:
  radarr-4k:
    kind: kubernetes
    source: radarr4k
    config: /srv/r4k
    category: movies-4k
  sonarr-4k:
    kind: kubernetes
    source: sonarr4k
    config: /srv/s4k
    category: tv-4k
data:
  root: /srv/library
  downloads: Downloads
  media: library
  folders:
    - {from: 4K/Movies, to: movies-4k}
    - {from: 4K/TV, to: tv-4k}
`))
	if err != nil {
		t.Errorf("two nested source directories under one parent are two renames, and Parse refused them: %v", err)
	}
}

// TestParseRefusesBadCategories: a category is a path segment box lays out
// itself, so a value that is not one is refused where it is written.
func TestParseRefusesBadCategories(t *testing.T) {
	base := "version: 2\napps:\n  radarr:\n    kind: kubernetes\n    source: radarr\n    config: /srv/r\n"
	tests := []struct{ name, extra, want string }{
		{"an absolute category", "    category: /movies\n", "must not start or end with /"},
		{"a trailing slash", "    category: movies/\n", "must not start or end with /"},
		{"an empty segment", "    category: 4K//Movies\n", "has an empty segment"},
		{"a parent", "    category: ..\n", "is not a directory name"},
		{"a backslash", "    category: 'a\\\\b'\n", "backslash"},
		{"a download category with a newline", "    downloadCategory: \"a\\nb\"\n", "backslash or a newline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(base + tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
	// A subcategory is one category with two segments, and both keys accept it.
	if _, err := Parse([]byte(base + "    category: 4K/Movies\n    downloadCategory: 4K/Movies\n")); err != nil {
		t.Errorf("a subcategory is a category, and Parse refused it: %v", err)
	}
}
