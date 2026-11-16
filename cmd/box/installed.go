package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/release"
)

// applyRelease is the chart release one `box apply` installs.
//
// apply keeps the release the box is on: `box update` is the command that moves
// a release, and a move needs the upgrade-from floors, the copy each step takes
// of the apps it moves and the health gate behind it. An apply that pulled the
// newest published release would make that move -- and skip all three -- because
// somebody edited a value, so a bare apply asks the cluster which release is
// installed and asks the chart source for exactly that one.
//
// Nothing installed yet is the one case with no release to keep: a first apply
// installs the newest this box can use, which is what it has always done.
//
// A --chart-version is the operator naming a release, and apply cannot honour
// it: installing anything but the installed release is a move. It is refused
// here, before the chart is loaded and so before the plan, the question and any
// write.
func applyRelease(ctx context.Context, opts release.Options, installed func(context.Context) (release.ReleaseState, error)) (release.Options, error) {
	state, err := installed(ctx)
	if err != nil {
		return opts, err
	}
	if !state.Exists {
		return opts, nil
	}
	want := strings.TrimSpace(opts.ChartVersion)
	if want == "" {
		opts.ChartVersion = state.ChartVersion
		return opts, nil
	}
	if want != state.ChartVersion {
		return opts, fmt.Errorf("apply keeps the installed release (%s); `box update` moves it to %s",
			state.ChartVersion, want)
	}
	// The release's own spelling, not the operator's: the chart source is
	// asked for a tag, and the tag is whichever spelling Helm recorded.
	opts.ChartVersion = state.ChartVersion
	return opts, nil
}
