package main

import (
	"context"
	"fmt"
	"time"

	"github.com/swizzin/s5n/internal/pin"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// boxPins is release.Options.Pins: the digest each tag pin renders, and the record
// it wrote. Apply never moves a pin that has a record; moving one is update's job.
type boxPins struct {
	out      say.Reporter
	resolver pin.Resolver
	// root and chartVersion are this apply's config root and release.
	root         string
	chartVersion string
	// record is what this run rendered and writes only after the apply succeeded.
	record []snapshot.Pin
	// Now is the clock a record is stamped with.
	Now func() time.Time
}

func newBoxPins(out say.Reporter, resolver pin.Resolver) *boxPins {
	return &boxPins{out: out, resolver: resolver, Now: time.Now}
}

// values reads the pins after apply loaded the files, asks the registry only for a
// tag with no record, and returns the overlay that renders repo:tag@digest.
func (b *boxPins) values(ctx context.Context, sb *seedbox.Seedbox, chart release.Chart, values map[string]any) (map[string]any, error) {
	if !pin.AnyTagPin(values, release.EnabledApps(values)) {
		return nil, nil
	}
	root, err := update.ConfigRoot(sb)
	if err != nil {
		return nil, err
	}
	records, err := snapshot.LoadPins(root)
	if err != nil {
		return nil, err
	}
	tagged, err := pin.TaggedPins(chart.Variants, values, release.EnabledApps(values), records)
	if err != nil {
		return nil, err
	}
	answers, err := pin.Recorded(ctx, b.resolver, tagged)
	if err != nil {
		return nil, err
	}
	for _, warning := range pin.Warnings(answers) {
		b.out.Warn(warning)
	}
	b.root, b.chartVersion = root, chart.Version
	b.record = pin.Records(answers, chart.Version, b.Now())
	return pin.Overlay(answers), nil
}

// applied writes the digests this run resolved, only after a successful apply: a
// record written before one would claim a check the run never completed.
func (b *boxPins) applied() {
	if len(b.record) == 0 {
		return
	}
	records, err := snapshot.LoadPins(b.root)
	if err != nil {
		b.out.Warn(fmt.Sprintf("keeping the old pin record: %v", err))
		return
	}
	for _, record := range b.record {
		records = snapshot.WithPin(records, record)
	}
	if err := snapshot.SavePins(b.root, records); err != nil {
		b.out.Warn(fmt.Sprintf("keeping the old pin record: %v", err))
		return
	}
	b.record = nil
}
