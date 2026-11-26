package linktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "apps", "qbittorrent")
	tests := []struct {
		name   string
		rel    string
		target string
		want   Result
	}{
		{
			"a link to a sibling stays as it is",
			filepath.Join("qBittorrent", "relative-link"), "qBittorrent.conf",
			Result{Target: "qBittorrent.conf"},
		},
		{
			"a link above its own directory, still inside",
			filepath.Join("logs", "link"), filepath.Join("..", "qBittorrent.conf"),
			Result{Target: filepath.Join("..", "qBittorrent.conf")},
		},
		{
			"an absolute link inside the tree is written relative",
			"absolute-link", filepath.Join(root, "qBittorrent", "qBittorrent.conf"),
			Result{Target: filepath.Join("qBittorrent", "qBittorrent.conf")},
		},
		{
			"a link to the tree's own root",
			filepath.Join("a", "link"), root,
			Result{Target: ".."},
		},
		{
			"a link to the kernel's stdout is left out",
			filepath.Join("logs", "qbittorrent.log"), "/proc/self/fd/1",
			Result{Skip: true},
		},
		{
			"a link into a device is left out",
			filepath.Join("logs", "null"), "/dev/null",
			Result{Skip: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(root, tt.rel, tt.target)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Resolve(%q, %q) = %+v, want %+v", tt.rel, tt.target, got, tt.want)
			}
		})
	}
}

func TestResolveRefusesWhatLeavesTheTree(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "apps", "qbittorrent")
	tests := []struct{ name, rel, target string }{
		{"a relative link that climbs out", filepath.Join("a", "link"), filepath.Join("..", "..", "etc", "passwd")},
		{"a relative link that lands on the parent", filepath.Join("a", "link"), ".." + string(filepath.Separator) + ".." + string(filepath.Separator) + ".."},
		{"an absolute link elsewhere on the host", "link", "/etc/passwd"},
		{"an absolute link next to the tree", "link", filepath.Join(root, "..", "sonarr", "config.xml")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(root, tt.rel, tt.target)
			if err == nil {
				t.Fatalf("Resolve accepted %q -> %q", tt.rel, tt.target)
			}
			// Both copies read this one sentence: the refusal says the tree, not the copy.
			if !strings.Contains(err.Error(), "outside the config directory") {
				t.Fatalf("the refusal does not say where the link goes:\n%s", err)
			}
		})
	}
}

func TestResolveLeavesARealLinkAlone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(root, "link", target)
	if err != nil || got.Skip || got.Target != "file" {
		t.Fatalf("Resolve = (%+v, %v), want the link kept as file", got, err)
	}
}
