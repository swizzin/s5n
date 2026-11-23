package authelia

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// extAuthzEndpoint is the part of the rendered configuration this test is
// about: the ext-authz endpoint's own settings, as Authelia reads them.
type extAuthzEndpoint struct {
	Implementation  string `yaml:"implementation"`
	AuthnStrategies []struct {
		Name    string   `yaml:"name"`
		Schemes []string `yaml:"schemes"`
	} `yaml:"authn_strategies"`
}

func renderedExtAuthz(t *testing.T) extAuthzEndpoint {
	t.Helper()
	raw, err := RenderConfiguration(testConfiguration())
	if err != nil {
		t.Fatalf("RenderConfiguration: %v", err)
	}
	var doc struct {
		Server struct {
			Endpoints struct {
				Authz map[string]extAuthzEndpoint `yaml:"authz"`
			} `yaml:"endpoints"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered configuration does not parse: %v\n%s", err, raw)
	}
	endpoint, ok := doc.Server.Endpoints.Authz["ext-authz"]
	if !ok {
		t.Fatalf("the configuration has no ext-authz endpoint:\n%s", raw)
	}
	return endpoint
}

// The ext-authz endpoint checks the session cookie and nothing else.
//
// Authelia's ExtAuthz default is a header strategy first and the cookie
// second, and its strategy loop returns on an error instead of trying the next
// one (handler_authz.go:303-322 at 4.39.28). Envoy forwards the request's own
// Authorization header to the auth server whether or not the SecurityPolicy
// names it, so an app whose UI sends a token of its own -- PeerBanHelper's UI
// sends `Authorization: Bearer <token>` on every call -- made the header
// strategy fail and the whole request a 401, with a valid session right there.
// A box that renders the default is a box that locks a signed-in user out of
// that app, which is why the one strategy is pinned here.
func TestExtAuthzAsksOnlyForTheSessionCookie(t *testing.T) {
	endpoint := renderedExtAuthz(t)
	if endpoint.Implementation != "ExtAuthz" {
		t.Fatalf("implementation = %q, want ExtAuthz", endpoint.Implementation)
	}
	if len(endpoint.AuthnStrategies) != 1 {
		t.Fatalf("ext-authz asks for %d strategies, want the one that cannot fail on a header: %#v",
			len(endpoint.AuthnStrategies), endpoint.AuthnStrategies)
	}
	strategy := endpoint.AuthnStrategies[0]
	if strategy.Name != "CookieSession" {
		t.Fatalf("ext-authz's strategy is %q, want CookieSession", strategy.Name)
	}
	if len(strategy.Schemes) != 0 {
		t.Fatalf("the cookie strategy carries schemes %#v, and only a header strategy may", strategy.Schemes)
	}
}

// The ext-authz endpoint's name and path are what the SecurityPolicy dials, so
// the strategies cannot be moved onto an endpoint nothing calls.
func TestExtAuthzIsTheEndpointTheEdgeCalls(t *testing.T) {
	raw, err := RenderConfiguration(testConfiguration())
	if err != nil {
		t.Fatalf("RenderConfiguration: %v", err)
	}
	var doc struct {
		Server struct {
			Endpoints struct {
				Authz map[string]extAuthzEndpoint `yaml:"authz"`
			} `yaml:"endpoints"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered configuration does not parse: %v", err)
	}
	if len(doc.Server.Endpoints.Authz) != 1 {
		t.Fatalf("the configuration has %d authz endpoints, want exactly the one the SecurityPolicy dials", len(doc.Server.Endpoints.Authz))
	}
	if _, ok := doc.Server.Endpoints.Authz["ext-authz"]; !ok {
		t.Fatalf("the only authz endpoint is not ext-authz: %#v", doc.Server.Endpoints.Authz)
	}
}
