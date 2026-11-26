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

// nodePorts lists Services in every namespace, a cluster-wide read only root's
// kubeconfig has. The port is root's and read from the grant afterwards, so a
// user's run never lists anything. The kubeconfig resolves inside the closure.
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

// portRangeHint names --service-node-port-range, which the API server does not
// publish: a narrowed cluster otherwise refuses the port with no setting named.
func portRangeHint(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// pkg/registry/core/service/portallocator's ErrNotInRange wording.
	if !strings.Contains(msg, "provided port is not in the valid range") {
		return err
	}
	if strings.Contains(msg, "service-node-port-range") {
		return err
	}
	return errors.New(msg + "\nbox picks the torrent port from the default NodePort range (30000-32767). This cluster's range is narrower or moved: widen it with the apiserver's --service-node-port-range (k0s: spec.api.extraArgs in /etc/k0s/k0s.yaml), then run `box init` again")
}
