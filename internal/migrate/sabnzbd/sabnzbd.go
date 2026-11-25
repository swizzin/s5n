// Package sabnzbd rewrites the paths SABnzbd stored before box's release takes
// its config over, and switches its own web login off.
//
// SABnzbd keeps all of it in sabnzbd.ini, one ConfigObj file: the two download
// directories, a directory per category, the script and backup folders, the
// news servers and the web settings. The chart seals the two download
// directories before the first start (internal/sabconf), so what this package
// is for is everything else that would point into the source's tree.
//
// Server credentials are never touched. SABnzbd stores a server password
// obfuscated ("!!!encoded!!!" + hexlify, sabnzbd/config.py:1224-1240), so a
// rewrite would have to re-encode a secret this package never reads; the Job
// leaves it to the app's own file. The API key is the same: it is the key the
// app answers to, and a migration keeps it rather than minting another.
package sabnzbd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/sabconf"
)

func init() { rewrite.Register("sabnzbd", rewriter{}) }

// configName is SABnzbd's one settings file, in the directory the image passes
// as --config-file /config/sabnzbd.ini (apps/sabnzbd/entrypoint.sh in
// home-operations/containers).
const configName = "sabnzbd.ini"

// The keys this package reads, all in [misc] (sabnzbd/cfg.py at 5.1.3).
const (
	sectionMisc    = "misc"
	keyCompleteDir = "complete_dir"
	keyDownloadDir = "download_dir"
	keyScriptDir   = "script_dir"
	keyBackupDir   = "nzb_backup_dir"
	keyUsername    = "username"
	keyPassword    = "password"
)

type rewriter struct{}

func (rewriter) Check(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return run(in, false)
}

func (rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
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
		return rep, fmt.Errorf("sabnzbd: %w", err)
	}
	f, err := sabconf.Parse(raw)
	if err != nil {
		return rep, fmt.Errorf("sabnzbd: %s: %w", configName, err)
	}

	counts := map[[2]string]int{}
	changed := false
	note := func(where, what string) {
		counts[[2]string{where, what}]++
		changed = true
	}

	// Every absolute path goes through this app's table. A rule covers it when
	// the source's tree moved; one no rule covers is a gap, which stops the
	// write pass before anything moves.
	for _, key := range []string{keyCompleteDir, keyDownloadDir, keyScriptDir, keyBackupDir} {
		value, ok := f.Get(sectionMisc, key)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		moved, covered := in.Plan.Rewrite(in.App, strings.TrimSpace(value))
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: configName, Key: sectionMisc + "/" + key, Path: strings.TrimSpace(value),
			})
			continue
		}
		if moved == strings.TrimSpace(value) {
			continue
		}
		if err := f.Set(sectionMisc, key, moved); err != nil {
			return rep, fmt.Errorf("sabnzbd: %s: %w", configName, err)
		}
		note(sectionMisc+"/"+key, "path")
	}

	// A category's directory is joined onto complete_dir when the job is
	// post-processed, so it is a path in its own right. The category name is
	// the Arrs' and never changes: only the directory behind it does.
	for _, name := range f.Children("categories") {
		where := "categories," + name
		value, ok := f.Get(where, "dir")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		moved, covered := in.Plan.Rewrite(in.App, strings.TrimSpace(value))
		if !covered {
			rep.Uncovered = append(rep.Uncovered, plan.Uncovered{
				App: in.App, File: configName, Key: where + "/dir", Path: strings.TrimSpace(value),
			})
			continue
		}
		if moved == strings.TrimSpace(value) {
			continue
		}
		if err := f.Set(where, "dir", moved); err != nil {
			return rep, fmt.Errorf("sabnzbd: %s: %w", configName, err)
		}
		note(where+"/dir", "path")
	}

	// Its own login goes: SABnzbd asks for one only when both username and
	// password are set (sabnzbd/interface.py:358-361 at 5.1.3, check_login), and
	// the box's edge is the login a person uses. Leaving them would be a second
	// password nobody was given.
	loginOff := false
	for _, key := range []string{keyUsername, keyPassword} {
		value, ok := f.Get(sectionMisc, key)
		if !ok || value == "" {
			continue
		}
		if err := f.Set(sectionMisc, key, ""); err != nil {
			return rep, fmt.Errorf("sabnzbd: %s: %w", configName, err)
		}
		note(sectionMisc+"/"+key, "own login")
		loginOff = true
	}
	if loginOff {
		rep.Notes = append(rep.Notes, "its own login is switched off; you sign in through the box")
	}

	for key, n := range counts {
		rep.Changes = append(rep.Changes, rewrite.Change{File: configName, Where: key[0], What: key[1], Count: n})
	}
	sort.Slice(rep.Changes, func(i, j int) bool {
		if rep.Changes[i].Where != rep.Changes[j].Where {
			return rep.Changes[i].Where < rep.Changes[j].Where
		}
		return rep.Changes[i].What < rep.Changes[j].What
	})

	if write && rep.Blocked() {
		return rep, fmt.Errorf("sabnzbd: the plan does not settle every path; nothing was written")
	}
	if !write || !changed {
		return rep, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("sabnzbd: %w", err)
	}
	if err := atomicfile.Write(path, f.Bytes(), info.Mode()); err != nil {
		return rep, fmt.Errorf("sabnzbd: %w", err)
	}
	return rep, nil
}
