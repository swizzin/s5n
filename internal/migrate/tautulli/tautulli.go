// Package tautulli rewrites the paths, the Plex connection and the login Tautulli
// stored in config.ini, which is the whole job: its database holds no absolute
// path (plexpy/config.py at v2.18.2). The path keys are the ones Tautulli itself
// rewrites when DATA_DIR moves (plexpy/__init__.py:160-170) plus the three
// declared paths (config.py:108, :163, :53), each in the section
// _CONFIG_DEFINITIONS gives it, since ConfigObj reads the same name in another
// section as a different setting.
//
// The spelling is lower case: Config._define folds every name (config.py:473),
// Config.write() builds the file from those keys (:528-556), and ConfigObj is
// case-sensitive. The lookup accepts any spelling and edits the line where it
// stands -- a rename would write a setting the app never reads -- and sections are
// not folded. Lines are edited in place, so comments and unknown keys survive.
//
// A 2.19 source runs ahead of the 2.18.2 pin: _upgrade steps match on
// CONFIG_VERSION and 23 matches none, and Config.write() writes unknown keys back
// (config.py:470-495). The database half of that downgrade is not this rewriter's.
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

// configName is config.ini in the directory given as --config (plexpy/config.py:
// FILENAME).
const configName = "config.ini"

// bindAddr is every container interface: a source's 127.0.0.1 is the pod itself.
const bindAddr = "0.0.0.0"

// The Plex connection and listen settings, with the sections _CONFIG_DEFINITIONS
// puts them in (plexpy/config.py:48-61, :127-136). Names are lower case: the
// spelling Tautulli writes and reads; the lookup accepts any, reports the file's.
const (
	sectionPMS      = "PMS"
	sectionWeb      = "General"
	sectionAdvanced = "Advanced"
	keyPMSIP        = "pms_ip"
	keyPMSPort      = "pms_port"
	keyPMSSSL       = "pms_ssl"
	keyPMSURL       = "pms_url"
	keyHTTPHost     = "http_host"
	keyHTTPPort     = "http_port"
	keyHTTPRoot     = "http_root"
)

// The keys that make Tautulli ask for its own password: a non-empty
// http_password is the switch (webstart.py:195-213 at v2.18.2), the two flags make
// the app hash it, and http_basic_auth only picks how it is asked. Behind box the
// login is at the edge, and internal/ttlconf seals these on every start. 2.19 also
// mints a password when the wizard never finished (__init__.py:3121-3145 at
// v2.19.1, stopped by first_run_complete) and refuses proxied requests without one
// (webstart.py:406-441, stopped by allow_public_no_password), so both are set.
var loginOffKeys = []struct {
	section string
	name    string
	value   string
	quote   string
}{
	{sectionWeb, "http_username", "", `"`},
	{sectionWeb, "http_password", "", `"`},
	{sectionWeb, "http_hash_password", "0", ""},
	{sectionWeb, "http_hashed_password", "0", ""},
	{sectionWeb, "http_basic_auth", "0", ""},
	{sectionWeb, "first_run_complete", "1", ""},
	{sectionAdvanced, "allow_public_no_password", "1", ""},
}

// pathKeys are the keys holding an absolute path, each in its own section
// (plexpy/config.py:84, :86, :101, :152, :163, :166, :108, :121-123, :53).
var pathKeys = []key{
	{sectionWeb, "backup_dir"},
	{sectionWeb, "cache_dir"},
	{sectionWeb, "export_dir"},
	{sectionWeb, "log_dir"},
	{sectionWeb, "git_path"},
	{sectionWeb, "https_cert"},
	{sectionWeb, "https_cert_chain"},
	{sectionWeb, "https_key"},
	{"Newsletter", "newsletter_dir"},
	{"Newsletter", "newsletter_custom_dir"},
	{sectionPMS, "pms_logs_folder"},
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

	// Every absolute path goes through this app's table; one no rule covers is a
	// gap.
	for _, k := range pathKeys {
		line, ok := f.get(k.section, k.name)
		if !ok || strings.TrimSpace(line.value) == "" {
			continue
		}
		moved, covered := in.Plan.Rewrite(in.App, line.value)
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: configName, Key: at(line), Path: line.value,
			})
			continue
		}
		if moved != line.value {
			edits = append(edits, edit{line: line.n, value: moved, quote: line.quote,
				where: at(line), what: "path"})
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
			where: at(line), what: e.what})
	}

	// Its own login goes, said out loud: the person signs in at the edge.
	edits = append(edits, loginOff(f)...)
	rep.Notes = append(rep.Notes, "its own login is switched off; you sign in through the box")
	return edits, nil
}

// loginOff is the edits that switch the app's login off. Only keys the file holds
// are edited: an absent one is answered by the app's defaults, and writing it
// would invent a setting.
func loginOff(f *iniFile) []edit {
	var edits []edit
	for _, k := range loginOffKeys {
		line, ok := f.get(k.section, k.name)
		if !ok || line.value == k.value {
			continue
		}
		edits = append(edits, edit{line: line.n, value: k.value, quote: k.quote,
			where: at(line), what: "own login"})
	}
	return edits
}

// at is one line's section/key as the file spells it: a report names a line the
// operator can find.
func at(line iniLine) string { return line.section + "/" + line.key }

// planPlex is the Plex connection: pms_ip and pms_port hold the address, and
// pms_url the same as a URL (plexpy/webserve.py:359 reads `PMS_URL_OVERRIDE or
// PMS_URL`), so it moves with them or a stale URL wins.
func planPlex(in rewrite.Input, f *iniFile, rep *rewrite.Report) ([]edit, error) {
	ipLine, haveIP := f.get(sectionPMS, keyPMSIP)
	urlLine, haveURL := f.get(sectionPMS, keyPMSURL)
	host := strings.TrimSpace(ipLine.value)
	port := atoi(strings.TrimSpace(f.value(sectionPMS, keyPMSPort)))
	// A URL's path settles it on a host of paths; the other two are host and port.
	var stored *url.URL
	if host == "" && haveURL {
		u, err := url.Parse(strings.TrimSpace(urlLine.value))
		if err != nil {
			return nil, fmt.Errorf("tautulli: %s: %s %q is not a URL: %w", configName, keyPMSURL, urlLine.value, err)
		}
		if u.Hostname() != "" {
			stored = u
			host, port = u.Hostname(), rewrite.URLPort(u)
		}
	}
	if host == "" {
		return nil, nil
	}
	res := in.Resolve(host, port)
	if stored != nil {
		res = in.ResolveURL(stored)
	}
	switch res.Verdict {
	case plan.Leave:
		// The plan says this address is not the box's; kept as it is.
		return nil, nil
	case plan.Rewrite:
	default:
		where := sectionPMS + "/" + keyPMSIP
		if haveIP {
			where = at(ipLine)
		} else if haveURL {
			where = at(urlLine)
		}
		rep.Unclassified = append(rep.Unclassified, rewrite.Address{
			App: in.App, File: configName, Where: where,
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
			where: at(line), what: what})
	}
	plexURL := target.URL()
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
