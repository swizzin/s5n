package authelia

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release/releasetest"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testConfiguration() Configuration {
	return Configuration{
		Host:          "box.example.com",
		Login:         "alice",
		EncryptionKey: testKey,
		Rules:         Rules("box.example.com", "alice", InstancesFromValues(fixtureValues())),
	}
}

// TestRenderConfiguration is what Authelia has to be able to start with: the
// portal on /auth of the seedbox's own host, a file backend watching the claim,
// no self-service, the filesystem notifier it insists on, and one session
// cookie covering the host. Every key here was checked against 4.39.28's own
// validator; a key that moved would be a login that never becomes Ready.
func TestRenderConfiguration(t *testing.T) {
	got, err := RenderConfiguration(testConfiguration())
	if err != nil {
		t.Fatalf("RenderConfiguration: %v", err)
	}
	text := string(got)
	for _, want := range []string{
		`address: 'tcp://0.0.0.0:9091/auth'`,
		"disable_healthcheck: true",
		"ext-authz:\n        implementation: 'ExtAuthz'",
		`path: '/config/users_database.yml'`,
		"watch: true",
		"password_reset:\n    disable: true",
		"password_change:\n    disable: true",
		"memory: 65536",
		"default_policy: 'deny'",
		`domain: ['box.example.com']`,
		`authelia_url: 'https://box.example.com/auth/'`,
		`encryption_key: '` + testKey + `'`,
		`path: '/config/db.sqlite3'`,
		"notifier:",
		"filesystem:",
		"filename: '/config/notifications.txt'",
		"subject:\n        - 'user:alice'",
		`- '^/sonarr-4k/api([/?].*)?$'`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("configuration does not contain %q:\n%s", want, text)
		}
	}
	// The rules keep their order: the portal before the login. Rendering sorts
	// nothing, and no rule mentions the ACME challenge any more: it is
	// answered on the host's port-80 listener, which carries no login.
	portal := strings.Index(text, `^/auth([/?].*)?$`)
	login := strings.Index(text, `policy: 'one_factor'`)
	if !(portal >= 0 && portal < login) {
		t.Errorf("rule order is wrong: portal %d, login %d", portal, login)
	}
	if strings.Contains(text, "acme-challenge") {
		t.Error("the rendered configuration still exempts the ACME challenge from the login")
	}
}

// A change to the host changes the configuration, and therefore the pod
// template's hash annotation: a box reached at a new name must not keep
// serving the old cookie domain.
func TestRenderConfigurationFollowsTheHost(t *testing.T) {
	a, err := RenderConfiguration(testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	c := testConfiguration()
	c.Host = "other.example.com"
	c.Rules = Rules(c.Host, c.Login, nil)
	b, err := RenderConfiguration(c)
	if err != nil {
		t.Fatal(err)
	}
	if ConfigHash(a) == ConfigHash(b) {
		t.Fatal("two hosts have one configuration hash: a renamed box would not roll its login")
	}
}

func TestRenderConfigurationRefusesWhatAutheliaWould(t *testing.T) {
	cases := map[string]func(*Configuration){
		"no host":   func(c *Configuration) { c.Host = "" },
		"no login":  func(c *Configuration) { c.Login = "" },
		"short key": func(c *Configuration) { c.EncryptionKey = "short" },
		"empty key": func(c *Configuration) { c.EncryptionKey = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := testConfiguration()
			mutate(&c)
			if _, err := RenderConfiguration(c); err == nil {
				t.Fatal("rendered a configuration Authelia would refuse at startup")
			}
		})
	}
}

// The rendered configuration is YAML Authelia reads, so it must not contain
// Go template leftovers or an unquoted value that would change meaning.
func TestRenderConfigurationHasNoTemplateLeftovers(t *testing.T) {
	got, err := RenderConfiguration(testConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{{", "}}", "<no value>"} {
		if strings.Contains(string(got), bad) {
			t.Fatalf("rendered configuration contains %q:\n%s", bad, got)
		}
	}
}

// The Deployment box writes names the image the pin names, repository and all.
// The pin is the only place that reference is written, so this is where a
// dropped repository shows up -- and containerd reads a bare
// "4.39.28@sha256:..." as docker.io/library/4.39.28, which is a pod that never
// starts. The version is not repeated here: the assertion is on the shape and on
// the repository, so a Renovate bump of the pin cannot fail this test.
func TestRenderedDeploymentCarriesThePinnedImage(t *testing.T) {
	pin := releasetest.Pins(t).Authelia
	manifests, err := RenderManifests(Manifests{
		Namespace:        "s5n-system",
		GatewayNamespace: "s5n-gateway",
		Gateway:          "s5n",
		Host:             "box.example.com",
		Claim:            ClaimName,
		ConfigMap:        ConfigMapName,
		Image:            pin,
		UID:              UID,
		GID:              GID,
		ConfigHash:       ConfigHash([]byte("configuration")),
	})
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
	var image string
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc["kind"] != "Deployment" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		podSpec, _ := template["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		if len(containers) != 1 {
			t.Fatalf("Deployment carries %d containers", len(containers))
		}
		c, _ := containers[0].(map[string]any)
		image, _ = c["image"].(string)
	}
	if image == "" {
		t.Fatal("the manifests carry no Deployment image")
	}
	if image != pin {
		t.Fatalf("Deployment image %q is not the pin %q: something between versions.yaml and the template rewrote it", image, pin)
	}
	// Repository:tag@digest, the form containerd resolves without guessing a
	// registry or a repository.
	qualified := regexp.MustCompile(`^docker\.io/authelia/authelia:[^@]+@sha256:[0-9a-f]{64}$`)
	if !qualified.MatchString(image) {
		t.Fatalf("Deployment image %q is not <registry>/<repository>:<tag>@sha256:<digest>", image)
	}
}

// testManifests is one host's login, as init renders it.
func testManifests() Manifests {
	return Manifests{
		Namespace:        "s5n-system",
		GatewayNamespace: "s5n-gateway",
		Gateway:          "s5n",
		Host:             "box.example.com",
		Claim:            ClaimName,
		ConfigMap:        ConfigMapName,
		Image:            "docker.io/authelia/authelia:test",
		UID:              UID,
		GID:              GID,
		ConfigHash:       ConfigHash([]byte("configuration")),
	}
}

// The portal has to exist at https://<host>/auth/, which is where Authelia's
// 302 sends the browser; without the route the login page is a 404 and the
// whole flow stops there. The route names the host, the Gateway the
// SecurityPolicy targets and the Service the policy calls, so a rename in one
// place fails here instead of in a browser.
func TestRenderedPortalRoute(t *testing.T) {
	manifests, err := RenderManifests(testManifests())
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
	var route map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc["kind"] == "HTTPRoute" {
			route = doc
		}
	}
	if route == nil {
		t.Fatal("the login's manifests carry no HTTPRoute: every 302 to /auth/ would be a 404")
	}
	meta, _ := route["metadata"].(map[string]any)
	if meta["name"] != RouteName || meta["namespace"] != "s5n-system" {
		t.Fatalf("route %v/%v, want %s/s5n-system", meta["namespace"], meta["name"], RouteName)
	}
	spec, _ := route["spec"].(map[string]any)
	hosts, _ := spec["hostnames"].([]any)
	if len(hosts) != 1 || hosts[0] != "box.example.com" {
		t.Fatalf("hostnames = %v, want the host's own name", hosts)
	}
	parents, _ := spec["parentRefs"].([]any)
	if len(parents) != 1 {
		t.Fatalf("parentRefs = %v", parents)
	}
	parent, _ := parents[0].(map[string]any)
	if parent["name"] != "s5n" || parent["namespace"] != "s5n-gateway" {
		t.Fatalf("parentRefs = %v, want the Gateway the policy targets", parents)
	}
	if _, ok := parent["sectionName"]; ok {
		t.Fatal("the portal route pins a listener: it must follow whatever listener serves the host")
	}
	rules, _ := spec["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("rules = %v, want one", rules)
	}
	rule, _ := rules[0].(map[string]any)
	matches, _ := rule["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches = %v", matches)
	}
	match, _ := matches[0].(map[string]any)
	path, _ := match["path"].(map[string]any)
	if path["type"] != "PathPrefix" || path["value"] != "/auth" {
		t.Fatalf("path = %v, want PathPrefix /auth", path)
	}
	backends, _ := rule["backendRefs"].([]any)
	if len(backends) != 1 {
		t.Fatalf("backendRefs = %v", backends)
	}
	backend, _ := backends[0].(map[string]any)
	if backend["name"] != ServiceName || backend["port"] != float64(ServicePort) {
		t.Fatalf("backendRefs = %v, want %s:%d", backends, ServiceName, ServicePort)
	}
}

// A login without a host cannot publish a portal: the route would have no
// hostname, and Authelia's redirect would point at a name box never wrote.
func TestRenderManifestsRefusesNoHost(t *testing.T) {
	m := testManifests()
	m.Host = ""
	if _, err := RenderManifests(m); err == nil {
		t.Fatal("the manifests rendered a portal route with no hostname")
	}
}

func TestValidateLogin(t *testing.T) {
	for _, ok := range []string{"alice", "a.b", "user_1", "a-b", "a@b"} {
		if err := ValidateLogin(ok); err != nil {
			t.Errorf("ValidateLogin(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a:b", "a/b", strings.Repeat("a", 65), "ali\nce"} {
		if err := ValidateLogin(bad); err == nil {
			t.Errorf("ValidateLogin(%q) accepted it", bad)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword: %v", err)
		}
		if len(p) != GeneratedPasswordLength {
			t.Fatalf("password %q is %d characters, want %d", p, len(p), GeneratedPasswordLength)
		}
		if strings.Trim(p, passwordAlphabet) != "" {
			t.Fatalf("password %q has a character outside the alphabet", p)
		}
		if seen[p] {
			t.Fatal("two generated passwords are identical")
		}
		seen[p] = true
	}
}
