package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// boxHolds is release.Options.Holds: apps kept on an older image, overlaid onto
// the values. A hold lives while its release pins the avoided image; lapsed ones
// are removed after a successful apply.
type boxHolds struct {
	out say.Reporter
	// root and lapsed are this apply's config root and lapsed holds.
	root   string
	lapsed []update.Lapse
}

func newBoxHolds(out say.Reporter) *boxHolds { return &boxHolds{out: out} }

// values reads holds after apply loaded the files; the config root is the
// grant's, and the holds file is small.
func (b *boxHolds) values(_ context.Context, sb *seedbox.Seedbox, chart release.Chart, values map[string]any) (map[string]any, error) {
	root, err := update.ConfigRoot(sb)
	if err != nil {
		return nil, err
	}
	holds, err := update.FileHolds{}.Load(root)
	if err != nil {
		return nil, err
	}
	overlay, lapsed, err := update.HoldValues(chart, values, holds)
	if err != nil {
		return nil, err
	}
	b.root, b.lapsed = root, lapsed
	return overlay, nil
}

// applyThenSettle forgets lapsed holds only after success: dropping one on a
// failure would move the app onto the avoided image.
func applyThenSettle(ctx context.Context, apply func(context.Context, release.Options) (release.Result, error), opts release.Options, holds *boxHolds) (release.Result, error) {
	res, err := apply(ctx, opts)
	if err != nil {
		return res, err
	}
	holds.applied()
	return res, nil
}

// applied removes lapsed holds, only after a successful apply. A hold that
// cannot be removed stays and box says so: being held is the safe half.
func (b *boxHolds) applied() {
	if len(b.lapsed) == 0 {
		return
	}
	holds, err := update.FileHolds{}.Load(b.root)
	if err != nil {
		b.out.Warn(fmt.Sprintf("keeping the hold on %s: %v", lapsedApps(b.lapsed), err))
		return
	}
	for _, lapse := range b.lapsed {
		holds = snapshot.WithoutHold(holds, lapse.Hold.App)
	}
	if err := (update.FileHolds{}).Save(b.root, holds); err != nil {
		b.out.Warn(fmt.Sprintf("keeping the hold on %s: %v", lapsedApps(b.lapsed), err))
		return
	}
	for _, lapse := range b.lapsed {
		b.out.Muted(lapse.Text)
	}
	b.lapsed = nil
}

// lapsedApps names the apps for a sentence.
func lapsedApps(lapses []update.Lapse) string {
	apps := make([]string, 0, len(lapses))
	for _, lapse := range lapses {
		apps = append(apps, lapse.Hold.App)
	}
	return strings.Join(apps, ", ")
}
