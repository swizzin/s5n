package apps

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Bazarr is one Bazarr instance's API. Upstream keeps exactly one Sonarr and one
// Radarr (bazarr/app/config.py at v1.6.2: the sonarr.* and radarr.* sections), so a
// 4K library is a second Bazarr pod.
//
// Every call is a partial form POST to /api/system/settings, as the settings page
// sends: only the posted keys are written (bazarr/api/system/settings.py:44-131), so
// the operator's settings survive. It authenticates with X-API-KEY, which the chart
// seeds (internal/bazconf).
type Bazarr struct {
	client  *Client
	urlBase string
	// postTimeout bounds the settings POST. Bazarr saves and then restarts its
	// live-sync connection inside the same request, looping until it connects
	// (bazarr/app/signalr_client.py at v1.6.2). The write happens first, so a lost
	// answer is a saved setting: the run reads it back.
	postTimeout time.Duration
}

// NewBazarr builds a client for one instance. urlBase is where the chart serves it,
// and the app must be configured with it before it starts: its routes register under
// it (bazarr/app/server.py:21-22).
func NewBazarr(baseURL, urlBase, apiKey string) *Bazarr {
	c := NewClient(baseURL, nil)
	c.Header.Set("X-API-KEY", apiKey)
	return &Bazarr{
		client:      c,
		urlBase:     strings.TrimRight(urlBase, "/"),
		postTimeout: 30 * time.Second,
	}
}

// api is one endpoint, under the instance's own prefix.
func (b *Bazarr) api(path string) string { return b.urlBase + "/api" + path }

// SubtitleSource is one Arr Bazarr keeps: the side and the address it reads.
//
// Enabled false means this box runs no such Arr: the side's own switch is turned off
// and nothing else is written, so a stale address never stays live.
type SubtitleSource struct {
	// Kind is the app's own section name: "sonarr" or "radarr".
	Kind string
	// Enabled is the app's general.use_<kind> switch.
	Enabled bool
	// Host is the in-release Service name, and Port its port.
	Host string
	Port int
	// URLBase is the path the Arr is served under (/sonarr), the app's base URL.
	URLBase string
	// APIKey is the Arr's key, read from the release Secret by the caller.
	APIKey string
}

// WaitReady waits for the app. /api/system/ping is deliberately unauthenticated
// (bazarr/api/system/ping.py at v1.6.2), and is the chart's readiness path too.
func (b *Bazarr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, func(ctx context.Context) error {
		var body Item
		if err := b.client.Get(ctx, b.api("/system/ping"), &body); err != nil {
			return err
		}
		if strings.EqualFold(body.Str("status"), "OK") {
			return nil
		}
		return fmt.Errorf("ping answered %v, want status OK", body["status"])
	})
}

// Settings reads the whole document, one key per section, unmasked: Bazarr is not an
// Arr, so the comparison can be exact.
func (b *Bazarr) Settings(ctx context.Context) (Item, error) {
	var s Item
	if err := b.client.Get(ctx, b.api("/system/settings"), &s); err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	return s, nil
}

// EnsureSource makes one side match the release and proves it did.
//
// The owned fields are the app's own form names: general.use_<kind> and, for a side
// this box runs, <kind>.ip, .port, .base_url, .apikey and .ssl. They are the
// release's, because a side left pointing elsewhere never runs. Everything else in
// the app -- languages, providers, scoring -- is the person's.
func (b *Bazarr) EnsureSource(ctx context.Context, spec SubtitleSource) (EnsureResult, error) {
	if spec.Kind != "sonarr" && spec.Kind != "radarr" {
		return EnsureResult{}, fmt.Errorf("bazarr: %q is not a subtitle source this app has (sonarr, radarr)", spec.Kind)
	}
	if spec.Enabled && (spec.Host == "" || spec.Port == 0) {
		return EnsureResult{}, fmt.Errorf("bazarr %s: an enabled side needs a host and a port", spec.Kind)
	}
	live, err := b.Settings(ctx)
	if err != nil {
		return EnsureResult{}, err
	}
	desired, drift := desiredSource(live, spec)
	if drift == nil {
		return EnsureResult{Change: Unchanged}, nil
	}
	form := make(map[string]string, len(desired))
	for _, f := range desired {
		form[f.name] = f.form
	}
	lost, err := b.postSettings(ctx, form)
	if err != nil {
		return EnsureResult{}, err
	}
	// The app answered, or its answer was lost to its sync loop. Either way the
	// next read says what the settings are: a write accepted and not applied stops
	// here.
	after, err := b.Settings(ctx)
	if err != nil {
		return EnsureResult{}, err
	}
	if differing := sourceDrift(after, desired); len(differing) > 0 {
		return EnsureResult{}, fmt.Errorf("bazarr %s: the app accepted the settings but still reports %s = %v",
			spec.Kind, differing[0].Field, differing[0].New)
	}
	note := ""
	if lost {
		note = fmt.Sprintf("saved; %s held the answer while it connected to the Arr", spec.Kind)
	}
	return EnsureResult{Change: Updated, Drift: drift, Note: note}, nil
}

// desiredSource is one side's fields and the ones that differ; nil drift is a second
// run.
func desiredSource(live Item, spec SubtitleSource) (want []field, drift []FieldChange) {
	want = sourceFields(spec)
	for _, f := range want {
		got, ok := nestedValue(live, f.section, f.field)
		if ok && SameValue(got, f.value) {
			continue
		}
		if f.secret {
			// A rotated Arr key is a change like any other, but the receipt never
			// prints one.
			drift = append(drift, FieldChange{Field: f.section + "." + f.field})
			continue
		}
		old := ""
		if ok {
			old = formatValue(got)
		}
		drift = append(drift, FieldChange{Field: f.section + "." + f.field, Old: old, New: formatValue(f.value)})
	}
	return want, drift
}

// sourceDrift lists the fields whose live value is not what was just sent.
func sourceDrift(live Item, want []field) []FieldChange {
	var out []FieldChange
	for _, f := range want {
		got, ok := nestedValue(live, f.section, f.field)
		if !ok || !SameValue(got, f.value) {
			out = append(out, FieldChange{Field: f.section + "." + f.field, New: f.form})
		}
	}
	return out
}

// field is one owned setting in both shapes: the typed value for GET's JSON and the
// form's own spelling.
type field struct {
	section string
	field   string
	name    string // the form key, settings-<section>-<field>
	value   any    // typed, for the comparison with GET's JSON
	form    string // what the form carries
	secret  bool
}

// sourceFields is the field list for one side; a disabled side has one: its switch.
func sourceFields(spec SubtitleSource) []field {
	use := field{
		section: "general", field: "use_" + spec.Kind, value: spec.Enabled,
		form: strconv.FormatBool(spec.Enabled),
	}
	use.name = "settings-general-" + use.field
	if !spec.Enabled {
		return []field{use}
	}
	base := strings.TrimRight(spec.URLBase, "/")
	if base == "" {
		base = "/"
	}
	out := []field{use}
	for _, f := range []struct {
		name   string
		value  any
		form   string
		secret bool
	}{
		{"ip", spec.Host, spec.Host, false},
		{"port", spec.Port, strconv.Itoa(spec.Port), false},
		{"base_url", base, base, false},
		{"apikey", spec.APIKey, spec.APIKey, true},
		{"ssl", false, "false", false},
	} {
		out = append(out, field{
			section: spec.Kind, field: f.name, name: "settings-" + spec.Kind + "-" + f.name,
			value: f.value, form: f.form, secret: f.secret,
		})
	}
	return out
}

// postSettings sends the form and treats a lost answer as what it is: the write
// happens first, so a timeout is proven by reading the settings back.
func (b *Bazarr) postSettings(ctx context.Context, form map[string]string) (lost bool, err error) {
	values := url.Values{}
	for k, v := range form {
		values.Set(k, v)
	}
	postCtx, cancel := context.WithTimeout(ctx, b.postTimeout)
	defer cancel()
	_, err = b.client.PostForm(postCtx, b.api("/system/settings"), values, nil)
	if err == nil {
		return false, nil
	}
	if !IsTimeout(err) {
		return false, fmt.Errorf("save settings: %w", err)
	}
	// The app saved, then held the request open while its live-sync connection came
	// up. The read after this decides, naming the field.
	return true, nil
}

// nestedValue reads settings[section][field].
func nestedValue(settings Item, section, field string) (any, bool) {
	sec, ok := settings[section].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := sec[field]
	return v, ok
}
