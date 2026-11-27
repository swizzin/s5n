package authelia

import (
	"errors"
	"fmt"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"

	autheliaassets "github.com/swizzin/s5n/install/platform/authelia"
	"github.com/swizzin/s5n/internal/seedbox"
)

// The three policies a rule can carry, in Authelia's vocabulary: bypass means the
// request carries its own credential (or the app signs the user in), and
// one_factor asks the portal for the session.
const (
	PolicyBypass    = "bypass"
	PolicyDeny      = "deny"
	PolicyOneFactor = "one_factor"
)

// Rule is one access_control rule, in the order Authelia evaluates them.
type Rule struct {
	// Domain is the host the rule applies to: the edge asks Authelia by host,
	// and an app served at a name of its own is a second domain for the box.
	Domain string
	// Policy is bypass, deny or one_factor.
	Policy string
	// Resources are path regexes, matched against the request path with its
	// query string. Empty means the whole domain.
	Resources []string
	// Subject is the one user the rule admits, without the `user:` prefix.
	Subject string
}

// Rules is the access-control list for one seedbox host: the portal first, then
// the paths that carry their own credentials, then the one login for everything
// else. hosted names the hosts themselves -- <instance>.<host> for every instance of
// an app the chart serves at a name of its own -- because that name, not the app, is
// what a second instance is reached at; a name with no rule falls into
// default_policy deny. ownLogin names the apps that sign their users in themselves:
// their whole host or path bypasses the login.
//
// The order is the design: Authelia takes the first matching rule, and a client
// presenting a key must never be asked for a form.
//
// There is deliberately no bypass for the ACME challenge: it arrives on the
// host's port-80 listener, which carries no login, and Authelia refuses an http://
// target before reading any rule ("has an insecure scheme 'http'", 4.39).
func Rules(host, login string, instances []seedbox.Instance, hosted, ownLogin []string) []Rule {
	rules := []Rule{
		// The portal itself. Without this the redirect to /auth/ would be
		// redirected to /auth/, forever.
		{Domain: host, Policy: PolicyBypass, Resources: []string{`^/auth([/?].*)?$`}},
	}
	ownHost := make(map[string]bool, len(hosted))
	for _, name := range hosted {
		ownHost[name] = true
	}
	selfAuth := make(map[string]bool, len(ownLogin))
	for _, app := range ownLogin {
		selfAuth[app] = true
	}
	// loginHosts are the domains the one login covers: the box's own, and any app
	// host the catalog does not mark as signing its users in. A hosted app without
	// a rule would fall into default_policy deny.
	loginHosts := []string{host}
	for _, inst := range instances {
		appHost := inst.Name + "." + host
		if ownHost[appHost] {
			if selfAuth[inst.App] {
				// An app that signs its users in itself does so on whatever name it
				// is served at.
				rules = append(rules, Rule{Domain: appHost, Policy: PolicyBypass})
				continue
			}
			loginHosts = append(loginHosts, appHost)
			continue
		}
		prefix := regexpQuote("/" + inst.Name)
		switch {
		case inst.Arr:
			// The API key is the credential: the Arrs and Prowlarr answer /api and
			// /ping without a session, and every integration box wires uses them.
			rules = append(rules, Rule{Domain: host, Policy: PolicyBypass, Resources: []string{
				"^" + prefix + `/api([/?].*)?$`,
				"^" + prefix + `/ping$`,
			}})
		case selfAuth[inst.App]:
			rules = append(rules, Rule{Domain: host, Policy: PolicyBypass, Resources: []string{"^" + prefix + `([/?].*)?$`}})
		}
	}
	for _, d := range loginHosts {
		rules = append(rules, Rule{Domain: d, Policy: PolicyOneFactor, Subject: login})
	}
	return rules
}

// HasRule reports whether a rendered configuration carries an access_control rule
// for domain.
//
// A host with no rule falls through to default_policy deny -- a 403 nobody can get
// past -- so a host counts as served only when its rule is in this file. A domain
// written as one string rather than a list is read the same way.
func HasRule(configuration, domain string) bool {
	var doc struct {
		AccessControl struct {
			Rules []struct {
				Domain yaml.Node `yaml:"domain"`
			} `yaml:"rules"`
		} `yaml:"access_control"`
	}
	if err := yaml.Unmarshal([]byte(configuration), &doc); err != nil {
		return false
	}
	for _, rule := range doc.AccessControl.Rules {
		for _, d := range domainsOf(rule.Domain) {
			if strings.EqualFold(d, domain) {
				return true
			}
		}
	}
	return false
}

// domainsOf is one rule's domain, whether it was written as a list or as a
// single value.
func domainsOf(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	}
	return nil
}

// regexpQuote escapes the characters a URL prefix may hold that a regex reads
// as syntax. Instance names are DNS labels, so only the dash could ever
// appear, but the escape belongs here rather than in a comment.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Configuration is what the configuration template needs.
type Configuration struct {
	Host          string
	Login         string
	EncryptionKey string
	Argon2        Argon2
	// Theme is Authelia's own setting, from ResolveTheme: light or dark.
	Theme string
	// AssetPath is server.asset_path, where the rendered logo and favicon are
	// mounted so the page carries the mark instead of Authelia's picture.
	AssetPath string
	Rules     []Rule
	// OIDC is the identity provider when this box has an app that signs in
	// through the login. Nil renders no identity_providers block: a discovery
	// request is a 404 rather than an issuer nobody uses.
	OIDC *OIDC
}

// ErrConfiguration says the configuration cannot be rendered: box refuses
// before it writes a ConfigMap Authelia would reject at startup.
var ErrConfiguration = errors.New("authelia configuration")

var configurationTemplate = template.Must(template.New("configuration.yml").Parse(string(autheliaassets.Configuration)))

// RenderConfiguration returns configuration.yml for one host.
func RenderConfiguration(c Configuration) ([]byte, error) {
	if strings.TrimSpace(c.Host) == "" {
		return nil, fmt.Errorf("%w: no host name", ErrConfiguration)
	}
	if strings.TrimSpace(c.Login) == "" {
		return nil, fmt.Errorf("%w: no login name", ErrConfiguration)
	}
	// Authelia refuses an encryption key shorter than 20 characters, and this is
	// the one secret in the file: a short one is a login that never starts.
	if len(c.EncryptionKey) < 20 {
		return nil, fmt.Errorf("%w: the storage encryption key is %d characters, and Authelia requires at least 20", ErrConfiguration, len(c.EncryptionKey))
	}
	// A theme Authelia does not know stops its startup, and a seedbox with no login
	// is every app behind it served by nothing.
	if !validBase(c.Theme) {
		return nil, fmt.Errorf("%w: theme %q is not one of %s or %s", ErrConfiguration, c.Theme, BaseLight, BaseDark)
	}
	if err := checkAssetPath(c.AssetPath); err != nil {
		return nil, err
	}
	if c.Argon2 == (Argon2{}) {
		c.Argon2 = DefaultArgon2
	}
	var b strings.Builder
	if err := configurationTemplate.Execute(&b, c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfiguration, err)
	}
	if c.OIDC != nil && c.OIDC.Enabled() {
		// The provider's block goes last: everything above is the page, the rules
		// and the session, and "who may get a token" is found at the end.
		block, err := RenderOIDC(*c.OIDC)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConfiguration, err)
		}
		b.WriteString("\n")
		b.Write(block)
	}
	return []byte(b.String()), nil
}
