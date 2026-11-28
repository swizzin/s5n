package main

// qbtconfig_test.go drives the qBittorrent init container: the chart passes a variant's
// tuning here, and it must reach the file only where the file has none of its own.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/qbtconf"
)

func TestQbtConfigSeedsTheDiskQueueSizeOnlyWhereTheFileHasNone(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "qBittorrent.conf")
	args := func(size string) []string {
		return []string{"--conf", conf, "--password-file", password, "--disk-queue-size", size}
	}

	// A fresh box on a 10 Gbit libtorrent 2.1 build: the credential and the queue the
	// variant was measured with.
	if err := runQbtConfig(args("536870912")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	doc := qbtconf.Parse(data)
	if v, ok := doc.Get("BitTorrent", `Session\DiskQueueSize`); !ok || v != "536870912" {
		t.Fatalf("[BitTorrent] Session\\DiskQueueSize = %q (present %v), want 536870912:\n%s", v, ok, data)
	}

	// Any other variant passes no size, and the value the file holds stays as it is.
	if err := runQbtConfig(args("0")); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatalf("a run with no seed rewrote the file:\n%s", again)
	}
	if strings.Count(string(again), `Session\DiskQueueSize`) != 1 {
		t.Fatalf("the file holds more than one disk queue size:\n%s", again)
	}
}
