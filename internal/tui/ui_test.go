package tui

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"charm.land/huh/v2"

	"github.com/swizzin/s5n/internal/setup"
)

// line drives one form the way huh's line-oriented mode does: a scripted reader
// and a buffer for what a person would see. Every test here asks one question,
// because each Prompter call is one form.
func line(input string) (*UI, *bytes.Buffer) {
	var out bytes.Buffer
	u := New(Options{In: strings.NewReader(input), Out: &out})
	u.accessible = true
	return u, &out
}

func TestInputTakesTheAnswerAndShowsTheChrome(t *testing.T) {
	u, out := line("  box.example.com  \n")
	u.Step(1, 3, "what domain will this box answer on?")
	got, err := u.Input(setup.Input{
		Key:      "gateway.host",
		Prompt:   "domain",
		Help:     "the name friends type",
		Default:  "box.example.com",
		Validate: func(s string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "box.example.com" {
		t.Fatalf("answer = %q, want the trimmed value", got)
	}
	text := stripANSI(out.String())
	// The line mode huh falls back to prints titles but not descriptions, so a
	// description is checked where it belongs: on the themed field in the
	// screenshots, and through the masthead copy above.
	for _, want := range []string{"b o x", "1/3", "what domain will this box answer on?", "domain"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the output is missing %q:\n%s", want, text)
		}
	}
}

func TestInputShowsTheFlowValidation(t *testing.T) {
	u, out := line("seedbox\nbox.example.com\n")
	got, err := u.Input(setup.Input{
		Key:    "gateway.host",
		Prompt: "domain",
		Validate: func(s string) error {
			if !strings.Contains(s, ".") {
				return errors.New("that is not a host name")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "box.example.com" {
		t.Fatalf("answer = %q, want the second try", got)
	}
	if !strings.Contains(stripANSI(out.String()), "that is not a host name") {
		t.Fatalf("the flow's error was not shown:\n%s", out.String())
	}
}

func TestConfirmUsesTheDefaultAndTheFlowLabels(t *testing.T) {
	u, out := line("\n")
	got, err := u.Confirm(setup.Confirm{
		Prompt:      "create /srv/s5n/data on that filesystem?",
		Default:     false,
		Affirmative: "create it",
		Negative:    "stop here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("an empty answer took the unsafe choice")
	}
	text := stripANSI(out.String())
	// Buttons are huh's normal mode; the line mode asks y/n with the default
	// marked, and an empty answer still has to take the safe choice.
	for _, want := range []string{"create /srv/s5n/data on that filesystem?", "[y/N]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the confirm is missing %q:\n%s", want, text)
		}
	}

	u, _ = line("y\n")
	if got, err := u.Confirm(setup.Confirm{Prompt: "go?"}); err != nil || !got {
		t.Fatalf("yes answered %v, %v", got, err)
	}
}

func TestSelectMapsValuesAndKeepsTheDefault(t *testing.T) {
	u, out := line("\n")
	got, err := u.Select(setup.Select{
		Key:    "kind",
		Prompt: "which kind?",
		Options: []setup.Option{
			{Value: "hd", Label: "high definition", Help: "smaller files"},
			{Value: "4k", Label: "4k", Help: "bigger files"},
		},
		Default: "4k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "4k" {
		t.Fatalf("answer = %q, want the default's value, not its label", got)
	}
	text := stripANSI(out.String())
	for _, want := range []string{"1. high definition  smaller files", "2. 4k               bigger files"} {
		if !strings.Contains(text, want) {
			t.Fatalf("option %q is not in the transcript:\n%s", want, text)
		}
	}
}

func TestMultiSelectStartsFromTheSelectedOptions(t *testing.T) {
	u, out := line("0\n")
	got, err := u.MultiSelect(setup.MultiSelect{
		Key:    "apps",
		Prompt: "which apps should this box have",
		Options: []setup.Option{
			{Value: "qbittorrent", Label: "qbittorrent", Help: "the download client"},
			{Value: "sonarr", Label: "sonarr", Help: "TV"},
			{Value: "radarr", Label: "radarr", Help: "films"},
		},
		Selected: []string{"qbittorrent", "radarr"},
		Min:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "qbittorrent,radarr" {
		t.Fatalf("answer = %v, want the preselected pair in catalog order", got)
	}
	// The hint is the field's description, which the line mode drops; the
	// checked marks are what it does show.
	if !strings.Contains(stripANSI(out.String()), "✓ qbittorrent") {
		t.Fatalf("the preselected option is not marked:\n%s", stripANSI(out.String()))
	}
}

func TestMultiSelectTogglesOneOn(t *testing.T) {
	u, _ := line("2\n")
	got, err := u.MultiSelect(setup.MultiSelect{
		Key: "apps",
		Options: []setup.Option{
			{Value: "qbittorrent", Label: "qbittorrent"},
			{Value: "sonarr", Label: "sonarr"},
			{Value: "radarr", Label: "radarr"},
		},
		Selected: []string{"qbittorrent", "radarr"},
		Min:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "qbittorrent,sonarr,radarr" {
		t.Fatalf("answer = %v, want sonarr added in catalog order", got)
	}
}

func TestTooFewNamesTheMinimum(t *testing.T) {
	if err := tooFew(2, 3); err != nil {
		t.Fatalf("three for a minimum of two: %v", err)
	}
	if err := tooFew(2, 1); err == nil || err.Error() != "at least 2 apps needed" {
		t.Fatalf("error = %v, want the line prompt's wording", err)
	}
	if err := tooFew(1, 0); err == nil || err.Error() != "at least 1 app needed" {
		t.Fatalf("error = %v, want the singular", err)
	}
}

func TestCancelMapsAHuhAbortOnlyOnce(t *testing.T) {
	if got := cancel(huh.ErrUserAborted); !errors.Is(got, setup.ErrCancelled) {
		t.Fatalf("huh's abort became %v, want setup.ErrCancelled", got)
	}
	if got := cancel(io.EOF); !errors.Is(got, io.EOF) {
		t.Fatalf("an unrelated error was rewritten: %v", got)
	}
}

func TestKeyMapQuitsOnEscAndCtrlC(t *testing.T) {
	keys := keyMap().Quit.Keys()
	if !contains(keys, "esc") || !contains(keys, "ctrl+c") {
		t.Fatalf("quit keys = %v, want esc and ctrl+c", keys)
	}
}

func TestStepMovesTheMasthead(t *testing.T) {
	u, out := line("x\n")
	u.Step(1, 4, "first")
	if _, err := u.Input(setup.Input{Key: "one", Prompt: "one"}); err != nil {
		t.Fatal(err)
	}
	u.Step(2, 4, "second")
	if _, err := u.Input(setup.Input{Key: "two", Prompt: "two"}); err != nil {
		t.Fatal(err)
	}
	text := stripANSI(out.String())
	if !strings.Contains(text, "2/4") || !strings.Contains(text, "second") {
		t.Fatalf("the second masthead is wrong:\n%s", text)
	}
	if !strings.Contains(text, "→ x") {
		t.Fatalf("the breadcrumb does not carry the first answer:\n%s", text)
	}
}

func TestPathCheckDrawsTreeAndWarning(t *testing.T) {
	var out bytes.Buffer
	u := New(Options{Out: &out})
	u.Step(2, 4, "where should your data and settings live?")
	err := u.ShowPathCheck(setup.PathCheck{
		Reports: []setup.PathReport{
			{
				Kind:       "data",
				Path:       "/srv/s5n/data",
				Mount:      setup.Mount{Major: 8, Minor: 1, Point: "/", FSType: "ext4", Source: "/dev/sda1"},
				Free:       900 * 1024 * 1024 * 1024,
				OnRoot:     true,
				SameDevice: true,
			},
			{
				Kind:    "config",
				Path:    "/srv/s5n/config",
				Exists:  true,
				Owner:   "1000:1000",
				Mount:   setup.Mount{Major: 8, Minor: 2, Point: "/srv", FSType: "btrfs", Source: "/dev/sdb1"},
				Free:    400 * 1024 * 1024 * 1024,
				Summary: "holds 2 entries at the top level: appdata, media\n          note: owned 0:0, not the box's 1000:1000; the apps cannot write there as it is",
			},
		},
		Footer: "  downloads/<category> and media/<category> are made on the first start, owned as above",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := stripANSI(out.String())
	for _, want := range []string{
		"2/4",
		"path check",
		"├ path",
		"├ exists",
		"no",
		"free space    900.0 GiB",
		"/dev/sda1",
		"⚠ mount point is / — writing here grows the system disk",
		"holds 2 entries at the top level: appdata, media",
		"note: owned 0:0, not the box's 1000:1000; the apps cannot write there as it is",
		"downloads/<category>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the path block is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "shares the root device") {
		t.Fatalf("a path on / is warned about twice:\n%s", text)
	}
	if !strings.Contains(text, "yes") {
		t.Fatalf("the existing path is not marked present:\n%s", text)
	}
}

func TestPathCheckSharesTheRootDeviceWarns(t *testing.T) {
	var out bytes.Buffer
	u := New(Options{Out: &out})
	err := u.ShowPathCheck(setup.PathCheck{Reports: []setup.PathReport{{
		Kind:       "config",
		Path:       "/srv/s5n/config",
		Mount:      setup.Mount{Major: 8, Minor: 1, Point: "/srv", FSType: "ext4"},
		Free:       10 << 30,
		SameDevice: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stripANSI(out.String()), "⚠ shares the root device (8:1) — filling it takes the host down") {
		t.Fatalf("the same-device warning is missing:\n%s", stripANSI(out.String()))
	}
}

func TestNoColorIsHonouredOnEveryLine(t *testing.T) {
	var out bytes.Buffer
	u := New(Options{Out: &out, NoColor: true})
	u.accessible = true
	u.Step(1, 3, "what domain will this box answer on?")
	if _, err := u.Input(setup.Input{Key: "gateway.host", Prompt: "domain", Default: "box.example.com"}); err != nil {
		t.Fatal(err)
	}
	shown := out.String()
	if strings.Contains(shown, "\x1b[38;2;") || strings.Contains(shown, "\x1b[48;2;") {
		t.Fatalf("NO_COLOR still emitted colour:\n%q", shown)
	}
	if !strings.Contains(stripANSI(shown), "b o x") {
		t.Fatalf("the plain masthead lost its wordmark:\n%s", stripANSI(shown))
	}
}

func TestOptionKeysPadTheHelpColumn(t *testing.T) {
	keys := optionKeys([]setup.Option{
		{Value: "a", Label: "qBittorrent", Help: "the download client"},
		{Value: "b", Label: "qui", Help: "torrent web ui"},
		{Value: "c", Label: "plex"},
	})
	if len(keys) != 3 {
		t.Fatalf("%d options, want 3", len(keys))
	}
	if keys[0].Key != "qBittorrent  the download client" {
		t.Fatalf("first key = %q", keys[0].Key)
	}
	if keys[1].Key != "qui          torrent web ui" {
		t.Fatalf("second key is not padded to the same column: %q", keys[1].Key)
	}
	if keys[2].Key != "plex" || keys[2].Value != "c" {
		t.Fatalf("an option without help should be its label alone: %+v", keys[2])
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
