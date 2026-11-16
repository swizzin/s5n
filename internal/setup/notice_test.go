package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/hostconf"
)

// noticeFile is root's config as bootstrap writes it, at a path a test can own.
func noticeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), hostconf.FileName)
	if err := hostconf.Write(path, hostconf.Template()); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInitAsksForTheNoticeAddressLastAndWritesIt: root's file is where box says
// what happened, and init is the one run that asks. The answer goes into the one
// url line and every other byte of the file is kept.
func TestInitAsksForTheNoticeAddressLastAndWritesIt(t *testing.T) {
	h := newHarness(t)
	path := noticeFile(t)
	before := h.read(path)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	// The first secret is the login's password (empty: box makes one), the
	// last is the address this test is about.
	h.prompt.secrets = []string{"", "https://hooks.example.test/services/T00/B00/token"}

	opts := h.options()
	opts.NoticePath = path
	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}

	// It was the last question the run asked.
	if len(h.prompt.events) == 0 || h.prompt.events[len(h.prompt.events)-1] != "secret:notices.url" {
		t.Fatalf("the notice question was not last: %v", h.prompt.events)
	}
	cfg, _, err := hostconf.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NoticeURL != "https://hooks.example.test/services/T00/B00/token" {
		t.Fatalf("NoticeURL = %q, want the answer", cfg.NoticeURL)
	}
	// Every other line is byte-identical.
	after := h.read(path)
	for _, line := range strings.Split(before, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "url:") {
			continue
		}
		if !strings.Contains(after, line) {
			t.Fatalf("init changed a line of root's file: %q is gone from\n%s", line, after)
		}
	}
	// What init says about it is the host, never the token.
	if strings.Contains(h.out.String(), "T00/B00") {
		t.Fatalf("init printed the address:\n%s", h.out)
	}
	if !strings.Contains(h.out.String(), "notices go to https://hooks.example.test") {
		t.Fatalf("init did not say where notices go:\n%s", h.out)
	}
}

// TestInitSkipsAnEmptyNoticeAddress: Enter is a real answer -- no notices -- and
// it leaves root's file exactly as bootstrap wrote it.
func TestInitSkipsAnEmptyNoticeAddress(t *testing.T) {
	h := newHarness(t)
	path := noticeFile(t)
	before := h.read(path)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.prompt.secrets = []string{"", ""}

	opts := h.options()
	opts.NoticePath = path
	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if got := h.read(path); got != before {
		t.Fatalf("an empty answer rewrote root's file:\n%s", got)
	}
}

// TestInitDoesNotAskWhenTheAddressIsSet: a re-run with an address asks nothing
// about it, and the file is not rewritten.
func TestInitDoesNotAskWhenTheAddressIsSet(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, path string)
		opts  func(o *Options)
	}{
		{
			name: "the address is already there",
			setup: func(t *testing.T, path string) {
				data, err := hostconf.SetNoticeURL(hostconf.Template(), "https://hooks.example.test/x")
				if err != nil {
					t.Fatal(err)
				}
				if err := hostconf.Write(path, data); err != nil {
					t.Fatal(err)
				}
			},
			opts: func(o *Options) {},
		},
		{
			name:  "bootstrap wrote no file",
			setup: func(t *testing.T, path string) { _ = os.Remove(path) },
			opts:  func(o *Options) {},
		},
		{
			name:  "an unattended run asks nothing",
			setup: func(t *testing.T, path string) {},
			opts:  func(o *Options) { *o = o.unattendedForTest() },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			path := noticeFile(t)
			c.setup(t, path)
			opts := h.options()
			opts.NoticePath = path
			c.opts(&opts)
			if opts.Unattended {
				h.prompt.inputs = nil
				h.prompt.multis = nil
			} else {
				h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
				h.prompt.multis = [][]string{{"alpha"}}
			}
			if _, err := h.run(opts); err != nil {
				t.Fatalf("init: %v\n%s", err, h.out)
			}
			for _, e := range h.prompt.events {
				if e == "secret:notices.url" {
					t.Fatal("init asked for a notice address it did not need")
				}
			}
		})
	}
}

// TestACancelledNoticeQuestionWritesNothing: the question is init's last, so a
// cancel there leaves both of the seedbox's files and root's alone.
func TestACancelledNoticeQuestionWritesNothing(t *testing.T) {
	h := newHarness(t)
	path := noticeFile(t)
	before := h.read(path)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.prompt.cancelAt = "secret"

	opts := h.options()
	opts.NoticePath = path
	if _, err := h.run(opts); err == nil {
		t.Fatal("a cancelled question still ran init")
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("a cancelled question wrote one of the seedbox's files")
	}
	if got := h.read(path); got != before {
		t.Fatal("a cancelled question wrote root's config")
	}
	if !strings.Contains(h.out.String(), "oh heck, nvm") {
		t.Fatalf("init did not say it stopped:\n%s", h.out)
	}
}

// unattendedForTest is a fully answered --yes run, for a subtest that needs one.
func (o Options) unattendedForTest() Options {
	o.Unattended = true
	o.Domain = "box.example.com"
	o.DataPath = "/srv/s5n/data"
	o.ConfigPath = "/srv/s5n/config"
	o.Apps = []string{"alpha"}
	return o
}

// TestAFilesTypoStopsInitBeforeTheFirstQuestion: init reads root's config the
// way every other run does -- strictly -- and stops naming file, line and key,
// before it asks anything. Nothing is asked, nothing is written: a run that
// cannot read the file it would write the notice address into has no business
// asking about anything else first.
func TestAFilesTypoStopsInitBeforeTheFirstQuestion(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), hostconf.FileName)
	if err := os.WriteFile(path, []byte("update:\n  timmer: on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	opts := h.options()
	opts.NoticePath = path
	_, err := h.run(opts)
	if err == nil {
		t.Fatal("init ran with a config it cannot parse")
	}
	if !strings.Contains(err.Error(), `box.yaml:2: unknown key "update.timmer"`) {
		t.Fatalf("init did not name the file, line and key: %v", err)
	}
	if len(h.prompt.events) != 0 {
		t.Fatalf("init asked %v before refusing root's file", h.prompt.events)
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("init wrote one of the seedbox's files")
	}
	// And the file is exactly as it was: init never rewrites it.
	if got := h.read(path); got != "update:\n  timmer: on\n" {
		t.Fatalf("init rewrote root's file:\n%s", got)
	}
}
