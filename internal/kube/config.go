// Package kube is how box's host-side commands build their Kubernetes clients:
// one kubeconfig loader, one place client-go's warnings are silenced, one
// SelfSubjectAccessReview preflight, and the ownership strings box stamps.
//
// It imports client-go and cli-runtime and no box package, so release, platform
// and bootstrap can use it without depending on each other: release and platform
// are siblings, and bootstrap must never import the write half.
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
// client is built from, through clientcmd's explicit path (includes, relative
// certificates and exec plugins as under kubectl). The path is required: empty
// would let client-go fall back to KUBECONFIG, ~/.kube/config or in-cluster, and
// writing to a cluster the operator did not name is the worst failure here. The
// flag/KUBECONFIG/record precedence is decided once, before this (ResolveKubeconfig).
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
// bootstrap probe reads the file it discovered, so it has bytes, not a path.
func ConfigFromBytes(raw []byte) (*rest.Config, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
	if err != nil {
		return nil, err
	}
	return quiet(cfg), nil
}

// ProbeClient is the transport for a kubeconfig box holds in memory: the same
// auth every writer gets (tokens, tokenFile, client certificates, exec plugins
// run lazily on the first request), bounded by timeout, plus the base URL box
// prefixes API paths with.
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
// kubeconfig path and namespace. Helm keeps its own loader (discovery, the
// RESTMapper, the release driver), so wrapping the configs it builds is how its
// clients inherit box's warning policy.
func Flags(path, namespace string) *genericclioptions.ConfigFlags {
	p, ns := path, namespace
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &p
	flags.Namespace = &ns
	flags.WrapConfigFn = quiet
	return flags
}

// WithRequestTimeout bounds every request the clients built from these flags
// make, and is what a caller with no wait of its own must use: a context
// deadline reaches nothing in Helm, whose release storage reads with
// context.Background() (helm.sh/helm/v4/pkg/storage/driver, secrets.Get).
func WithRequestTimeout(flags *genericclioptions.ConfigFlags, timeout time.Duration) *genericclioptions.ConfigFlags {
	inner := flags.WrapConfigFn
	flags.WrapConfigFn = func(cfg *rest.Config) *rest.Config {
		if inner != nil {
			cfg = inner(cfg)
		}
		cfg.Timeout = timeout
		return cfg
	}
	return flags
}

// quiet installs box's warning policy on one config: client-go writes the API
// server's deprecation and admission warnings through klog, a line nobody can
// act on in the run log, and box is the whole interface (docs/box.md). It goes
// on the config, never through rest.SetDefaultWarningHandler: a process-wide
// switch is only correct if the first client to run installs it.
func quiet(cfg *rest.Config) *rest.Config {
	cfg.WarningHandler = rest.NoWarnings{}
	return cfg
}
