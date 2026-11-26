package authelia

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// extAuthzEndpoint is the part of the rendered configuration this test is about.
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
// Authelia 4.39.28 defaults to a header strategy first (HeaderAuthorization,
// scheme basic: internal/configuration/schema/server.go) and the cookie second.
// Envoy forwards the request's Authorization header whether or not the
// SecurityPolicy names it, so an app whose UI sends its own token decides whether
// its signed-in user gets in. Measured against the pinned image: a well-formed
// foreign token is swallowed as errTokenIntent and the cookie is tried
// (handler_authz_authn.go:581-592 at 4.39.28), but an empty one -- `Authorization:
// Bearer `, which PeerBanHelper's page sends with no token
// (webui/src/service/utils.ts at v9.5.4) -- makes the strategy loop return
// (handler_authz.go:303-322): a 401 to a browser with a valid session.
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

// The endpoint's name and path are what the SecurityPolicy dials, so the
// strategies cannot be moved onto an endpoint nothing calls.
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
