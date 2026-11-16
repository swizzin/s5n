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

// boxHolds is what a command gives release.Options.Holds: the apps a rollback
// left on an older image, read from the file beside the grant's config root and
// overlaid onto the values the chart renders. A hold only lives while its
// release still pins the image the rollback avoided, so the same value names the
// holds a release has moved past, which box removes once an apply that rendered
// without them has succeeded.
type boxHolds struct {
	out say.Reporter
	// root and lapsed are this apply's: the config root it read and the holds
	// the release it applied has lapsed.
	root   string
	lapsed []update.Lapse
}

func newBoxHolds(out say.Reporter) *boxHolds { return &boxHolds{out: out} }

// values is release.Options.Holds. It runs after apply has loaded both files, so
// the config root comes from the grant it already parsed; reading the holds is
// the only second look, and it is a small file beside the snapshots.
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

// applyThenSettle runs one apply and, only when it succeeded, forgets the holds
// that release moved past. A failed or declined apply leaves every hold where it
// was: dropping one would move the app onto the image its rollback avoided.
func applyThenSettle(ctx context.Context, apply func(context.Context, release.Options) (release.Result, error), opts release.Options, holds *boxHolds) (release.Result, error) {
	res, err := apply(ctx, opts)
	if err != nil {
		return res, err
	}
	holds.applied()
	return res, nil
}

// applied removes the holds this apply's release moved past. It is called only
// after an apply returned without error: a hold dropped by a run whose apply
// failed would move the app onto the very image its rollback avoided. A hold that
// cannot be removed stays, and box says so -- the app is still held, which is
// the safe half of the failure.
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

// lapsedApps names the apps in one of box's sentences.
func lapsedApps(lapses []update.Lapse) string {
	apps := make([]string, 0, len(lapses))
	for _, lapse := range lapses {
		apps = append(apps, lapse.Hold.App)
	}
	return strings.Join(apps, ", ")
}
