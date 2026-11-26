// Package notice is the one message box posts when something happens to a
// seedbox: a sentence in text and content plus structured fields. One attempt,
// and a failed delivery never fails the update.
//
// The address is a box.yaml secret (0600); only hostconf.Redact may print it,
// and every error is cleaned first because *url.Error embeds the whole URL.
package notice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/hostconf"
)

// The events a notice reports; receivers' rules are written against these
// words, so they are stable.
const (
	EventUpdated    = "updated"
	EventRolledBack = "rolled-back"
	EventStopped    = "stopped"
	EventUnhealthy  = "unhealthy"
	EventHealthy    = "healthy"
)

// DefaultTimeout bounds one post: the update is the work, the notice is the
// telling.
const DefaultTimeout = 10 * time.Second

// Options are what every notice about this host says about it.
type Options struct {
	// Box is the grant's domain: whose box this is, at a glance.
	Box string
	// Seedbox is the namespace: which box on this host.
	Seedbox string
}

// App is one app in a notice, with the image it was on and the one it moved to
// where the event is a move.
type App struct {
	App  string `json:"app"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Notice is the body box posts. The optional fields are omitted rather than sent
// empty, so a receiver can tell "no gate" from "the gate was empty".
type Notice struct {
	Event   string `json:"event"`
	Box     string `json:"box,omitempty"`
	Seedbox string `json:"seedbox,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Apps    []App  `json:"apps,omitempty"`
	Gate    string `json:"gate,omitempty"`
	Error   string `json:"error,omitempty"`
	Command string `json:"command,omitempty"`
	// Text and Content are the same sentence: Slack and Mattermost read text,
	// Discord reads content.
	Text    string `json:"text"`
	Content string `json:"content"`
}

// Updated is a step that passed: the releases and the apps it moved.
func Updated(o Options, name, from, to string, apps []App) Notice {
	n := Notice{Event: EventUpdated, Box: o.Box, Seedbox: o.Seedbox, From: from, To: to, Apps: apps}
	line := subject(o) + name + " " + from + " → " + to
	if moves := appMoves(apps); moves != "" {
		line += " (" + moves + ")"
	}
	n.finish(line)
	return n
}

// RolledBack is one app box put back on the image it ran.
func RolledBack(o Options, name, app, from, to string) Notice {
	n := Notice{Event: EventRolledBack, Box: o.Box, Seedbox: o.Seedbox, Apps: []App{{App: app, From: to, To: from}}}
	line := subject(o) + name + ": " + app + " rolled back"
	switch {
	case from != "" && to != "":
		line += " from " + to + " to " + from
	case from != "":
		line += " to " + from
	}
	line += ", because it kept exiting on its new image"
	n.finish(line)
	return n
}

// Stopped is a step that did not pass: the gate, what it said, and the release
// the box is on now.
func Stopped(o Options, name, from, to, gate, reason, on string) Notice {
	n := Notice{Event: EventStopped, Box: o.Box, Seedbox: o.Seedbox, From: from, To: to, Gate: gate, Error: reason}
	line := subject(o) + name + " " + from + " → " + to + " stopped"
	if gate != "" {
		line += " at the " + gate + " gate"
	}
	if reason != "" {
		line += ": " + reason
	}
	if on != "" {
		line += "; the box is on " + on
	} else {
		line += "; the release's state could not be read"
	}
	n.finish(line)
	return n
}

// Health is an app's readiness changing; an unhealthy one names the command
// that answers it.
func Health(o Options, app string, ready bool, reason, command string) Notice {
	event := EventHealthy
	line := subject(o) + app + " is ready again"
	if !ready {
		event = EventUnhealthy
		line = subject(o) + app + " is not ready"
		if reason != "" {
			line += " (" + reason + ")"
		}
		if command != "" {
			line += "; " + command
		}
	}
	n := Notice{Event: event, Box: o.Box, Seedbox: o.Seedbox, Apps: []App{{App: app}}, Command: command}
	n.finish(line)
	return n
}

// finish sets the one sentence every reader shows.
func (n *Notice) finish(text string) {
	n.Text = text
	n.Content = text
}

// subject is whose box the sentence is about, when the notice knows a domain.
func subject(o Options) string {
	if o.Box == "" {
		return ""
	}
	return o.Box + ": "
}

// appMoves is the parenthesised list of what moved; an app whose images box
// could not name is its name alone.
func appMoves(apps []App) string {
	parts := make([]string, 0, len(apps))
	for _, app := range apps {
		switch {
		case app.From != "" && app.To != "":
			parts = append(parts, app.App+" "+app.From+" → "+app.To)
		default:
			parts = append(parts, app.App)
		}
	}
	return strings.Join(parts, ", ")
}

// Reading is one app's readiness as a notice reads it.
type Reading struct {
	Ready  bool
	Reason string
}

// Flips returns the apps whose readiness changed between two readings, sorted.
// A changed reason alone is not a change, and a box with no earlier reading has
// no flips: its first wake should not page about every app it runs.
func Flips(before, now map[string]Reading) []string {
	if len(before) == 0 {
		return nil
	}
	var out []string
	for app, h := range now {
		old, ok := before[app]
		if !ok {
			// An app enabled since the last wake has nothing it changed from.
			continue
		}
		if old.Ready != h.Ready {
			out = append(out, app)
		}
	}
	sort.Strings(out)
	return out
}

// Poster posts one notice. It is a value, so a command builds it once from root's
// config and a test points it at a server of its own.
type Poster struct {
	// URL is where root's config says notices go. Empty posts nothing.
	URL string
	// Client is the HTTP client. Nil is a client with DefaultTimeout.
	Client *http.Client
}

// Post sends one notice as JSON, once. A failure is the caller's to warn about,
// and every error names the address by Redact alone.
func (p Poster) Post(ctx context.Context, n Notice) error {
	if strings.TrimSpace(p.URL) == "" {
		return nil
	}
	body, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("building a notice: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("posting a notice: %s", p.clean(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("posting a notice: %s", p.clean(err))
	}
	defer resp.Body.Close()
	// Drain the answer's body so the connection can be reused; say only the status.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("posting a notice to %s: %s", hostconf.Redact(p.URL), resp.Status)
	}
	return nil
}

// clean takes the address out of an error: net/http wraps every transport
// failure in a *url.Error whose message is the whole URL, query included.
func (p Poster) clean(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	redacted := hostconf.Redact(p.URL)
	return redacted + ": " + strings.ReplaceAll(err.Error(), p.URL, redacted)
}

func (p Poster) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: DefaultTimeout}
}
