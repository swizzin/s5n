package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// HoldsName is the file beside the snapshots that records which apps this box holds
// at an older image.
const HoldsName = "holds.json"

// The two things that write a hold: an update step that rolled an app back itself,
// and an operator's `box rollback`.
const (
	ByUpdate   = "update"
	ByRollback = "rollback"
)

// Hold is one app held at the image it ran before a step moved it, and what the
// chart's pin is compared against on every later step: while the chart still pins At
// the app stays there, and once a release pins something else the hold has lapsed.
// Avoiding is recorded so a hold can tell "the chart moved on" from "the chart still
// pins the image this app was rolled back from".
//
// The record lives with the snapshots rather than in the values file, so a seedbox's
// own box can write one without a write to the release.
type Hold struct {
	// App is the app key (sonarr, not sonarr-4k): the hold overlays
	// apps.<app>.image, which the chart keeps per app and instances share.
	App string `json:"app"`
	// At is the image the app ran before the step, and Avoiding the image the
	// step moved it to.
	At       ImageRef `json:"at"`
	Avoiding ImageRef `json:"avoiding"`
	// Snapshot is the step directory's name, which is how a rollback finds the
	// other app's copies of the same step.
	Snapshot string `json:"snapshot"`
	// Since is when the hold was written, in UTC.
	Since time.Time `json:"since"`
	// By is what wrote it: ByUpdate or ByRollback.
	By string `json:"by"`
}

// holdsFile is the file's shape: one object with one key, so a file that grows a
// field later cannot be mistaken for a list of holds.
type holdsFile struct {
	Holds []Hold `json:"holds"`
}

// HoldsPath is where a seedbox's holds live: <config root>/.box/holds.json, beside
// the snapshots they refer to, under a directory the seedbox's own box can write.
func HoldsPath(configRoot string) string {
	return filepath.Join(configRoot, boxDir, HoldsName)
}

// LoadHolds reads the box's holds, in app order. A box that holds nothing has no
// file, which is not an error; anything else is an error naming the path: a file
// box cannot parse, an unknown field, two holds for one app. Guessing here drops a
// hold, and a dropped hold moves an app onto the very image its rollback avoided.
func LoadHolds(configRoot string) ([]Hold, error) {
	path := HoldsPath(configRoot)
	t, err := openTree(configRoot)
	if err != nil {
		// A config root that is not there holds nothing, like a box with no hold
		// file.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("holds %s: %w", path, err)
	}
	defer t.Close()
	name, err := t.rel(path)
	if err != nil {
		return nil, fmt.Errorf("holds %s: %w", path, err)
	}
	data, err := t.readFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("holds %s: %w", path, err)
	}
	var doc holdsFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("holds %s: %w", path, err)
	}
	// A second document after the first is trailing junk: only io.EOF means the
	// file ended where the object did.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("holds %s: holds more than the one document", path)
	}
	holds, err := checkedHolds(doc.Holds)
	if err != nil {
		return nil, fmt.Errorf("holds %s: %w", path, err)
	}
	for i := range holds {
		holds[i].Since = holds[i].Since.UTC()
	}
	return holds, nil
}

// SaveHolds writes the box's holds, sorted by app and at most one per app, through
// internal/atomicfile so a crash leaves the file as it was or complete. An empty
// list removes the file: its absence is what LoadHolds reads as none.
func SaveHolds(configRoot string, holds []Hold) error {
	path := HoldsPath(configRoot)
	checked, err := checkedHolds(holds)
	if err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	t, err := openTree(configRoot)
	if err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	defer t.Close()
	name, err := t.rel(path)
	if err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	if len(checked) == 0 {
		if err := t.remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("holds %s: %w", path, err)
		}
		return nil
	}
	// A link where the hold file goes is a refusal: box writes this file itself, and
	// the user can only have put a link there to point box somewhere. The rename would
	// safely replace the link, which is still not what the operator asked for.
	info, err := t.lstat(name)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("holds %s: it is a symlink, and box writes the hold file itself; remove it and box will write a new one", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("holds %s: %w", path, err)
	}
	own, err := treeOwnerOf(t)
	if err != nil {
		return err
	}
	if err := makeDirIn(t, filepath.Dir(name), own); err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	for i := range checked {
		checked[i].Since = checked[i].Since.UTC()
	}
	data, err := json.MarshalIndent(holdsFile{Holds: checked}, "", "  ")
	if err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	if err := atomicfile.WriteIn(t.root, name, append(data, '\n'), treeFileMode); err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	if err := own.ownIn(t, name); err != nil {
		return fmt.Errorf("holds %s: %w", path, err)
	}
	return nil
}

// WithHold puts one app's hold in place of any it had, in app order: a hold is never
// added beside the one it replaces, or two of them would leave box whichever it read
// first.
func WithHold(holds []Hold, h Hold) []Hold {
	out := WithoutHold(holds, h.App)
	out = append(out, h)
	sortHolds(out)
	return out
}

// WithoutHold drops one app's hold and keeps the rest in order. Removing a hold an
// app does not have is not a change.
func WithoutHold(holds []Hold, app string) []Hold {
	out := make([]Hold, 0, len(holds))
	for _, h := range holds {
		if h.App != app {
			out = append(out, h)
		}
	}
	return out
}

// checkedHolds copies a list, sorts it by app and refuses anything box could not act
// on: a name that is not an instance name, half an image reference, a step that is
// not a name, an unknown writer, two holds for one app.
func checkedHolds(holds []Hold) ([]Hold, error) {
	out := make([]Hold, len(holds))
	copy(out, holds)
	sortHolds(out)
	for i, h := range out {
		if err := checkHold(h); err != nil {
			return nil, err
		}
		if i > 0 && out[i-1].App == h.App {
			return nil, fmt.Errorf("%s is held twice; a box keeps one hold per app", h.App)
		}
	}
	return out, nil
}

// checkHold refuses one hold box could not apply, or could apply to the wrong app.
func checkHold(h Hold) error {
	if err := checkApp(h.App); err != nil {
		return fmt.Errorf("app %q: %w", h.App, err)
	}
	if err := checkImage(h.At); err != nil {
		return fmt.Errorf("app %q: at: %w", h.App, err)
	}
	if err := checkImage(h.Avoiding); err != nil {
		return fmt.Errorf("app %q: avoiding: %w", h.App, err)
	}
	if err := checkName(h.Snapshot); err != nil {
		return fmt.Errorf("app %q: snapshot: %w", h.App, err)
	}
	switch h.By {
	case ByUpdate, ByRollback:
	default:
		return fmt.Errorf("app %q: by %q is neither %q nor %q", h.App, h.By, ByUpdate, ByRollback)
	}
	return nil
}

// checkApp refuses a name a hold may carry: one path element in the catalog's shape
// for an instance (sonarr-4k). It is the config directory a rollback reads and
// writes, so anything else is refused rather than joined into a path.
func checkApp(app string) error {
	if err := checkName(app); err != nil {
		return err
	}
	for _, seg := range strings.Split(app, "-") {
		if seg == "" {
			return errors.New("has an empty segment")
		}
		for _, r := range seg {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				continue
			}
			return fmt.Errorf("has %q; an instance name is lowercase letters and digits, hyphen-separated (sonarr-4k)", r)
		}
	}
	return nil
}

// sortHolds orders holds by app, so two runs that hold the same apps write the same
// bytes.
func sortHolds(holds []Hold) {
	sort.Slice(holds, func(i, j int) bool { return holds[i].App < holds[j].App })
}
