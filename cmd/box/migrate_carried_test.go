package main

// migrate_carried_test.go is cutover's carried answers over the command: a source
// whose earlier read answered an address is not asked about it again, and the
// answer reads as one row on the screen.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
)

func TestCutoverCarriesTheEarlierReadsAnswers(t *testing.T) {
	manifests, root := unsettledSource(t)
	// PeerBanHelper's downloader points at a public name, which no source object
	// settles: one address, and one the plan proposes an app for.
	writePbhFixture(t, root)
	manifests = addPbhWorkload(t, manifests)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(dir, "final.tar.gz")
	o.Replace = true
	// The earlier read, taken a minute ago: the same source, with the one address
	// this read cannot settle answered by the operator.
	prior := readPlan(t, o)
	for i := range prior.Hosts {
		if prior.Hosts[i].Default && prior.Hosts[i].App != "" {
			prior.Hosts[i].Default = false
			prior.Hosts[i].Why = "you chose qbittorrent"
		}
	}
	writePrior(t, dir, time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC), prior)
	// A run with a prompter that answers nothing: a question asked here is a
	// question the earlier read already answered.
	asker := &fakeSelect{}
	o.Prompt = asker
	var out bytes.Buffer
	err := cutover(context.Background(), o, &say.Plain{W: &out}, proofOf())
	if !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v\n%s", err, out.String())
	}
	if len(asker.asked) != 0 {
		t.Errorf("cutover asked %d questions after a read had answered them: %v", len(asker.asked), asker.asked)
	}
	printed := out.String()
	if !strings.Contains(printed, "\nconnections\n") {
		t.Fatalf("the run does not head the answers it carried:\n%s", printed)
	}
	if !strings.Contains(printed, "✓ pbh            example.com:443 → qbittorrent") {
		t.Errorf("the carried answer is not one row:\n%s", printed)
	}
	// And the question is not asked again: the address is not one the run takes
	// as its own decision.
	if strings.Contains(printed, "1 default taken · the report names each one\nbox migrate cutover") {
		t.Errorf("the run says it took a default before it says what it read:\n%s", printed)
	}
	// The answer is in the plan the snapshot carries, and it is not a default:
	// the next run meets it as an answer, not as something box decided alone.
	opened, err := bundle.Open(o.Out, filepath.Join(t.TempDir(), "open"), nil, nil)
	if err != nil {
		t.Fatalf("the final snapshot does not open: %v", err)
	}
	p, err := plan.Parse(opened.Plan)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	var found *plan.HostRule
	for i := range p.Hosts {
		if p.Hosts[i].Host == "example.com" {
			found = &p.Hosts[i]
		}
	}
	if found == nil || found.Default || found.Leave || found.App != "qbittorrent" {
		t.Fatalf("the plan's rule for the carried address = %+v, want it answered with qbittorrent", found)
	}
}
