package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/release"
)

// applyRelease is the release one `box apply` installs: the installed one, read
// from the cluster. Moving a release needs floors, snapshots and a health gate,
// which only `box update` does; anything else is a move and --chart-version is
// refused before the chart is loaded. A first apply installs the newest.
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
	// Helm's own spelling, not the operator's.
	opts.ChartVersion = state.ChartVersion
	return opts, nil
}
