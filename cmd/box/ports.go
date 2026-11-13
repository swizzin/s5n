package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/setup"
)

// nodePorts reads every nodePort the cluster's Services already hold, so the
// torrent port can be picked from the part of the range nothing else has.
//
// It is one LIST of Services in every namespace. That is a cluster-wide read:
// it needs `list` on `services` with no namespace, which is what k0s's
// admin.conf -- root's kubeconfig, `system:masters` -- already grants. A user's
// kubeconfig is bound to their own namespace and does not have it, which is the
// design: the port is root's, assigned once at init and read out of the grant
// from then on, so a user's run never lists anything.
//
// The kubeconfig is resolved inside the closure, like apply's other cluster
// reads, so a run that never needs the port never opens a client.
func nodePorts(kubeconfig string) func(context.Context) ([]setup.NodePort, error) {
	return func(ctx context.Context) ([]setup.NodePort, error) {
		kc, err := release.ResolveKubeconfig(kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
		if err != nil {
			return nil, err
		}
		cfg, err := kube.Config(kc)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", kc, err)
		}
		ports, err := kube.NodePorts(ctx, cfg)
		if err != nil {
			return nil, err
		}
		held := make([]setup.NodePort, 0, len(ports))
		for _, p := range ports {
			held = append(held, setup.NodePort{Port: p.Port, Service: p.Service})
		}
		return held, nil
	}
}

// portRangeHint turns the API server's own refusal of the picked torrent port
// into the setting that caused it.
//
// box assumes the default NodePort range because an apiserver does not publish
// its --service-node-port-range through the API. A cluster narrowed to, say,
// 31000-32000 therefore refuses the port box picked, and the raw error
// ("provided port is not in the valid range") names no setting an operator
// could change. This adds the one sentence that does.
func portRangeHint(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// The wording is pkg/registry/core/service/portallocator's ErrNotInRange,
	// which the API server returns inside the Service's validation error.
	if !strings.Contains(msg, "provided port is not in the valid range") {
		return err
	}
	if strings.Contains(msg, "service-node-port-range") {
		return err
	}
	return errors.New(msg + "\nbox picks the torrent port from the default NodePort range (30000-32767). This cluster's range is narrower or moved: widen it with the apiserver's --service-node-port-range (k0s: spec.api.extraArgs in /etc/k0s/k0s.yaml), then run `box init` again")
}
