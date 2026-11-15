// Package tautulli rewrites the paths and the Plex connection Tautulli stored
// in its config.ini.
//
// Tautulli keeps everything in one INI file (`config.ini`, plexpy/config.py at
// v2.18.2: FILENAME = "config.ini") and its database holds no absolute path:
// `newsletter_log.filename` is a bare name joined to NEWSLETTER_DIR when it is
// used (plexpy/newsletter_handler.py:242-247), and the export table has no file
// column. So the file is the whole job.
//
// The set of keys is Tautulli's own: it rewrites exactly this list when its data
// directory moves (plexpy/__init__.py:160-170 assigns HTTPS_CERT,
// HTTPS_CERT_CHAIN, HTTPS_KEY, LOG_DIR, BACKUP_DIR, CACHE_DIR, EXPORT_DIR and
// NEWSLETTER_DIR relative to the new DATA_DIR), which is the app itself saying
// these values are absolute paths. GIT_PATH, NEWSLETTER_CUSTOM_DIR and
// PMS_LOGS_FOLDER are paths by declaration (plexpy/config.py:108, :163, :53).
// A key is edited in the section _CONFIG_DEFINITIONS gives it and nowhere else,
// because ConfigObj would read the same name in another section as a different
// setting.
//
// Lines are edited in place: only the value after `=` changes, so comments,
// ordering and every key migrate does not own survive byte for byte.
//
// The source is 2.19.1 and the pin is 2.18.2, so this is the one app where
// migrate reads a newer config than the app will. Tautulli's `_upgrade()` is a
// chain of `if CONFIG_VERSION == N` steps, and 23 is matched by none of
// 2.18.2's, so nothing is rewritten downward; unknown keys are kept because
// Config.write() writes the whole section back (plexpy/config.py:470-495,
// :660-805). The database half of that downgrade belongs to the coordinator,
// not to this rewriter.
package tautulli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

func init() { rewrite.Register("tautulli", rewriter{}) }

// configName is Tautulli's config file, in the directory it is given as
// --config /config (plexpy/config.py: FILENAME).
const configName = "config.ini"

// bindAddr is what a pod needs: every container interface. A single-host
// install commonly binds 127.0.0.1, which inside a pod is nobody but itself.
const bindAddr = "0.0.0.0"

// The Plex connection and the listen settings, with the sections
// _CONFIG_DEFINITIONS puts them in (plexpy/config.py:48-61, :127-136).
const (
	sectionPMS  = "PMS"
	sectionWeb  = "General"
	keyPMSIP    = "PMS_IP"
	keyPMSPort  = "PMS_PORT"
	keyPMSSSL   = "PMS_SSL"
	keyPMSURL   = "PMS_URL"
	keyHTTPHost = "HTTP_HOST"
	keyHTTPPort = "HTTP_PORT"
	keyHTTPRoot = "HTTP_ROOT"
)

// pathKeys are the keys holding an absolute path, each in its own section
// (plexpy/config.py:84, :86, :101, :152, :163, :166, :108, :121-123, :53).
var pathKeys = []key{
	{sectionWeb, "BACKUP_DIR"},
	{sectionWeb, "CACHE_DIR"},
	{sectionWeb, "EXPORT_DIR"},
	{sectionWeb, "LOG_DIR"},
	{sectionWeb, "GIT_PATH"},
	{sectionWeb, "HTTPS_CERT"},
	{sectionWeb, "HTTPS_CERT_CHAIN"},
	{sectionWeb, "HTTPS_KEY"},
	{"Newsletter", "NEWSLETTER_DIR"},
	{"Newsletter", "NEWSLETTER_CUSTOM_DIR"},
	{sectionPMS, "PMS_LOGS_FOLDER"},
}

type key struct{ section, name string }

type rewriter struct{}

// Check reads config.ini and reports what it would change, writing nothing.
func (rewriter) Check(_ context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(_ context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(in, true)
}

func run(in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	path := filepath.Join(in.Dir, configName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, configName+" is not in the staged config; nothing to rewrite in it")
			return rep, nil
		}
		return rep, fmt.Errorf("tautulli: %w", err)
	}
	f, err := parseINI(raw)
	if err != nil {
		return rep, fmt.Errorf("tautulli: %s: %w", configName, err)
	}
	edits, err := planEdits(in, f, &rep)
	rep.Changes = summarise(edits)
	if write && rep.Blocked() {
		return rep, fmt.Errorf("tautulli: the plan does not settle every path and address; nothing was written")
	}
	if err != nil || !write || len(edits) == 0 {
		return rep, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("tautulli: %w", err)
	}
	if err := atomicfile.Write(path, f.rewrite(edits), info.Mode()); err != nil {
		return rep, fmt.Errorf("tautulli: %w", err)
	}
	return rep, nil
}

// edit is one value's new text, on the line it was found on.
type edit struct {
	line  int
	value string
	quote string // the quote the old value used, kept so ConfigObj reads it back the same
	where string // section/key, for the report
	what  string // "path", "host", "port", "tls", "url", "bind", "url base"
}

// summarise turns the edits into counts per key, which is how a plan shows
// them: one line per key, never one per path.
func summarise(edits []edit) []rewrite.Change {
	counts := map[[3]string]int{}
	for _, e := range edits {
		counts[[3]string{configName, e.where, e.what}]++
	}
	out := make([]rewrite.Change, 0, len(counts))
	for k, n := range counts {
		out = append(out, rewrite.Change{File: k[0], Where: k[1], What: k[2], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].What < out[j].What
	})
	return out
}

// planEdits is both passes' decision: every value, its rewrite, and the gaps.
func planEdits(in rewrite.Input, f *iniFile, rep *rewrite.Report) ([]edit, error) {
	end, ok := in.Targets[in.App]
	if !ok {
		return nil, fmt.Errorf("tautulli: the plan has no endpoint for %q", in.App)
	}
	var edits []edit

	// Paths: every absolute value goes through this app's table, and one no
	// rule covers is a gap rather than a guess.
	for _, k := range pathKeys {
		line, ok := f.get(k.section, k.name)
		if !ok || strings.TrimSpace(line.value) == "" {
			continue
		}
		moved, covered := in.Plan.Rewrite(in.App, line.value)
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: configName, Key: k.section + "/" + k.name, Path: line.value,
			})
			continue
		}
		if moved != line.value {
			edits = append(edits, edit{line: line.n, value: moved, quote: line.quote,
				where: k.section + "/" + k.name, what: "path"})
		}
	}

	plexEdits, err := planPlex(in, f, rep)
	if err != nil {
		return nil, err
	}
	edits = append(edits, plexEdits...)

	// Listen settings become the chart's.
	for _, e := range []struct {
		name  string
		value string
		what  string
	}{
		{keyHTTPHost, bindAddr, "bind"},
		{keyHTTPPort, strconv.Itoa(end.Port), "port"},
		{keyHTTPRoot, end.URLBase, "url base"},
	} {
		line, ok := f.get(sectionWeb, e.name)
		if !ok || line.value == e.value {
			continue
		}
		edits = append(edits, edit{line: line.n, value: e.value, quote: line.quote,
			where: sectionWeb + "/" + e.name, what: e.what})
	}
	return edits, nil
}

// planPlex is the Plex connection. PMS_IP holds the host and PMS_PORT the port;
// PMS_URL is the same address as a URL, set when the server was entered by hand
// (plexpy/webserve.py:359 reads `PMS_URL_OVERRIDE or PMS_URL`), so it moves with
// them or a stale URL wins over the right host.
func planPlex(in rewrite.Input, f *iniFile, rep *rewrite.Report) ([]edit, error) {
	ipLine, haveIP := f.get(sectionPMS, keyPMSIP)
	urlLine, haveURL := f.get(sectionPMS, keyPMSURL)
	host := strings.TrimSpace(ipLine.value)
	port := atoi(strings.TrimSpace(f.value(sectionPMS, keyPMSPort)))
	if host == "" && haveURL {
		u, err := url.Parse(strings.TrimSpace(urlLine.value))
		if err != nil {
			return nil, fmt.Errorf("tautulli: %s: %s %q is not a URL: %w", configName, keyPMSURL, urlLine.value, err)
		}
		if u.Hostname() != "" {
			host, port = u.Hostname(), atoi(u.Port())
		}
	}
	if host == "" {
		return nil, nil
	}
	res := in.Plan.Resolve(in.App, host, port)
	switch res.Verdict {
	case plan.Leave:
		// The plan says this address is not the box's; kept as it is.
		return nil, nil
	case plan.Rewrite:
	default:
		rep.Unclassified = append(rep.Unclassified, rewrite.Address{
			App: in.App, File: configName, Where: sectionPMS + "/" + keyPMSIP,
			Host: host, Port: port,
		})
		return nil, nil
	}
	target, ok := in.Targets[res.App]
	if !ok {
		return nil, fmt.Errorf("tautulli: %s: the plan points the Plex server at %q, which has no endpoint on the box",
			configName, res.App)
	}
	var edits []edit
	set := func(name, value, what string) {
		line, ok := f.get(sectionPMS, name)
		if !ok || line.value == value {
			return
		}
		edits = append(edits, edit{line: line.n, value: value, quote: line.quote,
			where: sectionPMS + "/" + name, what: what})
	}
	plexURL := (&url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", target.Host, target.Port)}).String()
	if haveIP {
		set(keyPMSIP, target.Host, "host")
		set(keyPMSPort, strconv.Itoa(target.Port), "port")
		// Tautulli dials Plex over plain HTTP inside the release.
		set(keyPMSSSL, "0", "tls")
		if haveURL && strings.TrimSpace(urlLine.value) != "" {
			set(keyPMSURL, plexURL, "url")
		}
	} else if haveURL {
		set(keyPMSURL, plexURL, "url")
	}
	return edits, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
