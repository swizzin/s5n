package update

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/pin"
	"github.com/swizzin/s5n/internal/snapshot"
)

// fakeResolver answers one tag, and counts the asks so a test can prove the
// registry was or was not consulted.
type fakeResolver struct {
	answers map[string]string
	err     error
	asked   []string
}

func (f *fakeResolver) Resolve(_ context.Context, repository, reference string) (string, error) {
	f.asked = append(f.asked, repository+":"+reference)
	if f.err != nil {
		return "", f.err
	}
	digest, ok := f.answers[repository+":"+reference]
	if !ok {
		return "", pin.ErrNotFound
	}
	return digest, nil
}

// fakePins is pins.json in memory.
type fakePins struct {
	pins []snapshot.Pin
	log  *callLog
}

func (f *fakePins) Load(string) ([]snapshot.Pin, error) {
	if f.log != nil {
		f.log.add("pins read")
	}
	return f.pins, nil
}

func (f *fakePins) Save(_ string, pins []snapshot.Pin) error {
	if f.log != nil {
		f.log.add("pins write")
	}
	f.pins = pins
	return nil
}

// tagPinValues writes a values file that pins sonarr's tag, which the grant has
// already enabled.
func (h *harness) tagPinValues(t *testing.T, tag string) {
	t.Helper()
	body := "wiring:\n  timeoutSeconds: 300\napps:\n  sonarr:\n    image:\n      tag: \"" + tag + "\"\n"
	path := t.TempDir() + "/values.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.values = path
}

func (h *harness) withPins(resolver *fakeResolver, records []snapshot.Pin) *fakePins {
	store := &fakePins{pins: records, log: h.log}
	h.u.Pins = resolver
	h.u.PinStore = store
	return store
}

// TestARebuiltTagIsAMoveTheStepSnapshotsAndTheRunRecordsAfterwards: a tag pin
// follows its tag, so a repointed tag moves the app like a chart bump does.
func TestARebuiltTagIsAMoveTheStepSnapshotsAndTheRunRecordsAfterwards(t *testing.T) {
	h := newHarness(t)
	h.tagPinValues(t, "5.0")
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestC)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestC)
	resolver := &fakeResolver{answers: map[string]string{"registry.example.com/seedbox/sonarr:5.0": digestB}}
	store := h.withPins(resolver, []snapshot.Pin{{
		App: "sonarr", Repository: "registry.example.com/seedbox/sonarr",
		Tag: "5.0", Digest: digestA, Chart: "0.4.0", Resolved: h.now,
	}})

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(res.pinNotes) != 0 {
		t.Fatalf("pin notes = %v, want none: the chart's tag did not move", res.pinNotes)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].Moving) != 1 || res.Steps[0].Moving[0] != "sonarr" {
		t.Fatalf("steps = %+v, want sonarr moving on the rebuilt tag", res.Steps)
	}
	if !strings.Contains(h.out.String(), "sonarr: 5.0 was rebuilt, sha256:aaaaaaaa… -> sha256:bbbbbbbb…") {
		t.Fatalf("the plan does not name the rebuild:\n%s", h.out)
	}
	if len(h.snaps.takes) != 1 {
		t.Fatalf("takes = %+v, want the moving app snapshotted first", h.snaps.takes)
	}
	change := h.snaps.takes[0].images["sonarr"]
	if change.Ran.Digest != digestA || change.MovedTo.Digest != digestB || !change.Snapshot {
		t.Fatalf("snapshot image pair = %+v, want %s -> %s with a copy", change, digestA, digestB)
	}
	if len(h.app.calls) == 0 || h.app.calls[0].PinDigests["sonarr"] != digestB {
		t.Fatalf("apply options = %+v, want the resolved digest rendered", h.app.calls)
	}
	if len(store.pins) != 1 || store.pins[0].Digest != digestB || store.pins[0].Chart != "0.5.0" {
		t.Fatalf("pins = %+v, want sonarr recorded at the new digest under 0.5.0", store.pins)
	}
	if !store.pins[0].Resolved.Equal(h.now) {
		t.Fatalf("resolved = %v, want the run's clock", store.pins[0].Resolved)
	}
}

// TestAFailedStepKeepsTheRecordItStarted: the record is what the next run
// compares against, so a failed move must not write it.
func TestAFailedStepKeepsTheRecordItStarted(t *testing.T) {
	h := newHarness(t)
	h.tagPinValues(t, "5.0")
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestC)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestC)
	resolver := &fakeResolver{answers: map[string]string{"registry.example.com/seedbox/sonarr:5.0": digestB}}
	store := h.withPins(resolver, []snapshot.Pin{{
		App: "sonarr", Repository: "registry.example.com/seedbox/sonarr",
		Tag: "5.0", Digest: digestA, Chart: "0.4.0", Resolved: h.now,
	}})
	h.app.errFor["0.5.0"] = errors.New("helm: the upgrade failed")
	// The failed apply's second look reads health; one restart makes it the gate's.
	h.cluster.restarts = map[string]int32{"sonarr": 3}
	h.cluster.exiting = []Exiting{{App: "sonarr", Restarts: 3}}

	if _, err := h.run(t, false); err == nil {
		t.Fatalf("update passed after a failed apply:\n%s", h.out)
	}
	if len(store.pins) != 1 || store.pins[0].Digest != digestA {
		t.Fatalf("pins = %+v, want the old record kept", store.pins)
	}
}

// TestAnUncheckedPinRendersTheRecordAndSaysSo: registry trouble is not a reason to
// stop a box that already knows which bytes its tag was.
func TestAnUncheckedPinRendersTheRecordAndSaysSo(t *testing.T) {
	h := newHarness(t)
	h.tagPinValues(t, "5.0")
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestC)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestC)
	resolver := &fakeResolver{err: errors.New("dial tcp: connection refused")}
	store := h.withPins(resolver, []snapshot.Pin{{
		App: "sonarr", Repository: "registry.example.com/seedbox/sonarr",
		Tag: "5.0", Digest: digestA, Chart: "0.4.0", Resolved: h.now,
	}})

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	if len(res.Steps) != 1 || len(res.Steps[0].Moving) != 0 {
		t.Fatalf("steps = %+v, want no image move from an unchecked pin", res.Steps)
	}
	if !strings.Contains(h.out.String(), "sonarr: pinned to 5.0, which could not be checked") {
		t.Fatalf("the run does not say the pin was not checked:\n%s", h.out)
	}
	if len(h.app.calls) == 0 || h.app.calls[0].PinDigests["sonarr"] != digestA {
		t.Fatalf("apply options = %+v, want the recorded digest rendered", h.app.calls)
	}
	if len(store.pins) != 1 || !store.pins[0].Resolved.Equal(h.now) {
		t.Fatalf("pins = %+v, want the record kept with its old check time", store.pins)
	}
}

// TestATagWithNoRecordAndNoRegistryRefusesBeforeThePlan: box renders no tag
// without a digest, and guesses at none.
func TestATagWithNoRecordAndNoRegistryRefusesBeforeThePlan(t *testing.T) {
	h := newHarness(t)
	h.tagPinValues(t, "5.0")
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestC)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestC)
	h.withPins(&fakeResolver{err: errors.New("dial tcp: connection refused")}, nil)

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "sonarr") || !strings.Contains(err.Error(), "5.0") {
		t.Fatalf("update = %v, want a refusal naming the app and tag", err)
	}
	if len(h.app.calls) != 0 || h.confirm.asks != 0 {
		t.Fatalf("a refused plan applied %d times and asked %d questions", len(h.app.calls), h.confirm.asks)
	}
}

// TestTheChartMovedPastThePinIsSaidWithoutClaimingTheTagStopped: box cannot know
// when someone else's publisher stops building a version.
func TestTheChartMovedPastThePinIsSaidWithoutClaimingTheTagStopped(t *testing.T) {
	h := newHarness(t)
	h.tagPinValues(t, "5.3-lt2.1")
	h.charts.publishVariants("0.4.0", "0.4.0", "0.4.0", []byte(`sonarr:
  default:
    repository: registry.example.com/seedbox/sonarr
    tag: "5.3.1-lt2.1"
`))
	h.charts.publishVariants("0.5.0", "0.4.0", "0.4.0", []byte(`sonarr:
  default:
    repository: registry.example.com/seedbox/sonarr
    tag: "5.4.0-lt2.1"
`))
	resolver := &fakeResolver{answers: map[string]string{"registry.example.com/seedbox/sonarr:5.3-lt2.1": digestA}}
	h.withPins(resolver, []snapshot.Pin{{
		App: "sonarr", Repository: "registry.example.com/seedbox/sonarr",
		Tag: "5.3-lt2.1", Digest: digestA, Chart: "0.4.0", Resolved: h.now,
	}})

	res, err := h.run(t, false)
	if err != nil {
		t.Fatalf("update: %v\n%s", err, h.out)
	}
	want := "sonarr: the chart is on 5.4.0-lt2.1, and 5.3-lt2.1 gets rebuilds only while its publisher still builds that version"
	if len(res.pinNotes) != 1 || res.pinNotes[0] != want {
		t.Fatalf("pin notes = %v, want %q", res.pinNotes, want)
	}
	if !strings.Contains(h.out.String(), want) {
		t.Fatalf("the note is not on screen as a muted line:\n%s", h.out)
	}
	if len(res.Steps[0].Held) != 1 || res.Steps[0].Held[0] != "sonarr" {
		t.Fatalf("held = %v, want the pin named as what keeps sonarr", res.Steps[0].Held)
	}
}

// TestABoxThatKeepsNoPinFileRefusesATagPin: resolving without anywhere to record
// it would move the pin on every run.
func TestABoxThatKeepsNoPinFileRefusesATagPin(t *testing.T) {
	h := newHarness(t)
	h.tagPinValues(t, "5.0")
	h.charts.publish("0.4.0", "0.4.0", "0.4.0", digestC)
	h.charts.publish("0.5.0", "0.4.0", "0.4.0", digestC)
	h.u.Pins = &fakeResolver{}

	_, err := h.run(t, false)
	if err == nil || !strings.Contains(err.Error(), "keeps no pin file") {
		t.Fatalf("update = %v, want a refusal naming the missing record", err)
	}
}
