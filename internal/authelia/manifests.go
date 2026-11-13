package authelia

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	autheliaassets "github.com/swizzin/s5n/install/platform/authelia"
)

// The names box gives the login's objects. They are constants because the
// SecurityPolicy's backendRef, the Deployment's claim and the ReferenceGrant
// all have to name the same objects, and a name derived per host would clone
// the login once per seedbox on a host that has several.
const (
	// SystemNamespace is where the login lives: s5n-system, the platform's own
	// namespace, beside the admission policy that protects it. It is declared
	// here so the package that writes the login and the cluster client that
	// reads its configuration cannot disagree about where it is.
	SystemNamespace = "s5n-system"
	// ServiceName is the Service the SecurityPolicy calls. It is also the
	// Deployment's name.
	ServiceName = "authelia"
	// ConfigMapName holds configuration.yml.
	ConfigMapName = "s5n-authelia"
	// SecretName holds the storage encryption key. Created once, never
	// updated: rotating it orphans every encrypted row in SQLite.
	SecretName = "s5n-authelia"
	// ClaimName is the claim and the volume for the login's state.
	ClaimName = "s5n-authelia"
	// PolicyName is the SecurityPolicy that puts the login in front of the
	// Gateway. One per host, in the Gateway's namespace.
	PolicyName = "s5n-authelia"
	// RouteName is the HTTPRoute that publishes the portal on the host's own
	// name. It is a route, not a policy, so it sits in the login's namespace
	// beside the Service it forwards to.
	RouteName = "s5n-authelia"
	// ReferenceGrantName is the grant for the cross-namespace backendRef.
	ReferenceGrantName = "s5n-authelia"
	// ServicePort is the port Authelia listens on, and the one the
	// SecurityPolicy names.
	ServicePort = 9091
	// EncryptionKeySecretKey is the Secret's key name.
	EncryptionKeySecretKey = "storage-encryption-key"
)

// The login's fixed identity in the cluster.
const (
	// UID and GID are the login's own: fixed rather than the seedbox's
	// puid/pgid, because the login is root's and serves every seedbox on the
	// host. 65532 is the conventional unprivileged id (distroless's nonroot),
	// which keeps the value legible to anyone who has read a Kubernetes
	// manifest before; nothing in the image depends on it, only the claim's
	// ownership does.
	UID = 65532
	GID = 65532
	// HostPath is where the login's state lives on the host: one directory,
	// created by box, owned by UID and mode 0700. It sits under /var/lib
	// rather than /etc because it is state (a SQLite database that changes on
	// every login), not configuration -- box's only other host state is
	// /etc/swizzin/cluster.yaml, which is root's record of a decision.
	HostPath = "/var/lib/s5n/authelia"
	// ServerPath is where the configuration is mounted inside the pod. It is
	// not /config: /config is the claim, and a ConfigMap mounted over it
	// would hide the users database.
	ServerPath = "/etc/authelia"
	// ConfigFile is the configuration's file name in the ConfigMap and under
	// ServerPath.
	ConfigFile = "configuration.yml"
)

// Manifests is what the manifests template needs. Every field is a decision
// box made, so a rendering test can name the one it is checking.
type Manifests struct {
	// Namespace is s5n-system: the login is platform, not a seedbox.
	Namespace string
	// GatewayNamespace and Gateway name the Gateway the SecurityPolicy
	// targets and the portal route attaches to.
	GatewayNamespace string
	Gateway          string
	// ProxyNamespace is where the Gateway's proxy pods run. It is the peer
	// the login's NetworkPolicy admits and nobody else: the proxy is what sets
	// the headers Authelia trusts, so it is the only caller there may be.
	ProxyNamespace string
	// Host is the one name the portal is published on: the hostname of the
	// HTTPRoute that makes /auth reachable. It is the same name the
	// configuration's cookie lives on, because the 302 Authelia sends points
	// at https://<host>/auth/.
	Host string
	// Claim, ConfigMap and Image are the names the objects refer to.
	Claim     string
	ConfigMap string
	// Secret is the login's Secret. Its OIDC file is mounted when this box has
	// a provider; the Deployment never mounts the encryption key.
	Secret string
	Image  string
	UID    int
	GID    int
	// OIDC says this box has an app that signs in through the login, so the
	// provider's file is mounted and passed as a second --config.
	OIDC      bool
	OIDCFile  string
	OIDCMount string
	// ConfigHash is the sha256 of the rendered configuration, written as a
	// pod-template annotation so a configuration change rolls the pod.
	ConfigHash string
}

// ConfigHash is the annotation's value: the digest of the configuration bytes,
// which is all a pod template needs to change when the configuration does.
func ConfigHash(configuration []byte) string {
	sum := sha256.Sum256(configuration)
	return hex.EncodeToString(sum[:])
}

var manifestsTemplate = template.Must(template.New("manifests.yaml").Parse(string(autheliaassets.Manifests)))

// ErrManifests says the manifests cannot be rendered.
var ErrManifests = errors.New("authelia manifests")

// RenderManifests returns the login's objects as one multi-document stream.
func RenderManifests(m Manifests) ([]byte, error) {
	for name, value := range map[string]string{
		"namespace":         m.Namespace,
		"gateway namespace": m.GatewayNamespace,
		"gateway":           m.Gateway,
		"proxy namespace":   m.ProxyNamespace,
		"host":              m.Host,
		"claim":             m.Claim,
		"configmap":         m.ConfigMap,
		"image":             m.Image,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%w: no %s", ErrManifests, name)
		}
	}
	if m.OIDC {
		// The provider's file is a Secret the Deployment mounts: with no name
		// to mount, Authelia starts without the issuer's keys and every token
		// request fails.
		for name, value := range map[string]string{
			"secret":     m.Secret,
			"oidc file":  m.OIDCFile,
			"oidc mount": m.OIDCMount,
		} {
			if strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%w: no %s, and this box has an OIDC client", ErrManifests, name)
			}
		}
	}
	if m.UID <= 0 || m.GID <= 0 {
		return nil, fmt.Errorf("%w: uid/gid %d:%d", ErrManifests, m.UID, m.GID)
	}
	if len(m.ConfigHash) != 64 {
		return nil, fmt.Errorf("%w: config hash %q is not a sha256 digest", ErrManifests, m.ConfigHash)
	}
	var b strings.Builder
	if err := manifestsTemplate.Execute(&b, m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifests, err)
	}
	return []byte(b.String()), nil
}

// ValidateLogin is the shape of the one account box creates: Authelia's file
// backend keys rows by this name, and a name with whitespace or a colon in it
// is a row a hand-written file could not express. Plex, the Arrs and the
// seedbox's own unix account all allow this alphabet.
func ValidateLogin(login string) error {
	if login == "" {
		return errors.New("no login name")
	}
	if len(login) > 64 {
		return fmt.Errorf("login %q is longer than 64 characters", login)
	}
	for _, r := range login {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '@':
		default:
			return fmt.Errorf("login %q: use letters, digits, and - _ . @", login)
		}
	}
	return nil
}

// GeneratedPasswordLength is how long a generated password is. Authelia has no
// minimum of its own; 32 characters from crypto/rand is long enough that the
// only reason to change it is that it was printed once and lost.
const GeneratedPasswordLength = 32

// passwordAlphabet omits nothing and relies on rejection instead: every byte is
// drawn from crypto/rand and mapped onto a URL- and shell-safe alphabet.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword returns a random password. crypto/rand.Read is the only
// source; a failure is an error rather than a fallback, because a predictable
// password here is the login itself.
func GeneratePassword() (string, error) {
	out := make([]byte, 0, GeneratedPasswordLength)
	buf := make([]byte, 64)
	// 58 is not a divisor of 256, so a byte outside the largest multiple of
	// the alphabet's length is rejected rather than folded in: a modulo there
	// would make some characters likelier than others.
	limit := 256 - (256 % len(passwordAlphabet))
	for len(out) < GeneratedPasswordLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(out) == GeneratedPasswordLength {
				break
			}
		}
	}
	return string(out), nil
}

// PortString is the Service port as the SecurityPolicy's YAML spells it.
func PortString() string { return strconv.Itoa(ServicePort) }
