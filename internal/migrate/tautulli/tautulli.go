// Package tautulli rewrites the paths, the Plex connection and the login
// Tautulli stored in its config.ini.
//
// Tautulli keeps everything in one INI file (`config.ini`, plexpy/config.py at
// v2.18.2: FILENAME = "config.ini") and its database holds no absolute path:
// `newsletter_log.filename` is a bare name joined to NEWSLETTER_DIR when it is
// used (plexpy/newsletter_handler.py:242-247), and the export table has no file
// column. So the file is the whole job.
//
// The set of keys is Tautulli's own: it rewrites exactly this list when its data
// directory moves (plexpy/__init__.py:160-170 assigns https_cert,
// https_cert_chain, https_key, log_dir, backup_dir, cache_dir, export_dir and
// newsletter_dir relative to the new DATA_DIR), which is the app itself saying
// these values are absolute paths. git_path, newsletter_custom_dir and
// pms_logs_folder are paths by declaration (plexpy/config.py:108, :163, :53).
// A key is edited in the section _CONFIG_DEFINITIONS gives it and nowhere else,
// because ConfigObj would read the same name in another section as a different
// setting.
//
// The spelling is lower case, because that is the only spelling Tautulli writes
// and the only one it reads: Config._define folds each setting to
// `ini_key = name.lower()` (plexpy/config.py:473) and looks it up under that
// name, Config.write() builds the file from those keys (:528-556), and ConfigObj
// itself is case-sensitive (lib/configobj/__init__.py's Section.__getitem__ is
// dict's). The lookup accepts a key in any spelling and edits the line where it
// stands, so a file a person hand-edited is rewritten too and nothing is
// renamed: a rename would write a setting the app does not read. Sections are
// not folded, by Tautulli or here: _define passes the section through untouched.
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
// _CONFIG_DEFINITIONS puts them in (plexpy/config.py:48-61, :127-136). The names
// are lower case because that is the spelling Tautulli writes and reads a
// setting under (Config._define's `ini_key = name.lower()`, :473); the lookup
// in ini.go accepts any spelling and every report names the one the file holds.
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

// The keys that make Tautulli ask for its own password. A non-empty
// http_password is the switch (webstart.py:195-213 at v2.18.2: with one the app
// registers its auth tool and logs "authentication is enabled"; without one it
// says "authentication is disabled" and admits everyone), http_basic_auth only
// picks how it is asked, and the two flags are what make the app hash the value
// it is given. Behind box the person signs in at the edge once, so these are
// sealed off exactly as the chart's init container seals them
// (internal/ttlconf); a source's password would otherwise sit in the file until
// the first restart put it back, and would be a second password in front of an
// app nobody should have to sign in to twice.
//
// 2.19 added the two keys below them, and an empty password is not enough on its
// own any more: a start where the setup wizard never finished mints a random
// password (plexpy/__init__.py:3121-3145 at v2.19.1, generate_first_run_password
// -- which first_run_complete is what stops), and without a password the app
// refuses every request a reverse proxy forwarded (webstart.py:406-441,
// no_password_gate -- which allow_public_no_password is what stops) as well as
// minting one on a config that upgrades to 2.19 with none (config.py:796-800).
// So the migrated file says what the chart's init container will say on the next
// start, rather than waiting for it: first_run_complete in [General], and the
// gate's switch in [Advanced], the section _CONFIG_DEFINITIONS gives each.
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

	// And its own login goes, said out loud: the one the person uses is the
	// box's, and an app that asks for a second password is a password they were
	// never given.
	edits = append(edits, loginOff(f)...)
	rep.Notes = append(rep.Notes, "its own login is switched off; you sign in through the box")
	return edits, nil
}

// loginOff is the edits that switch Tautulli's own web login off. Only the keys
// the file holds are edited: one that is absent is one the app's defaults answer
// for, and writing it would be this package inventing a setting. A config.ini
// Tautulli wrote itself holds all of them -- Config.write() builds the file from
// _CONFIG_DEFINITIONS -- which is the only kind of file a migration reads.
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

// at is one line's section/key as the file spells it, which is what a report
// names: a plan that said General/HTTP_ROOT for a file holding
// General/http_root would name a line the operator cannot find.
func at(line iniLine) string { return line.section + "/" + line.key }

// planPlex is the Plex connection. pms_ip holds the host and pms_port the port;
// pms_url is the same address as a URL, set when the server was entered by hand
// (plexpy/webserve.py:359 reads `PMS_URL_OVERRIDE or PMS_URL`), so it moves with
// them or a stale URL wins over the right host.
func planPlex(in rewrite.Input, f *iniFile, rep *rewrite.Report) ([]edit, error) {
	ipLine, haveIP := f.get(sectionPMS, keyPMSIP)
	urlLine, haveURL := f.get(sectionPMS, keyPMSURL)
	host := strings.TrimSpace(ipLine.value)
	port := atoi(strings.TrimSpace(f.value(sectionPMS, keyPMSPort)))
	// A URL column is a whole URL, so its path is part of what settles it on a
	// host that serves several apps; the two columns are a host and a port.
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
