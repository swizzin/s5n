package apps

import (
	"context"

	"fmt"

	"net/url"
	"sort"
	"strings"
)

// Jellyfin is the media server. It is the one app here whose account is not in
// a settings file: its users and API keys are rows in data/jellyfin.db, and its
// first-run wizard is an API of its own. So the Job does not seed a file before
// the app starts. The chart's init container completes the wizard inside the
// pod, before the server the Service reaches starts; the Job then keeps one
// automation account and signs in as that account on every later run.
//
// Everything in this file is at v12.4 (jellyfin/jellyfin), the tag
// charts/s5n/variants.yaml pins:
//
//   - Jellyfin.Api/Controllers/StartupController.cs: POST /Startup/User,
//     /Startup/RemoteAccess and /Startup/Complete. The controller carries
//     [Authorize(Policy = Policies.FirstTimeSetupOrElevated)], and that policy's
//     handler succeeds for *anyone* while IsStartupWizardCompleted is false
//     (Jellyfin.Api/Auth/FirstTimeSetupPolicy/FirstTimeSetupHandler.cs). Those
//     three calls are therefore anonymous until the wizard is done, which is
//     why the chart completes it in the pod's init container and never lets a
//     wizard-open server answer the Gateway.
//   - POST /Startup/User refuses once the first user has a password
//     (StartupController.cs, UpdateStartupUser: a non-empty Password answers
//     403), which is what makes a re-run harmless rather than a password reset.
//   - Jellyfin.Api/Controllers/UserController.cs: POST /Users/AuthenticateByName
//     (no [Authorize]), POST /Users/New (elevated), POST /Users/{id}/Policy
//     (elevated) and POST /Users/{id}/Password (elevated). A new user is not an
//     administrator: the policy is a second call.
//   - Jellyfin.Api/Controllers/ApiKeyController.cs: POST /Auth/Keys?app= sets a
//     key, GET /Auth/Keys lists every key *with its token* (AuthenticationInfo
//     in MediaBrowser.Controller/Security/AuthenticationInfo.cs), so a run can
//     reuse the key named "box" instead of minting a second one.
//   - Jellyfin.Api/Controllers/LibraryStructureController.cs:
//     GET /Library/VirtualFolders and POST /Library/VirtualFolders with
//     name, collectionType, paths and refreshLibrary.
//   - Jellyfin.Api/Controllers/ConfigurationController.cs: GET and POST
//     /System/Configuration/{key}; the key for NetworkConfiguration is
//     "network" (MediaBrowser.Common/Net/NetworkConfigurationStore.cs).
//     KnownProxies is the list of peers whose X-Forwarded-For is honoured
//     (Jellyfin.Server/Extensions/ApiServiceCollectionExtensions.cs,
//     ConfigureForwardHeaders and AddProxyAddresses, which accepts a subnet),
//     and with it empty the forwarded headers are ignored completely.
type Jellyfin struct {
	client *Client
	// Token is the session token of the account this client acts as, or an API
	// key. It goes in X-Emby-Token, the header every Jellyfin client sends.
	Token string
}

// Jellyfin's own client header. Every request a Jellyfin client makes carries
// it, and a sign-in without one is refused: AuthenticateUserByName builds its
// AuthenticationRequest from it, and a missing Client is an
// ArgumentNullException on `request.App` -- the app's 400 "Error processing
// request", not a wrong password (Jellyfin.Api/Controllers/UserController.cs
// and Jellyfin.Server.Implementations/Security/AuthorizationContext.cs at
// v12.4). DeviceId is the device's own identity, so it is a constant here: one
// Job is one device.
const jellyfinClientHeader = `MediaBrowser Client="s5n", Device="wiring", DeviceId="s5n-wiring", Version="1.0"`

// NewJellyfin builds a tokenless client. Nothing it asks without a token is a
// write: /System/Info/Public and /Startup/* answer anonymously while the wizard
// is incomplete, and every other call needs Token set.
func NewJellyfin(baseURL string) *Jellyfin {
	c := NewClient(baseURL, nil)
	c.Header.Set("Authorization", jellyfinClientHeader)
	return &Jellyfin{client: c}
}

// PublicInfo is the subset of GET /System/Info/Public this Job reads.
type PublicInfo struct {
	Version                string `json:"Version"`
	StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
}

// Info is the readiness probe and the wizard check in one call.
// /System/Info/Public carries no [Authorize] (SystemController.cs), so it
// answers before the wizard is done; there is no /health route at v12.4.
func (j *Jellyfin) Info(ctx context.Context) (PublicInfo, error) {
	var out PublicInfo
	if err := j.client.Get(ctx, "/System/Info/Public", &out); err != nil {
		return PublicInfo{}, err
	}
	return out, nil
}

// Ping is the readiness probe.
func (j *Jellyfin) Ping(ctx context.Context) error {
	_, err := j.Info(ctx)
	return err
}

// SetFirstUser names and passwords the user Jellyfin creates on its first start.
// It is the wizard's only state-changing call, and it answers 403 once that user
// has a password: alreadyStarted reports that, and it is success for a run that
// only wanted the account to exist.
func (j *Jellyfin) SetFirstUser(ctx context.Context, name, password string) (bool, error) {
	body := map[string]string{"Name": name, "Password": password}
	err := j.client.Post(ctx, "/Startup/User", body, nil)
	if IsForbidden(err) {
		return true, nil
	}
	return false, err
}

// StartupUser is GET /Startup/User: the name of the first user.
//
// It is also the call that *makes* that user exist. Jellyfin starts with an
// empty user table and creates its default account inside
// UserManager.InitializeAsync (Jellyfin.Server.Implementations/Users/UserManager.cs
// at v12.4), which only the wizard's own read reaches: a POST /Startup/User
// before it answers 404, because UpdateStartupUser looks the first user up and
// refuses when there is none. So a client that wants to name the first account
// reads it first, then renames it.
func (j *Jellyfin) StartupUser(ctx context.Context) (string, error) {
	var out struct {
		Name string `json:"Name"`
	}
	if err := j.client.Get(ctx, "/Startup/User", &out); err != nil {
		return "", err
	}
	return out.Name, nil
}

// AllowRemoteAccess is POST /Startup/RemoteAccess. It is worth setting on a box
// reached over the internet: with it off Jellyfin refuses remote logins even
// when the edge let the request through.
func (j *Jellyfin) AllowRemoteAccess(ctx context.Context) error {
	return j.client.Post(ctx, "/Startup/RemoteAccess", map[string]bool{"EnableRemoteAccess": true}, nil)
}

// CompleteWizard is POST /Startup/Complete, which sets IsStartupWizardCompleted
// in system.xml (BaseApplicationConfiguration.cs). It is a no-op after the first
// run, so no "already complete" answer is needed.
func (j *Jellyfin) CompleteWizard(ctx context.Context) error {
	return j.client.Post(ctx, "/Startup/Complete", nil, nil)
}

// AuthenticateByName signs in and returns a session token. Every later call this
// run makes carries it, and the caller decides what to do with it: the Job never
// persists a session token, because a session is the app's, not the box's.
//
// A refused name or password is a LoginRejectedError, because the caller has to
// tell it from an app that is down: at v12.4 AuthenticationManager hands the
// name and password to DefaultAuthenticationProvider.Authenticate, which throws
// AuthenticationException ("Invalid username or password"), and
// Jellyfin.Api/Middleware/ExceptionMiddleware.cs maps that to 401. A 403 is not
// a wrong password -- SessionManager.AuthenticateNewSessionInternal throws it
// for a device the user may not use and for a user at their session limit -- so
// it stays an ordinary error.
func (j *Jellyfin) AuthenticateByName(ctx context.Context, username, password string) (string, error) {
	var out struct {
		AccessToken string `json:"AccessToken"`
	}
	body := map[string]string{"Username": username, "Pw": password}
	if err := j.client.Post(ctx, "/Users/AuthenticateByName", body, &out); err != nil {
		if IsUnauthorized(err) {
			return "", &LoginRejectedError{App: "jellyfin", Username: username}
		}
		return "", err
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return "", fmt.Errorf("jellyfin: sign in as %s returned no token", username)
	}
	return out.AccessToken, nil
}

// User is the part of a Jellyfin user this Job reads or writes.
type User struct {
	ID     string         `json:"Id"`
	Name   string         `json:"Name"`
	Policy map[string]any `json:"Policy"`
}

// Users lists every account. It needs an administrator's token.
func (j *Jellyfin) Users(ctx context.Context) ([]User, error) {
	var out []User
	if err := j.client.Get(ctx, "/Users", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FindUser returns the account with this name, ignoring case: Jellyfin's own
// lookups are case-insensitive and a source box may spell it differently.
func (j *Jellyfin) FindUser(ctx context.Context, name string) (User, bool, error) {
	users, err := j.Users(ctx)
	if err != nil {
		return User{}, false, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Name, name) {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

// CreateUser is POST /Users/New: the account exists afterwards, with the default
// (non-administrator) policy.
func (j *Jellyfin) CreateUser(ctx context.Context, name, password string) (User, error) {
	var out User
	body := map[string]string{"Name": name, "Password": password}
	if err := j.client.Post(ctx, "/Users/New", body, &out); err != nil {
		return User{}, err
	}
	return out, nil
}

// SetAdministrator makes an account an administrator, by writing its full policy
// back with IsAdministrator set. The policy is read first: POST
// /Users/{id}/Policy replaces the whole object (UserController.cs,
// UpdateUserPolicy), so sending a partial one would reset everything else.
func (j *Jellyfin) SetAdministrator(ctx context.Context, id string) (Change, error) {
	var user User
	if err := j.client.Get(ctx, "/Users/"+url.PathEscape(id), &user); err != nil {
		return Unchanged, err
	}
	if user.Policy == nil {
		return Unchanged, fmt.Errorf("jellyfin: user %s has no policy to read", user.Name)
	}
	if admin, _ := user.Policy["IsAdministrator"].(bool); admin {
		return Unchanged, nil
	}
	user.Policy["IsAdministrator"] = true
	if err := j.client.Post(ctx, "/Users/"+url.PathEscape(id)+"/Policy", user.Policy, nil); err != nil {
		return Unchanged, err
	}
	return Updated, nil
}

// SetPassword sets an account's password with an administrator's token. It is
// how a run puts back the release Secret's password after the account's was
// changed inside the app.
func (j *Jellyfin) SetPassword(ctx context.Context, id, password string) error {
	body := map[string]any{"CurrentPw": "", "NewPw": password, "ResetPassword": false}
	return j.client.Post(ctx, "/Users/"+url.PathEscape(id)+"/Password", body, nil)
}

// APIKey is one row of GET /Auth/Keys.
type APIKey struct {
	AppName     string `json:"AppName"`
	AccessToken string `json:"AccessToken"`
}

// APIKeys lists the permanent keys.
func (j *Jellyfin) APIKeys(ctx context.Context) ([]APIKey, error) {
	var out struct {
		Items []APIKey `json:"Items"`
	}
	if err := j.client.Get(ctx, "/Auth/Keys", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// EnsureAPIKey returns the key named app, minting it when there is none. A key
// that exists is reused, never duplicated: two keys of the same name would be
// two credentials to keep in step.
func (j *Jellyfin) EnsureAPIKey(ctx context.Context, app string) (string, Change, error) {
	keys, err := j.APIKeys(ctx)
	if err != nil {
		return "", Unchanged, err
	}
	for _, k := range keys {
		if k.AppName == app && strings.TrimSpace(k.AccessToken) != "" {
			return k.AccessToken, Unchanged, nil
		}
	}
	q := url.Values{"app": {app}}
	if err := j.client.Post(ctx, "/Auth/Keys?"+q.Encode(), nil, nil); err != nil {
		return "", Unchanged, err
	}
	// The POST answers 204 and carries nothing: the token is read back from the
	// list, and the newest row for this name is the one it made.
	keys, err = j.APIKeys(ctx)
	if err != nil {
		return "", Unchanged, err
	}
	var token string
	for _, k := range keys {
		if k.AppName == app && strings.TrimSpace(k.AccessToken) != "" {
			token = k.AccessToken
		}
	}
	if token == "" {
		return "", Unchanged, fmt.Errorf("jellyfin: minted an API key for %q and could not read it back", app)
	}
	return token, Created, nil
}

// Library is one virtual folder, as GET /Library/VirtualFolders reports it.
type Library struct {
	Name           string   `json:"Name"`
	Locations      []string `json:"Locations"`
	CollectionType string   `json:"CollectionType"`
}

// Libraries lists the virtual folders. It needs elevation at v12.4 unless the
// wizard is incomplete, which is why the Job lists them after the wizard.
func (j *Jellyfin) Libraries(ctx context.Context) ([]Library, error) {
	var out []Library
	if err := j.client.Get(ctx, "/Library/VirtualFolders", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureLibrary creates a virtual folder for one path, and adopts an existing
// one that already holds the path. Adoption is by location, not by name: a
// source box may have named its library anything, and renaming it here would be
// the box rewriting the person's own choice.
func (j *Jellyfin) EnsureLibrary(ctx context.Context, name, collectionType, path string) (Change, error) {
	existing, err := j.Libraries(ctx)
	if err != nil {
		return Unchanged, err
	}
	for _, l := range existing {
		for _, loc := range l.Locations {
			if strings.TrimRight(loc, "/") == strings.TrimRight(path, "/") {
				return Unchanged, nil
			}
		}
	}
	q := url.Values{
		fieldName:        {name},
		"collectionType": {collectionType},
		"paths":          {path},
		"refreshLibrary": {"true"},
	}
	if err := j.client.Post(ctx, "/Library/VirtualFolders?"+q.Encode(), nil, nil); err != nil {
		return Unchanged, err
	}
	return Created, nil
}

// NetworkConfiguration is the object GET /System/Configuration/network returns,
// kept generic so the Job writes back every field the app sent: the endpoint
// replaces the whole document (ConfigurationController.cs).
type NetworkConfiguration map[string]any

// Network reads the network configuration. It needs an administrator.
func (j *Jellyfin) Network(ctx context.Context) (NetworkConfiguration, error) {
	out := NetworkConfiguration{}
	if err := j.client.Get(ctx, "/System/Configuration/network", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Proxies is the list of peers whose X-Forwarded-For this server honours, as it
// holds it now (sorted).
//
// The Job reads this rather than writing it: Jellyfin builds ASP.NET's
// ForwardedHeadersOptions from this configuration at *startup*
// (Jellyfin.Server/Extensions/ApiServiceCollectionExtensions.cs,
// ConfigureForwardHeaders, called from AddJellyfinApi), so a value written
// through the API afterwards takes effect only when the process restarts. The
// chart therefore writes it into network.xml before the app reads it
// (internal/jellyconf), and this call is what proves the running server has it:
// with the list empty the app ignores the header entirely and every request
// through the Gateway counts as its own network.
func (j *Jellyfin) Proxies(ctx context.Context) ([]string, error) {
	conf, err := j.Network(ctx)
	if err != nil {
		return nil, err
	}
	return stringList(conf["KnownProxies"]), nil
}

// stringList reads a JSON array of strings, tolerating the absent and null
// shapes an older configuration has.
func stringList(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// SetToken points this client at a session token or an API key.
//
// It goes in the Authorization header's Token parameter, not in an X-Emby-Token
// of its own: at v12.4 a token in that header is the only one the app's
// authentication handler reads, and the legacy header answers 401 on every
// endpoint (checked against a 12.4 server: sign-in succeeds, then /Users with
// X-Emby-Token is 401 and with the Token parameter is 200).
func (j *Jellyfin) SetToken(token string) {
	j.Token = token
	j.client.Header.Set("Authorization", jellyfinClientHeader+`, Token="`+token+`"`)
}

// HasToken reports whether the client will authenticate.
func (j *Jellyfin) HasToken() bool { return strings.TrimSpace(j.Token) != "" }
