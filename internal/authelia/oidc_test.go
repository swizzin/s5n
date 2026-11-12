package authelia

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// testOIDC is one seedbox's provider: one app, one client, one policy.
func testOIDC() OIDC {
	digest, err := HashClientSecret("a-client-secret-of-48-characters-abcdefghijklm")
	if err != nil {
		panic(err)
	}
	return OIDC{
		Policies: []Policy{{Name: OIDCPolicyName("seedbox"), Login: "alice"}},
		Clients: []Client{{
			ID:           ClientID("seedbox", "qui"),
			Name:         "qui",
			Secret:       digest,
			RedirectURIs: []string{RedirectURI("box.example.com", "/qui")},
			Policy:       OIDCPolicyName("seedbox"),
		}},
	}
}

// TestRenderOIDC is the provider Authelia has to start with, and the two things
// that must not be in it: no signing material (that is the Secret's file), and
// no client that could authorize a different seedbox's login.
func TestRenderOIDC(t *testing.T) {
	got, err := RenderOIDC(testOIDC())
	if err != nil {
		t.Fatalf("RenderOIDC: %v", err)
	}
	text := string(got)
	for _, want := range []string{
		"identity_providers:",
		"  oidc:",
		"    enforce_pkce: 'public_clients_only'",
		"    authorization_policies:",
		"      seedbox:",
		"        default_policy: 'deny'",
		"          - policy: 'one_factor'",
		"              - 'user:alice'",
		"      - client_id: 'seedbox-qui'",
		"        client_name: 'qui'",
		"          - 'https://box.example.com/qui/api/auth/oidc/callback'",
		"        token_endpoint_auth_method: 'client_secret_basic'",
		"        scopes: ['openid', 'profile', 'email']",
		"        consent_mode: 'implicit'",
		"        authorization_policy: 'seedbox'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the provider block does not contain %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"hmac_secret", "BEGIN RSA PRIVATE KEY", "jwks"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the configuration carries %q: signing material belongs in the Secret's file, never the ConfigMap", unwanted)
		}
	}
}

// TestRenderOIDCNoClientsIsNoProvider: a box with no OIDC app renders nothing,
// so Authelia registers no provider routes and its discovery endpoint is a 404
// rather than an issuer with no clients.
func TestRenderOIDCNoClientsIsNoProvider(t *testing.T) {
	got, err := RenderOIDC(OIDC{})
	if err != nil {
		t.Fatalf("RenderOIDC: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty provider rendered %d bytes:\n%s", len(got), got)
	}
	// And the whole configuration carries no block either.
	config, err := RenderConfiguration(Configuration{
		Host:          "box.example.com",
		Login:         "alice",
		EncryptionKey: testKey,
		Theme:         BaseDark,
		AssetPath:     AssetPath,
		Rules:         Rules("box.example.com", "alice", InstancesFromValues(fixtureValues()), nil),
		OIDC:          &OIDC{},
	})
	if err != nil {
		t.Fatalf("RenderConfiguration: %v", err)
	}
	if strings.Contains(string(config), "identity_providers") {
		t.Fatalf("a box with no OIDC app rendered a provider:\n%s", config)
	}
}

// TestRenderConfigurationCarriesTheProvider: the block reaches the file box
// writes, which is what Authelia starts with.
func TestRenderConfigurationCarriesTheProvider(t *testing.T) {
	o := testOIDC()
	c := testConfiguration()
	c.OIDC = &o
	config, err := RenderConfiguration(c)
	if err != nil {
		t.Fatalf("RenderConfiguration: %v", err)
	}
	text := string(config)
	if !strings.Contains(text, "identity_providers:") || !strings.Contains(text, "client_id: 'seedbox-qui'") {
		t.Fatalf("the configuration carries no provider:\n%s", text)
	}
	// The access_control rules are still there: one file, both halves.
	if !strings.Contains(text, `domain: ['box.example.com']`) {
		t.Fatalf("the provider replaced the access_control rules:\n%s", text)
	}
}

// TestEachClientHasItsOwnPolicy is the isolation the design rests on: two
// seedboxes on one login render two policies and two clients, and neither
// client points at the other's policy. A shared policy would let one box's
// account authorize the other's app.
func TestEachClientHasItsOwnPolicy(t *testing.T) {
	digest, err := HashClientSecret("another-client-secret-of-48-chars-abcdefgh")
	if err != nil {
		t.Fatalf("HashClientSecret: %v", err)
	}
	o := OIDC{
		Policies: []Policy{
			{Name: OIDCPolicyName("seedbox"), Login: "alice"},
			{Name: OIDCPolicyName("otherbox"), Login: "bob"},
		},
		Clients: []Client{
			{ID: ClientID("seedbox", "qui"), Name: "qui", Secret: testOIDC().Clients[0].Secret,
				RedirectURIs: []string{RedirectURI("box.example.com", "/qui")}, Policy: OIDCPolicyName("seedbox")},
			{ID: ClientID("otherbox", "qui"), Name: "qui", Secret: digest,
				RedirectURIs: []string{RedirectURI("other.example.com", "/qui")}, Policy: OIDCPolicyName("otherbox")},
		},
	}
	got, err := RenderOIDC(o)
	if err != nil {
		t.Fatalf("RenderOIDC: %v", err)
	}
	text := string(got)
	for _, want := range []string{
		"      seedbox:\n        default_policy: 'deny'\n        rules:\n          - policy: 'one_factor'\n            subject:\n              - 'user:alice'",
		"      otherbox:\n        default_policy: 'deny'\n        rules:\n          - policy: 'one_factor'\n            subject:\n              - 'user:bob'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the policies are not one per seedbox, missing:\n%s\nin:\n%s", want, text)
		}
	}
	if strings.Count(text, "authorization_policy: 'seedbox'") != 1 ||
		strings.Count(text, "authorization_policy: 'otherbox'") != 1 {
		t.Errorf("a client points at the wrong policy:\n%s", text)
	}
	if strings.Contains(text, "user:alice") == false || strings.Count(text, "user:") != 2 {
		t.Errorf("want exactly one admitted account per policy:\n%s", text)
	}
}

// TestRenderOIDCSecrets: the Secret's file is a complete configuration document
// with the two pieces of signing material, and the key it carries is a real RSA
// key of the size the provider advertises.
func TestRenderOIDCSecrets(t *testing.T) {
	file, err := GenerateOIDCSecrets()
	if err != nil {
		t.Fatalf("GenerateOIDCSecrets: %v", err)
	}
	text := string(file)
	for _, want := range []string{
		"identity_providers:",
		"  oidc:",
		"    hmac_secret: '",
		"    jwks:",
		"      - key_id: 's5n'",
		"        algorithm: 'RS256'",
		"        use: 'sig'",
		"        key: |",
		"          -----BEGIN RSA PRIVATE KEY-----",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Secret's file does not contain %q:\n%s", want, text)
		}
	}
	start := strings.Index(text, "-----BEGIN RSA PRIVATE KEY-----")
	end := strings.Index(text, "-----END RSA PRIVATE KEY-----")
	if start < 0 || end < 0 {
		t.Fatal("the file carries no RSA private key block")
	}
	// The block is indented by the YAML scalar; strip it before parsing, which
	// is what Authelia's YAML parser does for us in the real file.
	body := strings.TrimSpace(strings.ReplaceAll(text[start:end+len("-----END RSA PRIVATE KEY-----")], "          ", ""))
	parsed, _ := pem.Decode([]byte(body + "\n"))
	if parsed == nil {
		t.Fatal("the key does not decode as PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(parsed.Bytes)
	if err != nil {
		t.Fatalf("the key is not a PKCS#1 RSA private key: %v", err)
	}
	if bits := key.N.BitLen(); bits < 2048 {
		t.Fatalf("the signing key is %d bits, and RS256 needs at least 2048", bits)
	}
	// The HMAC secret is the documented 64 characters or more.
	line := strings.SplitN(strings.SplitN(text, "hmac_secret: '", 2)[1], "'", 2)[0]
	if len(line) < OIDCHMACLength {
		t.Fatalf("the HMAC secret is %d characters, want at least %d", len(line), OIDCHMACLength)
	}
	if strings.Count(text, "PRIVATE KEY") != 2 {
		t.Fatalf("the file carries %d private-key markers, want one block", strings.Count(text, "PRIVATE KEY"))
	}
}

// TestClientSecretDigestIsAuthelia's: the digest box writes is one Authelia
// verifies, with the algorithm and parameters its own documentation names
// (pbkdf2-sha512, 310000 iterations, the crypt base64 alphabet, no padding).
func TestClientSecretDigestIsAuthelia(t *testing.T) {
	secret, err := GenerateOIDCClientSecret()
	if err != nil {
		t.Fatalf("GenerateOIDCClientSecret: %v", err)
	}
	digest, err := HashClientSecret(secret)
	if err != nil {
		t.Fatalf("HashClientSecret: %v", err)
	}
	if !strings.HasPrefix(digest, "$pbkdf2-sha512$310000$") {
		t.Fatalf("digest = %q, want the pbkdf2-sha512 prefix and 310000 iterations", digest)
	}
	if strings.ContainsAny(digest, "+=") {
		t.Fatalf("digest = %q carries '+', '=' or '/': Authelia's digest alphabet has none of the first two", digest)
	}
	parts := strings.Split(digest, "$")
	if len(parts) != 5 {
		t.Fatalf("digest = %q, want five $-separated fields", digest)
	}
	// 16 bytes of salt, 64 of SHA-512 key, both raw base64: 22 and 86
	// characters, which is the shape of Authelia's documented example.
	if len(parts[3]) != 22 || len(parts[4]) != 86 {
		t.Fatalf("digest halves are %d/%d characters, want 22/86", len(parts[3]), len(parts[4]))
	}
	if !DigestLooksValid(digest) {
		t.Fatalf("digest = %q does not match the schema's own pattern", digest)
	}
	if !VerifyClientSecret(digest, secret) {
		t.Fatal("the digest does not verify against the secret it was made from")
	}
	if VerifyClientSecret(digest, secret+"x") {
		t.Fatal("a different secret verified against the digest")
	}
	// Authelia's documented example also verifies: the format is not
	// box-specific.
	const documented = "$pbkdf2-sha512$310000$c8p78n7pUMln0jzvd4aK4Q$JNRBzwAo0ek5qKn50cFzzvE9RXV88h1wJn5KGiHrD0YKtZaR/nCb2CJPOsKaPK0hjf.9yHxzQGZziziccp6Yng"
	if !VerifyClientSecret(documented, "insecure_secret") {
		t.Fatal("Authelia's own documented digest does not verify: the encoding or parameters differ from 4.39.28's")
	}
}

// TestTwoHashesDiffer: the salt is random, so the same secret hashed twice is
// two different digests -- and box stores the one it wrote rather than
// recomputing it, which is what keeps the login's ConfigMap stable.
func TestTwoHashesDiffer(t *testing.T) {
	a, err := HashClientSecret("the-same-client-secret-twice-over")
	if err != nil {
		t.Fatalf("HashClientSecret: %v", err)
	}
	b, err := HashClientSecret("the-same-client-secret-twice-over")
	if err != nil {
		t.Fatalf("HashClientSecret: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of one secret are identical: the salt is not random")
	}
	if !VerifyClientSecret(a, "the-same-client-secret-twice-over") || !VerifyClientSecret(b, "the-same-client-secret-twice-over") {
		t.Fatal("one of the two digests does not verify")
	}
}

// TestRenderOIDCRefusesWhatAutheliaWould: every refusal here is a login that
// would fail its own startup, and a login that does not start serves nothing.
func TestRenderOIDCRefusesWhatAutheliaWould(t *testing.T) {
	broken := []struct {
		name string
		edit func(*OIDC)
	}{
		{"no redirect URI", func(o *OIDC) { o.Clients[0].RedirectURIs = nil }},
		{"plaintext secret", func(o *OIDC) { o.Clients[0].Secret = "not-a-digest" }},
		{"no policy", func(o *OIDC) { o.Clients[0].Policy = "" }},
		{"no client id", func(o *OIDC) { o.Clients[0].ID = "" }},
		{"a policy that admits nobody", func(o *OIDC) { o.Policies[0].Login = "" }},
	}
	for _, tc := range broken {
		t.Run(tc.name, func(t *testing.T) {
			o := testOIDC()
			tc.edit(&o)
			if _, err := RenderOIDC(o); err == nil {
				t.Fatal("rendered a provider Authelia would refuse")
			}
		})
	}
}

// TestRedirectURI: the callback is the app's own, under its base path. qui
// builds it from its baseUrl and mounts /callback under /api/auth/oidc, so a
// different string here is a redirect_uris mismatch and a failed login.
func TestRedirectURI(t *testing.T) {
	if got, want := RedirectURI("box.example.com", "/qui"), "https://box.example.com/qui/api/auth/oidc/callback"; got != want {
		t.Fatalf("RedirectURI = %q, want %q", got, want)
	}
	if got, want := RedirectURI("box.example.com", "/qui/"), "https://box.example.com/qui/api/auth/oidc/callback"; got != want {
		t.Fatalf("RedirectURI with a trailing slash = %q, want %q", got, want)
	}
}
