package release

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"oras.land/oras-go/v2/errdef"
	orasregistry "oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	orasauth "oras.land/oras-go/v2/registry/remote/auth"

	"github.com/swizzin/s5n/internal/fetch"
	"github.com/swizzin/s5n/internal/pin"
)

// DefaultRegistryTimeout bounds one image resolution: a tag box cannot check is
// one it leaves as it recorded it.
const DefaultRegistryTimeout = 30 * time.Second

// Registry resolves an image tag to the manifest digest it names now, for
// `box pin` and for update's tag pins. Pulls are anonymous: ghcr hands out an
// anonymous token for a public image, and box has no credential for anyone
// else's.
type Registry struct {
	// Timeout is one resolution's budget. Zero means DefaultRegistryTimeout.
	Timeout time.Duration
	// Retry is the download policy: a 5xx or a refused connection is retried, a
	// missing tag is the answer on the first try.
	Retry fetch.Policy
}

// Resolve asks the registry what a tag or digest names: the digest of the manifest
// it points at, which for a multi-arch tag is the index. A reference the registry
// does not have is pin.ErrNotFound.
func (r Registry) Resolve(ctx context.Context, repository, reference string) (string, error) {
	repo, err := remote.NewRepository(repository)
	if err != nil {
		return "", fmt.Errorf("image repository %s: %w", repository, err)
	}
	repo.PlainHTTP = plainRegistry(repository)
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultRegistryTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	repo.Client = &orasauth.Client{Client: &http.Client{
		Transport: fetch.Transport{Policy: r.Retry, Base: http.DefaultTransport},
		Timeout:   timeout,
	}}
	desc, err := repo.Resolve(ctx, reference)
	if err != nil {
		return "", registryError(repository, reference, err)
	}
	return desc.Digest.String(), nil
}

// registryError is box's voice for a registry that gave no digest. ghcr answers
// 403 for a private or missing package, which is the same answer as 404.
func registryError(repository, reference string, err error) error {
	switch {
	case isNotFound(err):
		return fmt.Errorf("%s has no %s: %w", repository, reference, pin.ErrNotFound)
	case statusOf(err) == http.StatusUnauthorized:
		return fmt.Errorf("%s/%s: the registry wants credentials, and box has none for it", repository, reference)
	}
	return fmt.Errorf("cannot resolve %s:%s: %w", repository, reference, err)
}

// isNotFound is a registry's "not there": 403 and 404, or oras's own answer.
func isNotFound(err error) bool {
	switch statusOf(err) {
	case http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return errors.Is(err, errdef.ErrNotFound) || errors.Is(err, pin.ErrNotFound)
}

// plainRegistry allows plain HTTP only for a loopback registry, as the chart pull
// does: the e2e's registry is one, and nothing else may be spoken to in the clear.
func plainRegistry(repository string) bool {
	host := repository
	if ref, err := orasregistry.ParseReference(repository); err == nil {
		host = ref.Registry
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
