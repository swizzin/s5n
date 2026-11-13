package authelia

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/crypto/pbkdf2"

	autheliaassets "github.com/swizzin/s5n/install/platform/authelia"
)

// The identity provider: who Authelia will issue tokens to, on which terms, and
// the two secrets it signs with.
//
// What lives where is the whole design. The configuration box writes into the
// ConfigMap carries the clients and their authorization policies -- a client
// secret's digest is not secret material, it is what a presented secret is
// checked against. The signing material (the access-token HMAC key and the RSA
// key behind every ID token) is never in that ConfigMap: it is one file in the
// login's Secret, mounted, and read as a second configuration file, because
// Authelia has no way to point a `jwks[].key` at a path (see OIDCFile).
const (
	// OIDCFile is the Secret key, and the file name, holding the issuer's
	// signing material. It is a second Authelia configuration file, loaded with
	// a second `--config`: Authelia's own secret mechanism
	// (`AUTHELIA_IDENTITY_PROVIDERS_OIDC_HMAC_SECRET_FILE`) covers
	// `hmac_secret` but cannot cover a JWK -- `IsSecretKey` refuses any key
	// containing `[]`, so `identity_providers.oidc.jwks[].key` has no env
	// spelling -- and splitting the two halves between an env var and a file
	// would be two mechanisms to get wrong for one provider.
	OIDCFile = "oidc.yml"
	// OIDCMountPath is where the Secret's file is mounted. It is not
	// /etc/authelia: that is the ConfigMap, and a Secret mounted over it would
	// hide the configuration.
	OIDCMountPath = "/etc/authelia-oidc"
	// OIDCKeyID is the `kid` of the one signing key. It is a constant because
	// a changed kid invalidates every token a client has cached, and nothing
	// box does needs a second key.
	OIDCKeyID = "s5n"
	// OIDCHMACLength is the HMAC secret's length. Authelia hashes whatever it
	// is given to 32 bytes, so the length buys nothing cryptographically; 64
	// characters is what its own documentation recommends, and a longer secret
	// is the one thing that stops a guessed one.
	OIDCHMACLength = 64
	// OIDCKeyBits is the RSA key size. 2048 is the smallest Authelia (and
	// every client library) accepts for RS256 without a warning; 3072 would
	// make every discovery document slower to verify for no practical gain on
	// a box that signs one ID token per login.
	OIDCKeyBits = 2048
	// OIDCClientSecretLength is a client secret's length in characters, from
	// crypto/rand. Authelia's rate limiting does not extend to the token
	// endpoint, so the secret is the only thing between a hostile pod and a
	// token: 48 characters of the alphabet below is ~286 bits.
	OIDCClientSecretLength = 48
)

// OIDCAlphabet is the character set a generated client secret is drawn from:
// letters and digits only, because the secret travels in an HTTP Basic header
// that RFC 6749 requires the client to URL-encode, and a secret with `+`, `/`
// or `=` in it is the one class of client that gets that wrong (Authelia's own
// FAQ documents the failures). Excludes nothing else; rejection sampling keeps
// it uniform.
const OIDCAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// Client is one application's OIDC registration.
type Client struct {
	// ID is `<namespace>-<app>`: the namespace is the seedbox, so two boxes on
	// one login never collide and the id says whose client it is in a log.
	ID string
	// Name is the app's name, shown on the consent page. It is the instance
	// name (qui, sonarr-4k), not the client id.
	Name string
	// Secret is the digest the token endpoint checks a presented secret
	// against. Never the plaintext: that is in the seedbox's own Secret, where
	// the app reads it and box never does again.
	Secret string
	// RedirectURIs are the exact callback URLs. Authelia matches them as
	// strings, so this is the one URL the app builds from its own base URL.
	RedirectURIs []string
	// Policy is the authorization policy the client points at: one per
	// seedbox, admitting that seedbox's own login and nothing else.
	Policy string
}

// Policy is one authorization policy: who may authorize a client.
type Policy struct {
	// Name is the policy's key in `authorization_policies`.
	Name string
	// Login is the one account the policy admits, without the `user:` prefix.
	Login string
}

// OIDC is the provider's configuration as the ConfigMap renders it.
type OIDC struct {
	Clients  []Client
	Policies []Policy
}

// Enabled reports whether there is a provider to render at all. A box with no
// OIDC app gets no `identity_providers` block, and Authelia registers no
// provider routes -- which is what every box did before this existed, and what
// a box whose only OIDC app is off must keep doing.
func (o OIDC) Enabled() bool { return len(o.Clients) > 0 }

// ClientID is one app's client id in one seedbox.
func ClientID(namespace, app string) string { return namespace + "-" + app }

// OIDCPolicyName is a seedbox's authorization policy: the namespace is the
// seedbox, and one policy per seedbox is what keeps one box's login out of
// another's client. It is the same string as the namespace because that is the
// only per-box name box has that cannot collide. (manifests.go's PolicyName is
// the SecurityPolicy's, which is a different object.)
func OIDCPolicyName(namespace string) string { return namespace }

// OIDCClientSecretName is the Secret in the seedbox's namespace holding one
// app's client secret. It is per app rather than one Secret for the release so
// the app's own pod mounts one key and a second OIDC app later mounts its own.
func OIDCClientSecretName(app string) string { return "s5n-oidc-" + app }

// OIDCClientSecretKey is the key inside that Secret: the plaintext the app
// reads with QUI__OIDC_CLIENT_SECRET_FILE.
const OIDCClientSecretKey = "client-secret"

// OIDCClientDigestKey is the login Secret's key for one client's digest. It is
// stored rather than recomputed because a digest carries a random salt: a
// recomputed one would differ on every render, which would change the login's
// ConfigMap on every apply and roll the login (and drop every session) for
// nothing.
func OIDCClientDigestKey(clientID string) string { return "oidc-client-" + clientID }

// OIDCCallbackPath is the path an OIDC client's callback is at, under the app's
// own base URL. qui builds it from its baseUrl and mounts its routes under
// /api/auth/oidc (internal/api/server.go:470, `r.Route("/oidc",
// authHandler.GetOIDCHandler().Routes)` with `r.Get("/callback", ...)` at
// internal/api/handlers/oidc.go:159), so the redirect URI is
// https://<host>/qui/api/auth/oidc/callback. It is a constant because the
// catalog's `oidc:` flag says an app is a client, not where its callback is,
// and a second client with a different path is a box change that says so.
const OIDCCallbackPath = "/api/auth/oidc/callback"

// RedirectURI is one app's callback URL on this box.
func RedirectURI(host, basePath string) string {
	return "https://" + host + strings.TrimSuffix(basePath, "/") + OIDCCallbackPath
}

// ErrOIDC says the provider's configuration cannot be rendered.
var ErrOIDC = errors.New("oidc provider")

var oidcTemplate = template.Must(template.New("oidc").Parse(string(autheliaassets.OIDC)))

// RenderOIDC returns the `identity_providers` block the ConfigMap carries: the
// clients, their redirect URIs and the authorization policies. It never carries
// signing material; that is RenderOIDCSecrets.
//
// A box with no OIDC app renders nothing at all: Authelia registers the
// provider's routes only when the block is present, and a box that has no
// client should not advertise an issuer.
func RenderOIDC(o OIDC) ([]byte, error) {
	if !o.Enabled() {
		return nil, nil
	}
	for _, c := range o.Clients {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Name) == "" {
			return nil, fmt.Errorf("%w: a client with no id or name", ErrOIDC)
		}
		if len(c.RedirectURIs) == 0 {
			return nil, fmt.Errorf("%w: client %s has no redirect URI, and Authelia refuses a client without one", ErrOIDC, c.ID)
		}
		if !DigestLooksValid(c.Secret) {
			return nil, fmt.Errorf("%w: client %s has no usable secret digest (%q)", ErrOIDC, c.ID, c.Secret)
		}
		if strings.TrimSpace(c.Policy) == "" {
			return nil, fmt.Errorf("%w: client %s has no authorization policy: every client is scoped to one seedbox's login", ErrOIDC, c.ID)
		}
	}
	if o.Enabled() {
		// Sorted so two renders of the same inputs are byte-identical: the
		// configuration's hash is what decides whether the login rolls.
		sort.Slice(o.Clients, func(i, j int) bool { return o.Clients[i].ID < o.Clients[j].ID })
		sort.Slice(o.Policies, func(i, j int) bool { return o.Policies[i].Name < o.Policies[j].Name })
		for _, p := range o.Policies {
			if err := ValidateLogin(p.Login); err != nil {
				return nil, fmt.Errorf("%w: policy %s: %v", ErrOIDC, p.Name, err)
			}
		}
	}
	var b strings.Builder
	if err := oidcTemplate.Execute(&b, o); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOIDC, err)
	}
	return []byte(b.String()), nil
}

// RenderOIDCSecrets is the Secret's file: the HMAC key the access tokens are
// signed with and the one RS256 key ID tokens are signed with. It is a complete
// Authelia configuration document on its own, merged under the ConfigMap's by a
// second --config, so nothing here is ever in the ConfigMap or a value.
func RenderOIDCSecrets(hmac string, keyPEM []byte) ([]byte, error) {
	if len(hmac) < 32 {
		return nil, fmt.Errorf("%w: the HMAC secret is %d characters, and Authelia recommends 64 or more", ErrOIDC, len(hmac))
	}
	if len(keyPEM) == 0 {
		return nil, fmt.Errorf("%w: no signing key", ErrOIDC)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Rendered by box: do not edit. The issuer's signing material, kept in the\n")
	fmt.Fprintf(&b, "## login's Secret and mounted beside its configuration. It is a second\n")
	fmt.Fprintf(&b, "## configuration file because Authelia has no file reference for a JWK.\n")
	fmt.Fprintf(&b, "identity_providers:\n  oidc:\n")
	fmt.Fprintf(&b, "    # Signs access tokens. Authelia hashes it to 32 bytes, so its length is\n")
	fmt.Fprintf(&b, "    # entropy rather than a parameter.\n")
	fmt.Fprintf(&b, "    hmac_secret: '%s'\n", hmac)
	fmt.Fprintf(&b, "    jwks:\n")
	fmt.Fprintf(&b, "      # One key signs every ID token. Changing it invalidates tokens clients\n")
	fmt.Fprintf(&b, "      # have cached, so box generates it once and never rotates it.\n")
	fmt.Fprintf(&b, "      - key_id: '%s'\n", OIDCKeyID)
	fmt.Fprintf(&b, "        algorithm: 'RS256'\n")
	fmt.Fprintf(&b, "        use: 'sig'\n")
	fmt.Fprintf(&b, "        key: |\n")
	for _, line := range strings.Split(strings.TrimRight(string(keyPEM), "\n"), "\n") {
		fmt.Fprintf(&b, "          %s\n", line)
	}
	return []byte(b.String()), nil
}

// GenerateOIDCSecrets makes the provider's signing material: a random HMAC
// secret and a fresh RSA key, rendered as the Secret's file.
func GenerateOIDCSecrets() ([]byte, error) {
	hmac, err := randomString(OIDCHMACLength)
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, OIDCKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate the OIDC signing key: %w", err)
	}
	return RenderOIDCSecrets(hmac, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// randomString draws n characters from alphabet with rejection sampling, so
// every character is equally likely (a modulo over 256 would not be).
func randomString(n int) (string, error) {
	return randomFrom(OIDCAlphabet, n)
}

// randomFrom is randomString for a caller that names its alphabet.
func randomFrom(alphabet string, n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 64)
	limit := 256 - (256 % len(alphabet))
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate a secret: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// GenerateOIDCClientSecret is one app's client secret as the seedbox's Secret
// holds it.
func GenerateOIDCClientSecret() (string, error) {
	return randomFrom(OIDCAlphabet, OIDCClientSecretLength)
}

// The client secret's digest, in the format Authelia 4.39.28 verifies a
// client_secret with: `$pbkdf2-sha512$<iterations>$<salt>$<key>`, each half
// base64 with the crypt alphabet (standard base64 with '+' written as '.') and
// no padding. go-crypt's `Base64RawAdaptedEncoding` is that alphabet
// (github.com/go-crypt/crypt internal/encoding/base64adapted.go:8-13); Authelia's
// own documentation example is
// `$pbkdf2-sha512$310000$c8p78n7pUMln0jzvd4aK4Q$JNRBzw...`
// (docs/content/configuration/identity-providers/openid-connect/clients.md:46).
var (
	cryptBase64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789./").WithPadding(base64.NoPadding)
	// PBKDF2SHA512Iterations is the work factor the digest declares. Authelia
	// reads it from the digest, so a box may raise it later without a config
	// change; 310000 is what its documentation uses.
	PBKDF2SHA512Iterations = 310000
	// pbkdf2SaltLen and pbkdf2KeyLen are the sizes the same examples show: 16
	// bytes of salt, 64 bytes of SHA-512 key.
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 64
)

// HashClientSecret hashes a client secret for Authelia's client list. It is
// called once, when the secret is generated, and the digest is stored: a client
// secret is checked on every token request, unlike a password, so it is a
// digest rather than a plaintext even though both are box's.
func HashClientSecret(secret string) (string, error) {
	if len(secret) < 16 {
		return "", fmt.Errorf("%w: a client secret of %d characters is too short to hash", ErrOIDC, len(secret))
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt a client secret: %w", err)
	}
	return hashClientSecretWithSalt(secret, salt), nil
}

// hashClientSecretWithSalt is ImageHash with the salt given, so a test can pin
// the digest.
func hashClientSecretWithSalt(secret string, salt []byte) string {
	key := pbkdf2.Key([]byte(secret), salt, PBKDF2SHA512Iterations, pbkdf2KeyLen, sha512.New)
	return fmt.Sprintf("$pbkdf2-sha512$%d$%s$%s", PBKDF2SHA512Iterations,
		cryptBase64.EncodeToString(salt), cryptBase64.EncodeToString(key))
}

// VerifyClientSecret answers whether secret is the one digest was made from.
// It is Authelia's own check, in constant time, with the parameters the digest
// declares -- the unit test and the e2e's login are both this function's
// witness that the digest box writes is one Authelia accepts.
func VerifyClientSecret(digest, secret string) bool {
	parts := strings.Split(digest, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha512" {
		return false
	}
	var iterations int
	if _, err := fmt.Sscanf(parts[2], "%d", &iterations); err != nil || iterations < 1 {
		return false
	}
	salt, err := cryptBase64.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := cryptBase64.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2.Key([]byte(secret), salt, iterations, len(want), sha512.New)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// DigestLooksValid is the format check Authelia applies to a client_secret
// before it will start: the algorithm prefix, an iteration count and two
// base64 halves. It is the same expression as the schema's PasswordDigest
// pattern (internal/configuration/schema/types.go:73 at 4.39.28), narrowed to
// the one algorithm box writes.
func DigestLooksValid(digest string) bool {
	parts := strings.Split(digest, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha512" {
		return false
	}
	if strings.Trim(parts[2], "0123456789") != "" || parts[2] == "" {
		return false
	}
	for _, half := range parts[3:] {
		if half == "" {
			return false
		}
		for _, r := range half {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '/', r == '.':
			default:
				return false
			}
		}
	}
	return true
}
