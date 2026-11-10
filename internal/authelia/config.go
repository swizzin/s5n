package authelia

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"

	autheliaassets "github.com/swizzin/s5n/install/platform/authelia"
)

// Instance is one workload the chart serves on this host: one URL prefix on
// the seedbox's host name. It mirrors `s5n.instances` in the chart, because
// the prefix access_control has to talk about is the route's own
// (`/<name>`), and a rule list box typed by hand would drift from the routes
// the release actually creates.
type Instance struct {
	// Name is the workload name -- sonarr, sonarr-4k, plex -- and therefore
	// the URL prefix.
	Name string
	// App is the values key it came from.
	App string
	// Arr is whether it is one of the Arrs (or Prowlarr): an app that
	// authenticates its own API with a key and declares its own environment
	// prefix in the chart. Their `/api` and `/ping` carry their own
	// credentials; their UI does not.
	Arr bool
}

// InstancesFromValues is the chart's `s5n.instances` list, rebuilt from the
// values document box already has. The chart derives an instance name from
// `apps.<app>.instances[].suffix` and treats a values entry without
// `instances` as a single workload named after the app; an enabled instance of
// an Arr with suffix "4k" is `sonarr-4k`, which is also its URL prefix.
//
// It reads the merged values (chart defaults, then the user's file, then the
// grant), which is the same document Helm renders, so a 4K instance that
// exists has its own rules and one that does not is never mentioned.
func InstancesFromValues(values map[string]any) []Instance {
	apps, _ := values["apps"].(map[string]any)
	out := make([]Instance, 0, len(apps))
	names := make([]string, 0, len(apps))
	for name := range apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, app := range names {
		cfg, ok := apps[app].(map[string]any)
		if !ok {
			continue
		}
		if on, _ := cfg["enabled"].(bool); !on {
			continue
		}
		arr := hasEnvPrefix(cfg)
		instances, _ := cfg["instances"].([]any)
		if len(instances) == 0 {
			out = append(out, Instance{Name: app, App: app, Arr: arr})
			continue
		}
		for _, raw := range instances {
			inst, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if on, _ := inst["enabled"].(bool); !on {
				continue
			}
			suffix, _ := inst["suffix"].(string)
			name := app
			if suffix != "" {
				name = app + "-" + suffix
			}
			out = append(out, Instance{Name: name, App: app, Arr: arr})
		}
	}
	return out
}

// hasEnvPrefix is the chart's own test for "this app is an Arr": every Arr and
// Prowlarr declares the environment prefix its settings are read from, and
// nothing else does.
func hasEnvPrefix(cfg map[string]any) bool {
	_, ok := cfg["envPrefix"]
	return ok
}

// selfAuthenticating are the apps whose whole URL prefix is exempt from the
// login: Plex serves its own clients and its own token, and Seerr and
// Tautulli are reached by the same media clients and APIs. Their names are
// the chart's app keys, not instances -- the prefix still comes from the
// instance list.
var selfAuthenticating = map[string]bool{
	"plex":     true,
	"seerr":    true,
	"tautulli": true,
}

// Rule is one access_control rule, in the order Authelia evaluates them.
type Rule struct {
	// Policy is bypass, deny or one_factor.
	Policy string
	// Resources are path regexes, matched against the request path with its
	// query string.
	Resources []string
	// Subject is the one user the rule admits, without the `user:` prefix.
	Subject string
}

// Rules is the access-control list for one host: the portal first, then the
// paths that carry their own credentials, then the one login for everything
// else.
//
// The order is the whole design. Authelia takes the first rule that matches, so
// the portal comes first (the login page has to be reachable by someone who is
// not logged in), and each app's API and ping before the Arrs' UI -- a reverse
// proxy or an API client that presents a key must never be asked for a form.
//
// There is deliberately no bypass for the ACME challenge. It never reaches
// Authelia: cert-manager's HTTP-01 request arrives on the host's own port-80
// listener, which carries no login at all, and Authelia would refuse an http://
// target before it read any rule ("has an insecure scheme 'http'", 4.39). A
// bypass here would be dead config that reads as if the challenge could pass
// through the login.
func Rules(host, login string, instances []Instance) []Rule {
	rules := []Rule{
		// The portal itself. Without this the redirect to /auth/ would be
		// redirected to /auth/, forever.
		{Policy: "bypass", Resources: []string{`^/auth([/?].*)?$`}},
	}
	for _, inst := range instances {
		prefix := regexpQuote("/" + inst.Name)
		switch {
		case inst.Arr:
			// The API key is the credential: Sonarr, Radarr and Prowlarr all
			// answer /api and /ping without a session, and every integration
			// box wires up uses them.
			rules = append(rules, Rule{Policy: "bypass", Resources: []string{
				"^" + prefix + `/api([/?].*)?$`,
				"^" + prefix + `/ping$`,
			}})
		case selfAuthenticating[inst.App]:
			rules = append(rules, Rule{Policy: "bypass", Resources: []string{"^" + prefix + `([/?].*)?$`}})
		}
	}
	return append(rules, Rule{Policy: "one_factor", Subject: login})
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

// Configuration is what the configuration template needs: the host, the one
// login, the encryption key from the Secret, and the rules derived from the
// chart's instances.
type Configuration struct {
	Host          string
	Login         string
	EncryptionKey string
	Argon2        Argon2
	Rules         []Rule
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
	// Authelia refuses an encryption key shorter than 20 characters, and the
	// key is the one thing in this file that is a secret: a short one here
	// would be a login that never starts.
	if len(c.EncryptionKey) < 20 {
		return nil, fmt.Errorf("%w: the storage encryption key is %d characters, and Authelia requires at least 20", ErrConfiguration, len(c.EncryptionKey))
	}
	if c.Argon2 == (Argon2{}) {
		c.Argon2 = DefaultArgon2
	}
	var b strings.Builder
	if err := configurationTemplate.Execute(&b, c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfiguration, err)
	}
	return []byte(b.String()), nil
}
