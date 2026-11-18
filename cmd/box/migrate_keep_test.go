package main

// migrate_keep_test.go drives the keep path's pre-flight: what stops a cutover
// from installing onto a cluster it did not install, and where in the run that is
// asked.

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/discover"
)

func TestCutoverStopsOnABlockerAfterTheFinalSnapshot(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(dir, "final.tar.gz")
	// The keep path, and the source has never been read before: the pre-flight is
	// what this test is about.
	o.NoPriorDiscover = true
	blocker := discover.Blocker{
		What:  "ports",
		Where: "kube-system/traefik",
		Why:   "this LoadBalancer Service answers on 80, and box's Gateway needs the node's 80 and 443",
		Check: "kubectl -n kube-system get service traefik -o wide",
	}
	seams := cutoverSeams{
		proof: proofOf().proof,
		keep: func(context.Context, cutoverOptions, migrationRead) ([]discover.Blocker, error) {
			return []discover.Blocker{blocker}, nil
		},
	}
	reporter, text := recording()

	err := cutover(context.Background(), o, reporter, seams)
	if !errors.Is(err, errCutoverBlocked) {
		t.Fatalf("cutover = %v, want the keep path's stop", err)
	}
	// The snapshot is taken before the stop: it is what --replace-cluster and the
	// apply that follows need, and what the operator has left either way.
	if _, err := os.Stat(o.Out); err != nil {
		t.Errorf("the pre-flight stopped the run without the final snapshot: %v", err)
	}
	for _, want := range []string{
		"kube-system/traefik: this LoadBalancer Service answers on 80",
		"check: kubectl -n kube-system get service traefik -o wide",
		"box does not change a cluster it did not install",
		"--replace-cluster is the other road",
		"box migrate apply " + o.Out,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the stop does not say %q:\n%s", want, text)
		}
	}
	// Nothing was installed onto the cluster: the install path names the snapshot
	// it opens first, and the run stopped before it.
	if strings.Contains(text.String(), "snapshot: ") {
		t.Errorf("the run went on to install over a blocker:\n%s", text)
	}
}

// TestCutoverReplaceClusterNeverAsksTheKeepPreFlight: replacing the cluster is what
// --replace-cluster is for, so that path does not ask a question about a cluster
// the operator is about to throw away.
func TestCutoverReplaceClusterNeverAsksTheKeepPreFlight(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Replace = true
	o.NoPriorDiscover = true
	asked := false
	seams := cutoverSeams{
		proof: proofOf().proof,
		keep: func(context.Context, cutoverOptions, migrationRead) ([]discover.Blocker, error) {
			asked = true
			return nil, nil
		},
	}
	reporter, _ := recording()

	if err := cutover(context.Background(), o, reporter, seams); !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v, want the replace path's stop", err)
	}
	if asked {
		t.Error("the replace path asked the keep path's pre-flight")
	}
}

// TestCutoverKeepPathGoesOnWithoutABlocker: a cluster with nothing on it the
// pre-flight objects to is one box installs onto in the same run, so the run
// reaches the install -- which, on a host with no box, is where the answers a plan
// cannot hold are required.
func TestCutoverKeepPathGoesOnWithoutABlocker(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(dir, "final.tar.gz")
	o.NoPriorDiscover = true
	// The install's own flags, as runMigrateCutover registers them: the answer
	// files point into this test's directory, and none of the four a plan cannot
	// hold is given.
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o.apply = addCutoverApplyFlags(fs, &o.Kubeconfig)
	o.chart = o.apply.chart
	*o.apply.valuesPath = filepath.Join(dir, "values.yaml")
	*o.apply.grantPath = filepath.Join(dir, "grant.yaml")
	*o.apply.install.record = filepath.Join(dir, "cluster.yaml")
	reporter, text := recording()

	err := cutover(context.Background(), o, reporter, proofOf())
	if err == nil {
		t.Fatal("the keep path installed a box with none of the answers a plan cannot hold")
	}
	if errors.Is(err, errCutoverBlocked) || errors.Is(err, errCutoverStopped) || errors.Is(err, errCutoverPrior) {
		t.Fatalf("the run stopped for the wrong reason: %v", err)
	}
	// It got as far as the install, which is where the answers a plan cannot hold
	// are required: that is the proof the pre-flight let it through.
	if !strings.Contains(err.Error(), "--domain") || !strings.Contains(err.Error(), "this host has no box") {
		t.Errorf("the run stopped as %v, want the install's missing answers", err)
	}
	if _, err := os.Stat(o.Out); err != nil {
		t.Errorf("the keep path wrote no final snapshot: %v", err)
	}
	// And it named the snapshot it opened: the install ran, rather than being
	// skipped over.
	if !strings.Contains(text.String(), "snapshot: ") {
		t.Errorf("the run does not name the snapshot it opened:\n%s", text)
	}
}
