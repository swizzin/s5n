package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/say"
)

type dbgValues struct{}

func (dbgValues) Enable(string, string) error { return nil }

func TestDbgStagedTree(t *testing.T) {
	path, _ := plexFullSnapshot(t)
	var buf bytes.Buffer
	opened, cleanup, err := openSnapshot(path, &say.Plain{W: &buf})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	p, err := snapshotPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	env := apply.Env{
		ConfigRoot: root,
		Configs:    filepath.Join(opened.Dir, "config"),
		Bulk:       snapshotBulk{opened: opened},
		Catalog:    []string{"plex"},
		PUID:       os.Geteuid(),
		PGID:       os.Getegid(),
		Targets:    map[string]rewrite.Endpoint{"plex": {Host: "plex", Port: 32400}},
		Enabled:    func(string) bool { return false },
		Confirm:    func(apply.Summary) (bool, error) { return true, nil },
		Values:     dbgValues{},
		Run: func(ctx context.Context, app string, argv []string, stdin []byte) ([]byte, error) {
			var lines []string
			_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				rel, _ := filepath.Rel(root, p)
				lines = append(lines, fmt.Sprintf("%s %v", rel, info.Mode()))
				return nil
			})
			sort.Strings(lines)
			t.Logf("staged at check:\n%s", joinLines(lines))
			return []byte("@end|1\n"), nil
		},
	}
	if _, err := apply.Run(context.Background(), p, opened.Plan, env); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func joinLines(s []string) string {
	out := ""
	for _, l := range s {
		out += "  " + l + "\n"
	}
	return out
}
