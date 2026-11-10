// Package kube is how box's host-side commands build their Kubernetes
// clients: one kubeconfig loader, one place client-go's warnings are silenced,
// one SelfSubjectAccessReview preflight, and the ownership strings box stamps
// on what it owns.
//
// It depends on client-go and cli-runtime and on none of box's packages, so
// `release`, `platform`, `bootstrap` and the next command can all use it
// without depending on each other: `release` and `platform` are siblings, and
// bootstrap must never import the write half.
package kube

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config turns a kubeconfig path into the client config every host-side box
// client is built from.
//
// The path is required. box always has one by the time it reads or writes:
// `box bootstrap` resolved it from the cluster decision, `box apply` from the
// record. An empty path would make client-go fall back to KUBECONFIG,
// ~/.kube/config or an in-cluster config, and writing to a cluster the operator
// did not name is the worst failure these commands have.
//
// The loader is clientcmd's explicit path, so a kubeconfig that includes other
// files, names a relative certificate or uses an exec credential plugin behaves
// exactly as it does under kubectl. There is no second precedence here: the
// flag/KUBECONFIG/record order is decided once, before this is called
// (ResolveKubeconfig).
func Config(path string) (*rest.Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no kubeconfig path: box resolves the cluster before it builds a client")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	return quiet(cfg), nil
}

// ConfigFromBytes is Config for a kubeconfig box already holds in memory: the
// bootstrap probe reads the file it discovered, so it has the bytes and not a
// path it resolved.
func ConfigFromBytes(raw []byte) (*rest.Config, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, err
	}
	return quiet(cfg), nil
}

// ProbeClient is the transport for a kubeconfig box holds in memory: the same
// auth every writer gets (bearer tokens, tokenFile, client certificates, and
// exec credential plugins, which client-go runs lazily inside the transport on
// the first request), bounded by timeout, plus the base URL box prefixes its
// API paths with.
func ProbeClient(raw []byte, timeout time.Duration) (*http.Client, string, error) {
	cfg, err := ConfigFromBytes(raw)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
	client.Timeout = timeout
	return client, strings.TrimRight(cfg.Host, "/"), nil
}

// Flags is the client getter Helm's action.Configuration needs, over the same
// kubeconfig path and namespace.
//
// Helm keeps its own loader rather than taking a *rest.Config: it wants
// discovery, the RESTMapper and the release driver. Wrapping the configs it
// builds is therefore how Helm's clients inherit box's warning policy, instead
// of relying on whichever client happened to be built first.
func Flags(path, namespace string) *genericclioptions.ConfigFlags {
	p, ns := path, namespace
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &p
	flags.Namespace = &ns
	flags.WrapConfigFn = quiet
	return flags
}

// quiet installs box's warning policy on one config and returns it.
//
// client-go prints deprecation notices through klog, which puts a line like
// "Endpoints is deprecated" in the middle of box's own output; box is the whole
// interface (docs/box.md), so that noise is dropped rather than explained away.
// Helm's own warnings go through slog and are silenced by each caller's logger.
//
// It is set on the config, never through rest.SetDefaultWarningHandler: a
// process-wide switch is only correct if the first client to run installs it,
// which is why the storage client printed warnings until Helm happened to be
// constructed first.
func quiet(cfg *rest.Config) *rest.Config {
	cfg.WarningHandler = rest.NoWarnings{}
	return cfg
}
