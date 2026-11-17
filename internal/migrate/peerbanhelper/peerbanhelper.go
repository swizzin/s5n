// Package peerbanhelper rewrites PeerBanHelper's own listen settings and the
// downloaders it holds.
//
// Its whole state lives under the directory `-Dpbh.datadir` names, and the chart
// points that at /config (charts/s5n/templates/workloads.yaml, PBH's
// JAVA_TOOL_OPTIONS; PBH-BTN/PeerBanHelper@v9.5.4:src/main/java/com/ghostchu/peerbanhelper/Main.java:367-378).
// The file this package edits is `<datadir>/config/config.yml` (Main.java:373
// and :414, and the image writes the template there on a first start, :543-548).
//
// That file holds no filesystem path: the values that look like paths are the
// IP-database *names* and the `database:` block, which holds an address, not a
// path (src/main/resources/config.yml at v9.5.4). What it does hold is the two
// things a box changes:
//
//   - its own listener, under `server:`: `http`, `address` and `prefix` (the
//     template's own comments: the port, the bind address, and the URL PBH
//     hands a downloader so it can fetch PBH's blocklist);
//   - one downloader per id under `client:`, each with a `type` and an
//     `endpoint` (DownloaderManagerImpl.java:65-91, which reads `client`'s keys
//     and hands each section to QBittorrent.loadFromConfig;
//     QBittorrentConfigImpl.java:31-48 reads the keys this package cares about).
//
// The chart hands PBH its API token and its "no login at the edge" switch
// through the environment (PBH_APITOKEN, PBH_WEB_REQUIRELOGIN, workload.yaml),
// so `server.token` in this file is not migrate's and is left exactly as the
// source wrote it. Wiring manages the qBittorrent downloader it owns through
// PBH's WebAPI on every run (internal/apps/pbh.go, EnsureDownloader), so a
// downloader this pass rewrites is one wiring then adopts by id: the endpoint
// it writes is the same one this pass wrote, and nothing here fights it.
package peerbanhelper

import (
	"context"
	"fmt"
	"net"
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

func init() { rewrite.Register("pbh", rewriter{}) }

const (
	// configRel is config.yml inside the directory the app is given as
	// /config: PBH's own home is `<datadir>/config`.
	configRel = "config/config.yml"

	sectionServer = "server"
	keyHTTP       = "http"
	keyAddress    = "address"
	keyPrefix     = "prefix"

	sectionClient = "client"
	keyType       = "type"
	keyName       = "name"
	keyEndpoint   = "endpoint"

	// bindAddr is what a pod needs: every container interface. A single-host
	// install commonly binds 127.0.0.1, which inside a pod is nobody but itself.
	bindAddr = "0.0.0.0"

	// downloaderKind is what an adopted downloader is filed as in wiring's
	// ownership record (internal/wiring/owned.go).
	downloaderKind = "pbh-downloader"

	// appQBittorrent is the one download client box runs, and so the one app a
	// downloader of that type can point at on the box.
	appQBittorrent = "qbittorrent"
)

type rewriter struct{}

// Check reads config.yml and reports what it would change, writing nothing.
func (rewriter) Check(_ context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(in, false)
}

// Write makes exactly the changes Check reported, in the staged copy only.
func (rewriter) Write(_ context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(in, true)
}

// decision is both passes' work, so Write cannot make a change its Check did not
// report.
type decision struct {
	file  *ymlFile
	edits []ymlEdit
	tally tally
	owned []rewrite.Owned
}

func run(in rewrite.Input, write bool) (rewrite.Report, error) {
	rep := rewrite.Report{App: in.App}
	path := filepath.Join(in.Dir, filepath.FromSlash(configRel))
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rep.Notes = append(rep.Notes, configRel+" is not in the staged config; PeerBanHelper writes one on its first start")
			return rep, nil
		}
		return rep, fmt.Errorf("peerbanhelper: %w", err)
	}
	file, err := parseYAML(raw)
	if err != nil {
		return rep, fmt.Errorf("peerbanhelper: %s: %w", configRel, err)
	}
	d := &decision{file: file, tally: tally{}}
	if err := d.listen(in); err != nil {
		return rep, err
	}
	if err := d.downloaders(in, &rep); err != nil {
		return rep, err
	}
	rep.Changes = d.tally.changes()
	rep.Owned = d.owned
	sortOwned(rep.Owned)
	if write && rep.Blocked() {
		return rep, fmt.Errorf("peerbanhelper: the plan does not settle every path and address; nothing was written")
	}
	if !write || len(d.edits) == 0 {
		return rep, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("peerbanhelper: %w", err)
	}
	if err := atomicfile.Write(path, d.file.apply(d.edits), info.Mode()); err != nil {
		return rep, fmt.Errorf("peerbanhelper: %w", err)
	}
	return rep, nil
}

// listen makes PBH's own listener the chart's: its port, every interface, and a
// blocklist URL a downloader inside the release can reach.
func (d *decision) listen(in rewrite.Input) error {
	end, ok := in.Targets[in.App]
	if !ok {
		return fmt.Errorf("peerbanhelper: the plan has no endpoint for %q", in.App)
	}
	keys := []struct {
		key   string
		value string
		what  string
	}{
		{keyHTTP, strconv.Itoa(end.Port), "port"},
		{keyAddress, bindAddr, "bind"},
		// PBH gives this URL to the downloader it asks to fetch its blocklist,
		// so it has to be the app's in-cluster address, not the source's
		// 127.0.0.1 default, which inside another pod is that pod itself.
		{keyPrefix, "http://" + net.JoinHostPort(end.Host, strconv.Itoa(end.Port)), "url"},
	}
	for _, k := range keys {
		if v, found := d.file.get(sectionServer, k.key); found && v.str == k.value {
			continue
		}
		edit, ok := d.file.set(k.value, sectionServer, k.key)
		if !ok {
			return fmt.Errorf("peerbanhelper: %s has no %s section, so its %s cannot be set to the chart's; this is not the config PeerBanHelper writes",
				configRel, sectionServer, k.key)
		}
		d.edits = append(d.edits, edit)
		d.tally.add(configRel, sectionServer+"/"+k.key, k.what)
	}
	return nil
}

// downloaders points every downloader the plan settles at the box, and reports
// the ones it does not settle.
func (d *decision) downloaders(in rewrite.Input, rep *rewrite.Report) error {
	for _, v := range d.file.values {
		if len(v.path) != 3 || v.path[0] != sectionClient || v.path[2] != keyEndpoint {
			continue
		}
		id := v.path[1]
		stored := strings.TrimSpace(v.str)
		where := sectionClient + "/" + id + "/" + keyEndpoint
		u, err := url.Parse(stored)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			rep.Notes = append(rep.Notes, fmt.Sprintf("%s %s: %q is not an http URL; left as it is", configRel, where, stored))
			continue
		}
		// A URL's port is the one it names, or the one its scheme implies: an
		// https endpoint with no port is 443, which is the port a rule has to
		// name (and the one a refusal prints).
		port := rewrite.URLPort(u)
		if p := u.Port(); p != "" {
			if _, convErr := strconv.Atoi(p); convErr != nil {
				rep.Notes = append(rep.Notes, fmt.Sprintf("%s %s: %q names a port that is not a number; left as it is", configRel, where, stored))
				continue
			}
		}
		typ := strings.ToLower(d.file.str(sectionClient, id, keyType))
		res := in.ResolveURL(u)
		switch res.Verdict {
		case plan.Leave:
			continue
		case plan.Rewrite:
		default:
			rep.Unclassified = append(rep.Unclassified, rewrite.Address{
				App: in.App, File: configRel, Where: where, Host: u.Hostname(), Port: port,
			})
			// The downloader's own type says which app its address can only
			// mean: box runs one qBittorrent, and a qBittorrent downloader the
			// plan cannot place can be nothing else. That is a proposal for
			// Settle to write as a default, never a rewrite here.
			if name, ok := d.proposes(in, typ); ok {
				rep.Defaults = append(rep.Defaults, rewrite.Default{
					Host: u.Hostname(), Port: port, App: name,
					Why: "it is a qBittorrent downloader, and this box runs one qBittorrent",
				})
			}
			continue
		}
		end, ok := in.Targets[res.App]
		if !ok {
			return fmt.Errorf("peerbanhelper: %s %s: the plan points %s at %q, which has no endpoint on the box",
				configRel, where, address(u.Hostname(), port), res.App)
		}
		// A downloader's endpoint is the plain address: PBH appends /api/v2
		// itself, and the loader strips one trailing slash
		// (QBittorrentConfigImpl.java:33-35). The box serves qBittorrent at no
		// URL base, and a path the source stored is the source's own shape.
		endpoint := end.URL()
		if endpoint != stored {
			edit, ok := d.file.set(endpoint, v.path...)
			if !ok {
				return fmt.Errorf("peerbanhelper: %s: cannot set %s", configRel, where)
			}
			d.edits = append(d.edits, edit)
			d.tally.add(configRel, where, "host")
		}
		// Only the qBittorrent types are what wiring manages, and only a
		// downloader the source already had can be adopted: it is filed under
		// the id PBH stores it under, which is the string key `client:` uses
		// (docs/box.md, "Migrated connections are adopted by ID").
		switch typ {
		case "qbittorrent", "qbittorrentee":
			name := d.file.str(sectionClient, id, keyName)
			if name == "" {
				name = id
			}
			d.owned = append(d.owned, rewrite.Owned{
				Kind: downloaderKind, StringID: id, Name: name, Target: res.App,
			})
		}
	}
	return nil
}

// proposes is the one qBittorrent a downloader of this type can only be, and
// whether the box runs exactly one: two instances and an address nothing
// settles cannot mean one of them, so nothing is proposed and the address is
// left and called out.
func (d *decision) proposes(in rewrite.Input, typ string) (string, bool) {
	switch typ {
	case "qbittorrent", "qbittorrentee":
	default:
		return "", false
	}
	return in.Sole(appQBittorrent)
}

// sortOwned orders adopted connections so a report reads the same twice.
func sortOwned(owned []rewrite.Owned) {
	sort.Slice(owned, func(i, j int) bool {
		if owned[i].Kind != owned[j].Kind {
			return owned[i].Kind < owned[j].Kind
		}
		return owned[i].StringID < owned[j].StringID
	})
}

func address(host string, port int) string {
	if port == 0 {
		return host
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// tally counts edits by file, key and kind, which is how a plan shows them.
type tally map[[3]string]int

func (t tally) add(file, where, what string) { t[[3]string{file, where, what}]++ }

func (t tally) changes() []rewrite.Change {
	out := make([]rewrite.Change, 0, len(t))
	for k, n := range t {
		out = append(out, rewrite.Change{File: k[0], Where: k[1], What: k[2], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].What < out[j].What
	})
	return out
}
