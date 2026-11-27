package main

// netproof_test.go pins when a cluster is proved to enforce NetworkPolicy: once
// per cluster box did not install, never on the k0s box it did, and never for a
// cluster a record already says was proved.

import (
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
)

// fakeRecord is a record store in memory; a nil rec is no record at all.
type fakeRecord struct {
	rec     *bootstrap.Record
	loadErr error
	saveErr error
	saved   []*bootstrap.Record
}

func (f *fakeRecord) Load() (*bootstrap.Record, error) { return f.rec, f.loadErr }

func (f *fakeRecord) Save(rec *bootstrap.Record) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, rec)
	f.rec = rec
	return nil
}

func (f *fakeRecord) RecordPath() string { return "/etc/s5n/cluster.yaml" }

func externalRecord() *bootstrap.Record {
	return &bootstrap.Record{
		Source:       bootstrap.SourceExternal,
		Kubeconfig:   "/home/example/.kube/config",
		Distribution: bootstrap.DistKubeadm,
		Nodes:        1,
		Platform:     bootstrap.DefaultPlatform(),
	}
}

func managedRecord() *bootstrap.Record {
	rec := externalRecord()
	rec.Source = bootstrap.SourceK0sManaged
	return rec
}

// TestTheProofRunsOnAClusterBoxDidNotInstall: an external cluster is proved, and
// the record keeps the answer so the next apply does not prove it again.
func TestTheProofRunsOnAClusterBoxDidNotInstall(t *testing.T) {
	rec := &fakeRecord{rec: externalRecord()}
	ran := 0
	if err := proveOnceNetworkPolicy(rec, func() error { ran++; return nil }); err != nil {
		t.Fatalf("proveOnceNetworkPolicy: %v", err)
	}
	if ran != 1 {
		t.Fatalf("the proof ran %d times, want 1", ran)
	}
	if len(rec.saved) != 1 || !rec.saved[0].NetworkPolicyProved {
		t.Fatalf("saved %+v, want the record with the proof", rec.saved)
	}
	if !rec.rec.NetworkPolicyProved {
		t.Error("the stored record does not carry the proof")
	}

	// The next apply reads the record and proves nothing.
	if err := proveOnceNetworkPolicy(rec, func() error { ran++; return nil }); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if ran != 1 {
		t.Errorf("a recorded proof was proved again (%d runs)", ran)
	}
}

// TestTheProofSkipsTheK0sClusterBoxInstalled: k0s installs kube-router, and the
// record says which cluster this is.
func TestTheProofSkipsTheK0sClusterBoxInstalled(t *testing.T) {
	rec := &fakeRecord{rec: managedRecord()}
	ran := false
	if err := proveOnceNetworkPolicy(rec, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("proveOnceNetworkPolicy: %v", err)
	}
	if ran {
		t.Error("the k0s cluster box installed was proved")
	}
	if len(rec.saved) != 0 {
		t.Errorf("saved %+v, want nothing written", rec.saved)
	}
}

// TestTheProofRunsWithNoRecordAndWritesNone: apply on a cluster bootstrap never
// decided on proves it and records nothing, because apply does not choose a
// cluster.
func TestTheProofRunsWithNoRecordAndWritesNone(t *testing.T) {
	rec := &fakeRecord{}
	ran := false
	if err := proveOnceNetworkPolicy(rec, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("proveOnceNetworkPolicy: %v", err)
	}
	if !ran {
		t.Error("a cluster with no record was not proved")
	}
	if len(rec.saved) != 0 {
		t.Errorf("saved %+v, want no record written", rec.saved)
	}
}

// TestAFailedProofWritesNoRecord: the proof's refusal stands alone; a cluster that
// ignored policy must not be recorded as one that enforced it.
func TestAFailedProofWritesNoRecord(t *testing.T) {
	rec := &fakeRecord{rec: externalRecord()}
	err := proveOnceNetworkPolicy(rec, func() error { return errors.New("does not enforce NetworkPolicy") })
	if err == nil || err.Error() != "does not enforce NetworkPolicy" {
		t.Fatalf("err = %v, want the proof's own", err)
	}
	if len(rec.saved) != 0 {
		t.Errorf("saved %+v after a refused proof", rec.saved)
	}
}

// TestTheProofStopsOnARecordItCannotRead: a record box cannot read stops the run
// rather than proving a cluster it cannot identify.
func TestTheProofStopsOnARecordItCannotRead(t *testing.T) {
	rec := &fakeRecord{loadErr: errors.New("cluster.yaml: unknown key \"bogus\"")}
	ran := false
	err := proveOnceNetworkPolicy(rec, func() error { ran = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v, want the read failure", err)
	}
	if ran {
		t.Error("proved a cluster whose record could not be read")
	}
}

// TestAFailedSaveIsAnError: the answer must be kept or the run says why not, or
// every apply proves the same cluster again in silence.
func TestAFailedSaveIsAnError(t *testing.T) {
	rec := &fakeRecord{rec: externalRecord(), saveErr: errors.New("read-only file system")}
	if err := proveOnceNetworkPolicy(rec, func() error { return nil }); err == nil {
		t.Fatal("proveOnceNetworkPolicy swallowed a failed save")
	}
}
