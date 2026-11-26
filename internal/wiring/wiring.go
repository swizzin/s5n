package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/apps"
)

// The two Arrs a Bazarr instance reads, named as the app names its sections.
const (
	sideSonarr = "sonarr"
	sideRadarr = "radarr"
)

// qbitClientName is what every app's own connection to qBittorrent is called, and
// the name the Job writes when it creates the object.
const qbitClientName = "qBittorrent"

// sabClientName is what SABnzbd is called in an Arr's download client list, beside
// qBittorrent's: two clients of different protocols.
const sabClientName = "SABnzbd"

// Writes counts the application objects the run changed. Directories are reported
// per step instead, because a directory is not an owned object. The e2e asserts a
// second run reports zero created and zero updated.
type Writes struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// Step is one line of the receipt. Drift lists owned fields that had been changed on
// the live object and were restored; a secret field is never represented in it.
type Step struct {
	Name   string             `json:"name"`
	Result string             `json:"result"` // ok | skipped | failed
	Detail string             `json:"detail,omitempty"`
	Drift  []apps.FieldChange `json:"drift,omitempty"`
}

// Result is the whole receipt, published into the wiring ConfigMap. OwnedRecord is
// published separately as owned.json, so it is excluded here.
type Result struct {
	Status          string          `json:"status"` // ok | failed
	StartedAt       time.Time       `json:"startedAt"`
	FinishedAt      time.Time       `json:"finishedAt"`
	DurationSeconds float64         `json:"durationSeconds"`
	Writes          Writes          `json:"writes"`
	Steps           []Step          `json:"steps"`
	Hardlinks       []HardlinkCheck `json:"hardlinks,omitempty"`
	Error           string          `json:"error,omitempty"`
	OwnedRecord     *OwnedRecord    `json:"-"`
}

// Runner executes the wiring. Log receives human-readable progress lines.
type Runner struct {
	Config  Config
	Log     io.Writer
	Publish func(ctx context.Context, data map[string]string) error
	Now     func() time.Time
	// HardlinkCheck is injectable so tests can avoid a real filesystem proof.
	HardlinkCheck func(savePath, rootFolder string) *HardlinkCheck
	// ContractRetry bounds how long qBittorrent gets to settle on the data
	// contract; zero means 90 seconds.
	ContractRetry time.Duration
	// LoadOwned reads owned.json from the wiring ConfigMap, nil with no error when
	// the key or ConfigMap is absent. Injectable, so unit tests need no cluster;
	// nil means every run adopts by name.
	LoadOwned func(ctx context.Context) ([]byte, error)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		fmt.Fprintf(r.Log, format+"\n", args...)
	}
}

// stepLog is one line per check: the check's name and what it cost. box reads the
// newest line and shows it beside a spinner.
func (r *Runner) stepLog(name, detail string) {
	r.logf("%s: %s", name, detail)
}

func (r *Runner) contractRetry() time.Duration {
	if r.ContractRetry > 0 {
		return r.ContractRetry
	}
	return 90 * time.Second
}

func (r *Runner) hardlink() func(string, string) *HardlinkCheck {
	if r.HardlinkCheck != nil {
		return r.HardlinkCheck
	}
	return CheckHardlink
}

// Run performs the wiring and always returns a receipt; a non-nil error means the
// release is not wired correctly.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	res := &Result{Status: "ok", StartedAt: r.now()}
	err := r.work(ctx, res)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
	}
	res.FinishedAt = r.now()
	res.DurationSeconds = res.FinishedAt.Sub(res.StartedAt).Seconds()

	if r.Publish != nil {
		if perr := r.publish(ctx, res); perr != nil {
			if err == nil {
				res.Status = "failed"
				res.Error = fmt.Sprintf("publish wiring result: %v", perr)
				err = fmt.Errorf("%s", res.Error)
			}
			r.stepLog("publish", "failed: "+perr.Error())
		}
	}
	r.logf("%s (%.1fs, %d created / %d updated / %d unchanged)",
		strings.ToUpper(res.Status), res.DurationSeconds, res.Writes.Created, res.Writes.Updated, res.Writes.Unchanged)
	return res, err
}

// ensure records a reconciling step: it counts the write and turns an error into
// the run's failure.
func (r *Runner) ensure(res *Result, name string, change apps.Change, err error) error {
	if err != nil {
		res.Steps = append(res.Steps, Step{Name: name, Result: "failed", Detail: err.Error()})
		r.stepLog(name, "failed: "+err.Error())
		return fmt.Errorf("%s: %w", name, err)
	}
	switch change {
	case apps.Created:
		res.Writes.Created++
	case apps.Updated:
		res.Writes.Updated++
	case apps.Skipped:
		res.Steps = append(res.Steps, Step{Name: name, Result: stepSkipped})
		r.stepLog(name, stepSkipped)
		return nil
	default:
		res.Writes.Unchanged++
	}
	res.Steps = append(res.Steps, Step{Name: name, Result: "ok", Detail: change.String()})
	r.stepLog(name, change.String())
	return nil
}

// ensureResult is ensure's counterpart for an owned object: it records drift and a
// recreated object's previous id on top of counting the write.
func (r *Runner) ensureResult(res *Result, name string, result apps.EnsureResult, err error) error {
	if err != nil {
		res.Steps = append(res.Steps, Step{Name: name, Result: "failed", Detail: err.Error()})
		r.stepLog(name, "failed: "+err.Error())
		return fmt.Errorf("%s: %w", name, err)
	}
	switch result.Change {
	case apps.Created:
		res.Writes.Created++
	case apps.Updated:
		res.Writes.Updated++
	case apps.Skipped:
		res.Steps = append(res.Steps, Step{Name: name, Result: stepSkipped})
		r.stepLog(name, stepSkipped)
		return nil
	default:
		res.Writes.Unchanged++
	}
	detail := result.Change.String()
	switch {
	case result.Recreated && result.PreviousStringID != "":
		detail = fmt.Sprintf("recreated (previous id %s)", result.PreviousStringID)
	case result.Recreated:
		detail = fmt.Sprintf("recreated (previous id %d)", result.PreviousID)
	case result.Note != "":
		detail = result.Note
	}
	step := Step{Name: name, Result: "ok", Detail: detail, Drift: result.Drift}
	res.Steps = append(res.Steps, step)
	if len(result.Drift) > 0 {
		r.stepLog(name, fmt.Sprintf("%s (%d fields restored)", detail, len(result.Drift)))
	} else {
		r.stepLog(name, detail)
	}
	return nil
}

// loadOwned reads owned.json. An absent or unparsable record falls back to an empty
// one, which makes every lookup adopt by name; note says why.
//
// A failed read is different and stops the run: adopting by name after an API blip
// would miss a renamed object, create a duplicate, and record the duplicate's id.
func (r *Runner) loadOwned(ctx context.Context) (rec *OwnedRecord, note string, err error) {
	if r.LoadOwned == nil {
		// Ownership tracking is not wired up at all (e.g. --no-publish).
		return NewOwnedRecord(), "", nil
	}
	raw, err := r.LoadOwned(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("read owned.json: %w", err)
	}
	if raw == nil {
		return NewOwnedRecord(), "no owned.json yet: adopting existing objects by name", nil
	}
	rec, perr := ParseOwnedRecord(raw)
	if perr != nil {
		r.logf("owned.json: %v; falling back to adoption by name", perr)
		return NewOwnedRecord(), "owned.json unreadable: adopting existing objects by name", nil
	}
	return rec, "", nil
}

// okStep records a check that has no desired-state diff of its own.
func (r *Runner) okStep(res *Result, name, detail string) {
	res.Steps = append(res.Steps, Step{Name: name, Result: "ok", Detail: detail})
	r.stepLog(name, "ok ("+detail+")")
}

func (r *Runner) work(ctx context.Context, res *Result) error {
	c := r.Config
	timeout := timeoutDuration(c.TimeoutSeconds)

	r.logf("s5n wiring: %d Arr instance(s), qBittorrent=%v, Prowlarr=%v",
		len(c.Instances), c.Qbittorrent != nil, c.Prowlarr != nil)

	// Ownership (AGENTS.md invariant 11): only an object whose id is recorded here is
	// ours to update, drift-correct or recreate.
	owned, note, err := r.loadOwned(ctx)
	if err != nil {
		// res.OwnedRecord stays nil, so the receipt is published without an
		// owned.json key and the record already in the ConfigMap is left alone.
		return r.ensure(res, "wiring/owned", apps.Unchanged, err)
	}
	res.OwnedRecord = owned
	if note != "" {
		r.okStep(res, "wiring/owned", note)
	}

	// The data directories before any app step: the Arr root-folder step rejects a
	// library path that does not exist, so this makes an empty data path possible at
	// all.
	dirs, err := EnsureDataDirs(c.Instances)
	for _, d := range dirs {
		detail := "exists"
		if d.Created {
			detail = "created"
		}
		r.okStep(res, d.Name, detail)
	}
	if err != nil {
		return r.ensure(res, "data/dirs", apps.Unchanged, err)
	}

	// Every app calls qBittorrent with an empty username and password: the WebUI
	// skips its login for the release's addresses (docs/box.md, "Logins"), so no
	// app's database holds a credential.
	var qbit *apps.Qbit
	if c.Qbittorrent != nil {
		qbit = apps.NewQbit(c.Qbittorrent.URL, "", "")
		r.logf("waiting for qBittorrent at %s", c.Qbittorrent.URL)
		if err := qbit.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "qbittorrent/ready", apps.Unchanged, err)
		}
		version, _ := qbit.Version(ctx)
		r.okStep(res, "qbittorrent/ready", version)

		// qBittorrent's image re-applies its own save path, so the config seed cannot
		// be trusted alone: set it through the API, then assert.
		change := apps.Unchanged
		err = apps.Retry(ctx, r.contractRetry(), func(ctx context.Context) error {
			var e error
			change, e = qbit.EnsureDataContract(ctx)
			return e
		})
		if err := r.ensure(res, "qbittorrent/data-contract", change, err); err != nil {
			return err
		}

		// One category per download directory, named the way the instance's torrents
		// are; a migrated box keeps the source's name.
		seen := map[string]bool{}
		for _, inst := range c.Instances {
			category := downloadCategory(inst)
			if seen[category] {
				continue
			}
			seen[category] = true
			change, err := qbit.EnsureCategory(ctx, category, inst.SavePath, c.Qbittorrent.SavePath)
			if err := r.ensure(res, "qbittorrent/category/"+category, change, err); err != nil {
				return err
			}
		}
	}

	// SABnzbd is the second download client: the same categories, each with its
	// directory under complete_dir. It comes before the Arrs, because an Arr's client
	// test dials SAB and its category has to exist by then.
	var sab *apps.Sabnzbd
	sabHost, sabPort, sabKey := "", 0, ""
	if c.Sabnzbd != nil {
		var err error
		if sabKey, err = ReadSecret(c.Sabnzbd.APIKeyFile); err != nil {
			return r.ensure(res, "sabnzbd/credentials", apps.Unchanged, err)
		}
		sab = apps.NewSabnzbd(c.Sabnzbd.URL, c.Sabnzbd.URLBase, sabKey)
		if sabHost, sabPort, err = hostPort(c.Sabnzbd.URL); err != nil {
			return r.ensure(res, "sabnzbd/url", apps.Unchanged, err)
		}
		r.logf("waiting for SABnzbd at %s%s", c.Sabnzbd.URL, c.Sabnzbd.URLBase)
		if err := sab.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "sabnzbd/ready", apps.Unchanged, err)
		}
		version, _ := sab.Version(ctx)
		r.okStep(res, "sabnzbd/ready", version)

		// The image's defaults are on /config, where no import can be a hardlink; the
		// chart seals them and this proves the running app agrees.
		change, err := sab.EnsureDataContract(ctx, apps.Config{
			CompleteDir: c.Sabnzbd.SavePath,
			DownloadDir: c.Sabnzbd.DownloadDir,
		})
		if err := r.ensure(res, "sabnzbd/data-contract", change, err); err != nil {
			return err
		}

		// One category per download directory, with its directory under
		// complete_dir. An unknown name falls back to "*" with no directory, so the
		// category is created before any Arr can grab into it.
		seen := map[string]bool{}
		for _, inst := range c.Instances {
			category := downloadCategory(inst)
			if seen[category] {
				continue
			}
			seen[category] = true
			change, err := sab.EnsureCategory(ctx, category, category)
			if err := r.ensure(res, "sabnzbd/category/"+category, change, err); err != nil {
				return err
			}
		}
	}

	// Every Arr gets the same qBittorrent, its own category, its root folder and
	// hardlink imports. The Arr keys are collected for Prowlarr.
	arrKeys := map[string]string{}
	host, port := "", 0
	if c.Qbittorrent != nil {
		var err error
		if host, port, err = hostPort(c.Qbittorrent.URL); err != nil {
			return r.ensure(res, "qbittorrent/url", apps.Unchanged, err)
		}
	}

	for _, inst := range c.Instances {
		key, err := ReadSecret(inst.APIKeyFile)
		if err != nil {
			return r.ensure(res, inst.Name+"/credentials", apps.Unchanged, err)
		}
		arrKeys[inst.Name] = key
		arr := apps.NewArr(apps.Kind(inst.Kind), inst.Name, inst.URL, inst.URLBase, key)
		r.logf("waiting for %s at %s%s", inst.Name, inst.URL, inst.URLBase)
		if err := arr.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, inst.Name+"/ready", apps.Unchanged, err)
		}
		r.okStep(res, inst.Name+"/ready", inst.URL+inst.URLBase)

		if qbit != nil {
			result, err := arr.EnsureDownloadClient(ctx, apps.DownloadClientSpec{
				Name: qbitClientName,
				// The directory the instance's torrents are in, which a
				// migrated box keeps.
				Category: downloadCategory(inst),
				Host:     host,
				Port:     port,
				// Owned as empty: qBittorrent skips its login for the release's
				// addresses, so one an older install stored is overwritten here.
				Username: "",
				Password: "",
			}, owned.ArrDownloadClientID(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetArrDownloadClientID(inst.Name, result.ID)
			}
			if err := r.ensureResult(res, inst.Name+"/downloadclient", result, err); err != nil {
				return err
			}
		}
		if sab != nil {
			// The same rule as qBittorrent's client: the live object is the operator's,
			// and only the data-contract fields are overlaid.
			result, err := arr.EnsureDownloadClient(ctx, apps.DownloadClientSpec{
				Implementation: "Sabnzbd",
				Protocol:       "usenet",
				Name:           sabClientName,
				// SABnzbd folds every category name and the Arr refuses one the app
				// does not list, so the client carries the folded name; the directory
				// keeps the Arrs' spelling (4K/Movies under 4k/movies).
				Category: strings.ToLower(downloadCategory(inst)),
				Host:     sabHost,
				Port:     sabPort,
				URLBase:  c.Sabnzbd.URLBase,
				APIKey:   sabKey,
			}, owned.ArrSabnzbdClientID(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetArrSabnzbdClientID(inst.Name, result.ID)
			}
			if err := r.ensureResult(res, inst.Name+"/sabnzbd", result, err); err != nil {
				return err
			}
		}
		change, err := arr.EnsureRootFolder(ctx, inst.RootFolder)
		if err := r.ensure(res, inst.Name+"/rootfolder", change, err); err != nil {
			return err
		}
		change, err = arr.EnsureHardlinks(ctx)
		if err := r.ensure(res, inst.Name+"/hardlinks", change, err); err != nil {
			return err
		}
	}

	// Bazarr reads one Sonarr and one Radarr each, so a box with an HD and a 4K pair
	// runs one per pair. A side this box runs no Arr for is switched off rather than
	// left pointing at a Service that does not answer.
	for _, bz := range c.Bazarrs {
		key, err := ReadSecret(bz.APIKeyFile)
		if err != nil {
			return r.ensure(res, bz.Name+"/credentials", apps.Unchanged, err)
		}
		b := apps.NewBazarr(bz.URL, bz.URLBase, key)
		r.logf("waiting for %s at %s%s", bz.Name, bz.URL, bz.URLBase)
		if err := b.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, bz.Name+"/ready", apps.Unchanged, err)
		}
		r.okStep(res, bz.Name+"/ready", bz.URL+bz.URLBase)
		for _, side := range []struct {
			kind string
			cfg  *BazarrSide
		}{{sideSonarr, bz.Sonarr}, {sideRadarr, bz.Radarr}} {
			name := bz.Name + "/" + side.kind
			spec := apps.SubtitleSource{Kind: side.kind}
			if side.cfg != nil {
				apiKey, err := ReadSecret(side.cfg.APIKeyFile)
				if err != nil {
					return r.ensure(res, name, apps.Unchanged, err)
				}
				spec.Enabled = true
				spec.Host = side.cfg.Host
				spec.Port = side.cfg.Port
				spec.URLBase = side.cfg.URLBase
				spec.APIKey = apiKey
			}
			result, err := b.EnsureSource(ctx, spec)
			if err := r.ensureResult(res, name, result, err); err != nil {
				return err
			}
		}
	}

	if c.Prowlarr != nil {
		key, err := ReadSecret(c.Prowlarr.APIKeyFile)
		if err != nil {
			return r.ensure(res, "prowlarr/credentials", apps.Unchanged, err)
		}
		p := apps.NewProwlarr(c.Prowlarr.URL, c.Prowlarr.URLBase, c.Prowlarr.SelfURL, key)
		r.logf("waiting for Prowlarr at %s%s", c.Prowlarr.URL, c.Prowlarr.URLBase)
		if err := p.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "prowlarr/ready", apps.Unchanged, err)
		}
		r.okStep(res, "prowlarr/ready", c.Prowlarr.URL+c.Prowlarr.URLBase)
		for _, inst := range c.Instances {
			var impl string
			switch apps.Kind(inst.Kind) {
			case apps.Sonarr:
				impl = "Sonarr"
			case apps.Radarr:
				impl = "Radarr"
			case apps.Lidarr:
				// Prowlarr registers Lidarr as "Lidarr" and talks to it over
				// Lidarr's /api/v1 (prowlarr@v3.1.0.5799:
				// src/NzbDrone.Core/Applications/Lidarr/Lidarr.cs).
				impl = "Lidarr"
			default:
				return r.ensure(res, "prowlarr/application/"+inst.Name, apps.Unchanged,
					fmt.Errorf("instance %q: unknown Arr kind %q has no Prowlarr application implementation", inst.Name, inst.Kind))
			}
			result, err := p.EnsureApplication(ctx, apps.ApplicationSpec{
				Name:           inst.Name,
				Implementation: impl,
				BaseURL:        strings.TrimRight(inst.URL, "/") + inst.URLBase,
				APIKey:         arrKeys[inst.Name],
			}, owned.ProwlarrApplicationID(inst.Name), owned.ProwlarrApplicationFingerprint(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetProwlarrApplicationID(inst.Name, result.ID)
				// The Arr's key, proven by Prowlarr's own test above: recorded only
				// now, like every other fingerprint.
				owned.SetProwlarrApplicationFingerprint(inst.Name, apps.SecretFingerprint(arrKeys[inst.Name]))
			}
			if err := r.ensureResult(res, "prowlarr/application/"+inst.Name, result, err); err != nil {
				return err
			}
		}
	}

	// qui and autobrr keep their own user tables and hand out an API key only to a
	// session (docs/box.md). The chart gives each a password in the release Secret
	// and an init container that writes it at every start; the Job opens a session
	// with it. A rejected login is the one state an operator must fix by hand.
	if c.Qui != nil {
		password, err := ReadSecret(c.Qui.PasswordFile)
		if err != nil {
			return r.ensure(res, "qui/credentials", apps.Unchanged, err)
		}
		q := apps.NewQui(c.Qui.URL, c.Qui.URLBase, c.Qui.Username, password)
		r.logf("waiting for qui at %s%s", c.Qui.URL, c.Qui.URLBase)
		// qui exits instead of serving when its OIDC issuer is unreachable, so a
		// timeout here is a configuration error.
		if err := q.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "qui/ready", apps.Unchanged, err)
		}
		r.okStep(res, "qui/ready", c.Qui.URL+c.Qui.URLBase)
		if err := q.Login(ctx); err != nil {
			return r.ensure(res, "qui/login", apps.Unchanged, loginFailed(err, "qui"))
		}
		r.okStep(res, "qui/login", c.Qui.Username)
		if c.Qbittorrent == nil {
			// An instance needs a qBittorrent to point at; saying so beats a step that
			// never runs.
			if err := r.ensure(res, "qui/instance", apps.Skipped, nil); err != nil {
				return err
			}
		} else {
			result, err := q.EnsureInstance(ctx, apps.QuiInstanceSpec{
				Name: qbitClientName,
				Host: strings.TrimRight(c.Qbittorrent.URL, "/"),
				// Owned as empty: qBittorrent skips its login for the release's
				// addresses, so qui's instance holds no credential either.
				Username: "",
				Password: "",
				// /data is the same path in every app and qBittorrent saves under
				// /data/downloads/<category>, so no second path mapping is needed.
				HasLocalFilesystemAccess: true,
			}, owned.QuiInstanceID())
			if err == nil && result.ID != 0 {
				owned.SetQuiInstanceID(result.ID)
			}
			if err := r.ensureResult(res, "qui/instance", result, err); err != nil {
				return err
			}
		}
		// One Arr instance per Sonarr and Radarr: qui's integration resolves a title
		// to the ID it searches for, made from qui's pod, so a row that cannot reach
		// its Arr is qui's own test failing -- the receipt's proof that the catalog's
		// `calls` and the callee's ingress agree.
		//
		// The address carries the Arr's URL base, as autobrr's does. Lidarr has no
		// instance type in qui (internal/models/arr_instance.go at v1.36.1), so it is
		// reported skipped.
		for _, inst := range c.Instances {
			var kind string
			switch apps.Kind(inst.Kind) {
			case apps.Sonarr:
				// qui's instance types are the app names themselves
				// (internal/models/arr_instance.go at v1.36.1).
				kind = string(apps.Sonarr)
			case apps.Radarr:
				kind = string(apps.Radarr)
			default:
				name := "qui/arr/" + inst.Name
				res.Steps = append(res.Steps, Step{Name: name, Result: "skipped",
					Detail: fmt.Sprintf("qui has no %s instance type", inst.Kind)})
				r.stepLog(name, "skipped")
				continue
			}
			result, err := q.EnsureArrInstance(ctx, apps.QuiArrSpec{
				Name:    inst.Name,
				Type:    kind,
				BaseURL: strings.TrimRight(inst.URL, "/") + inst.URLBase,
				APIKey:  arrKeys[inst.Name],
			}, owned.QuiArrInstanceID(inst.Name), owned.QuiArrInstanceFingerprint(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetQuiArrInstanceID(inst.Name, result.ID)
				// Only after qui's own test proved the row: a fingerprint recorded for
				// a write nothing accepted would hide a rotation.
				owned.SetQuiArrInstanceFingerprint(inst.Name, apps.SecretFingerprint(arrKeys[inst.Name]))
			}
			if err := r.ensureResult(res, "qui/arr/"+inst.Name, result, err); err != nil {
				return err
			}
		}
	}

	if c.Autobrr != nil {
		password, err := ReadSecret(c.Autobrr.PasswordFile)
		if err != nil {
			return r.ensure(res, "autobrr/credentials", apps.Unchanged, err)
		}
		a := apps.NewAutobrr(c.Autobrr.URL, c.Autobrr.URLBase, c.Autobrr.Username, password)
		r.logf("waiting for autobrr at %s%s", c.Autobrr.URL, c.Autobrr.URLBase)
		// autobrr starts with an unreachable OIDC issuer and retries in the
		// background, so readiness is only the HTTP API's.
		if err := a.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "autobrr/ready", apps.Unchanged, err)
		}
		r.okStep(res, "autobrr/ready", c.Autobrr.URL+c.Autobrr.URLBase)
		if err := a.Login(ctx); err != nil {
			return r.ensure(res, "autobrr/login", apps.Unchanged, loginFailed(err, "autobrr"))
		}
		r.okStep(res, "autobrr/login", c.Autobrr.Username)
		if c.Qbittorrent != nil {
			result, err := a.EnsureDownloader(ctx, apps.DownloaderSpec{
				Name: qbitClientName,
				Type: "QBITTORRENT",
				Host: strings.TrimRight(c.Qbittorrent.URL, "/"),
				// Owned as empty; autobrr holds no qBittorrent credential.
				Username: "",
				Password: "",
			}, owned.AutobrrClientID(qbitClientName), owned.AutobrrClientFingerprint(qbitClientName))
			if err == nil && result.ID != 0 {
				owned.SetAutobrrClientID(qbitClientName, result.ID)
			}
			if err := r.ensureResult(res, "autobrr/downloader/qbittorrent", result, err); err != nil {
				return err
			}
		}
		// One client per Arr instance, HD and 4K alike. The host carries the Arr's
		// base path, which autobrr uses verbatim, and the key is the Arr's own from
		// the release Secret. Filters and actions are the operator's.
		for _, inst := range c.Instances {
			var kind string
			switch apps.Kind(inst.Kind) {
			case apps.Sonarr:
				kind = "SONARR"
			case apps.Radarr:
				kind = "RADARR"
			case apps.Lidarr:
				// autobrr's Arr clients are one type per app, and LIDARR is its own
				// (internal/domain/downloader.go at v1.91.1: DownloaderTypeLidarr).
				kind = "LIDARR"
			default:
				return r.ensure(res, "autobrr/downloader/"+inst.Name, apps.Unchanged,
					fmt.Errorf("instance %q: unknown Arr kind %q has no autobrr download client type", inst.Name, inst.Kind))
			}
			result, err := a.EnsureDownloader(ctx, apps.DownloaderSpec{
				Name:   inst.Name,
				Type:   kind,
				Host:   strings.TrimRight(inst.URL, "/") + inst.URLBase,
				APIKey: arrKeys[inst.Name],
			}, owned.AutobrrClientID(inst.Name), owned.AutobrrClientFingerprint(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetAutobrrClientID(inst.Name, result.ID)
				owned.SetAutobrrClientFingerprint(inst.Name, apps.SecretFingerprint(arrKeys[inst.Name]))
			}
			if err := r.ensureResult(res, "autobrr/downloader/"+inst.Name, result, err); err != nil {
				return err
			}
		}
	}

	// PeerBanHelper reads qBittorrent's Web API for peers to ban, so it needs one
	// downloader. Its id is a key in its own config, and PBH answers HEALTHY only
	// once its poll loop has logged in with what was written.
	if c.Pbh != nil {
		token, err := ReadSecret(c.Pbh.TokenFile)
		if err != nil {
			return r.ensure(res, "pbh/credentials", apps.Unchanged, err)
		}
		p := apps.NewPbh(c.Pbh.URL, token)
		r.logf("waiting for PeerBanHelper at %s", c.Pbh.URL)
		if err := p.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "pbh/ready", apps.Unchanged, pbhReady(err))
		}
		detail := c.Pbh.URL
		if version, verr := p.Version(ctx); verr == nil && version != "" {
			detail = fmt.Sprintf("%s (version=%s)", detail, version)
		}
		r.okStep(res, "pbh/ready", detail)

		if c.Qbittorrent == nil {
			// Nothing to point it at: saying so beats a step that never runs.
			if err := r.ensure(res, "pbh/downloader", apps.Skipped, nil); err != nil {
				return err
			}
		} else {
			result, err := p.EnsureDownloader(ctx, apps.PbhDownloaderSpec{
				ID:       "qbittorrent",
				Name:     qbitClientName,
				Endpoint: strings.TrimRight(c.Qbittorrent.URL, "/"),
				// Owned as empty: PBH reads qBittorrent's API without a login.
				Username: "",
				Password: "",
			}, owned.PbhDownloaderID())
			if err == nil {
				var status string
				if status, err = p.WaitHealthy(ctx, result.StringID, timeout); err != nil {
					err = fmt.Errorf("pbh downloader %q is %s: %w", result.StringID, status, err)
				}
			}
			if err == nil && result.StringID != "" {
				owned.SetPbhDownloaderID(result.StringID)
			}
			if err := r.ensureResult(res, "pbh/downloader", result, err); err != nil {
				return err
			}
		}
	}

	// Prove every distinct download/library pair: one claim is not one filesystem,
	// and a disk mounted inside it passes a tv check while turning every 4K import
	// into a copy.
	proven := map[[2]string]bool{}
	for _, inst := range c.Instances {
		pair := [2]string{inst.SavePath, inst.RootFolder}
		if proven[pair] {
			continue
		}
		proven[pair] = true
		name := "data/hardlink/" + inst.Category
		check := r.hardlink()(inst.SavePath, inst.RootFolder)
		res.Hardlinks = append(res.Hardlinks, *check)
		if !check.OK {
			return r.ensure(res, name, apps.Unchanged,
				fmt.Errorf("hardlink proof failed between %s and %s: %s", inst.SavePath, inst.RootFolder, check.Error))
		}
		r.okStep(res, name, fmt.Sprintf("nlink=%d sameDevice=%v", check.Nlink, check.SameDevice))
	}

	// Jellyfin, before Plex: both are media servers, and this one's account is
	// the only thing here that has to exist before a person can open the app.
	if c.Jellyfin != nil {
		if err := r.wireJellyfin(ctx, res, c.Jellyfin); err != nil {
			return err
		}
	}

	// Plex, then the two apps that attach to it (PLAN.md's catalog order). A missing
	// or failed claim token leaves Plex unclaimed -- /identity still answers, so that
	// is not a failure -- and the media rows report pending. Tautulli's PMS link uses
	// the release Secret's plex-token, which is independent of the claim token.
	var plexIdentity *apps.Identity
	var plex *apps.Plex
	if c.Plex != nil {
		plex = apps.NewPlex(c.Plex.URL)
		r.logf("waiting for Plex at %s", c.Plex.URL)
		if err := plex.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "plex/ready", apps.Unchanged, err)
		}
		ident, err := plex.Identity(ctx)
		if err != nil {
			return r.ensure(res, "plex/ready", apps.Unchanged, err)
		}
		plexIdentity = &ident
		r.okStep(res, "plex/ready", fmt.Sprintf("version=%s claimed=%v", ident.Version, ident.Claimed))
		if ident.Claimed {
			r.okStep(res, "plex/claim", "machineIdentifier="+ident.MachineIdentifier)
		} else {
			// Skipping the claim is allowed, but on an unclaimed server the first
			// visitor to Seerr owns it. Say that only when Seerr is in the release.
			detail := "no claim token (or it expired before Plex started): Plex is not signed in to a Plex.tv account"
			if c.Seerr != nil {
				detail = "Plex is not claimed yet: until it is, whoever signs in to Seerr first becomes its owner. Claim it at https://plex.tv/claim"
			}
			res.Steps = append(res.Steps, Step{Name: "plex/claim", Result: stepPending, Detail: detail})
			r.stepLog("plex/claim", "pending (no claim token)")
		}
	}

	if c.Tautulli != nil {
		if plexIdentity == nil {
			res.Steps = append(res.Steps, Step{Name: "tautulli/plex", Result: stepPending, Detail: "plex is not deployed in this release"})
		} else {
			key, err := ReadSecret(c.Tautulli.APIKeyFile)
			if err != nil {
				return r.ensure(res, "tautulli/credentials", apps.Unchanged, err)
			}
			// The PMS link is token-based: trusting a network range would expose the
			// server unauthenticated through the Gateway, whose Envoy comes from a pod
			// IP. No token therefore means pending, not a tokenless write.
			plexToken, haveToken, err := ReadOptionalSecret(c.Tautulli.PlexTokenFile)
			if err != nil {
				return r.ensure(res, "tautulli/plex", apps.Unchanged, err)
			}
			// Why the link will not be written, "" when it will. The token is proven
			// against Plex before touching Tautulli: its identity check reads Plex's
			// unauthenticated /identity, and /library/sections answers 401 to a token
			// Plex does not accept.
			pendingReason, pendingShort := "", ""
			if !haveToken {
				// box fills the token in from Plex's own settings on every apply, so
				// claiming the server is the person's part here.
				pendingReason = plexTokenMissing(plexIdentity, "Tautulli")
				pendingShort = "no Plex token"
			} else if err := plex.VerifyToken(ctx, plexToken); err != nil {
				detail, ok := plexTokenPending(plexIdentity, err)
				if !ok {
					return r.ensure(res, "tautulli/plex", apps.Unchanged, err)
				}
				pendingReason, pendingShort = detail, "plex is not claimed"
			}
			tt := apps.NewTautulli(c.Tautulli.URL, key)
			r.logf("waiting for Tautulli at %s", c.Tautulli.URL)
			if err := tt.WaitReady(ctx, timeout); err != nil {
				return r.ensure(res, "tautulli/ready", apps.Unchanged, err)
			}
			r.okStep(res, "tautulli/ready", c.Tautulli.URL)
			if pendingReason != "" {
				res.Steps = append(res.Steps, Step{Name: "tautulli/plex", Result: stepPending, Detail: pendingReason})
				r.stepLog("tautulli/plex", "pending ("+pendingShort+")")
			} else {
				plexHost, plexPort, err := hostPort(c.Plex.URL)
				if err != nil {
					return r.ensure(res, "tautulli/plex", apps.Unchanged, err)
				}
				// Tautulli withholds pms_token, so the fingerprint of the token last
				// written and proven makes a Secret rotation visible (owned.json).
				change, err := tt.EnsurePlexConnection(ctx, plexHost, plexPort,
					plexIdentity.MachineIdentifier, plexToken, owned.TautulliPlexTokenFingerprint())
				if err := r.ensure(res, "tautulli/plex", change, err); err != nil {
					return err
				}
				// Only after the write and its proof succeeded: a fingerprint recorded
				// for an unaccepted write would mask a rotation.
				owned.SetTautulliPlexTokenFingerprint(apps.PlexTokenFingerprint(plexToken))
			}
		}
	}

	if c.Maintainerr != nil {
		// Maintainerr's Plex connection is a row in its settings and the one thing
		// that must be right before any rule can run: one it cannot reach deletes
		// nothing. It takes the same server token as Tautulli's link.
		if plexIdentity == nil {
			res.Steps = append(res.Steps, Step{Name: "maintainerr/plex", Result: stepPending, Detail: "plex is not deployed in this release"})
		} else {
			plexToken, haveToken, err := ReadOptionalSecret(c.Maintainerr.PlexTokenFile)
			if err != nil {
				return r.ensure(res, "maintainerr/plex", apps.Unchanged, err)
			}
			// Same rule as Tautulli's: prove the token against Plex first, and an
			// unclaimed server refuses every token, so that state is pending.
			pendingReason, pendingShort := "", ""
			if !haveToken {
				pendingReason = plexTokenMissing(plexIdentity, "Maintainerr")
				pendingShort = "no Plex token"
			} else if err := plex.VerifyToken(ctx, plexToken); err != nil {
				detail, ok := plexTokenPending(plexIdentity, err)
				if !ok {
					return r.ensure(res, "maintainerr/plex", apps.Unchanged, err)
				}
				pendingReason, pendingShort = detail, "plex is not claimed"
			}
			me := apps.NewMaintainerr(c.Maintainerr.URL, c.Maintainerr.URLBase)
			r.logf("waiting for Maintainerr at %s%s", c.Maintainerr.URL, c.Maintainerr.URLBase)
			// Its readiness answers from the app's own database, which says the
			// settings written here land in a store that works.
			if err := me.WaitReady(ctx, timeout); err != nil {
				return r.ensure(res, "maintainerr/ready", apps.Unchanged, err)
			}
			r.okStep(res, "maintainerr/ready", c.Maintainerr.URL+c.Maintainerr.URLBase)
			if pendingReason != "" {
				res.Steps = append(res.Steps, Step{Name: "maintainerr/plex", Result: stepPending, Detail: pendingReason})
				r.stepLog("maintainerr/plex", "pending ("+pendingShort+")")
			} else {
				plexHost, plexPort, err := hostPort(c.Plex.URL)
				if err != nil {
					return r.ensure(res, "maintainerr/plex", apps.Unchanged, err)
				}
				// Maintainerr masks the token, so the fingerprint of the one last
				// written and proven makes a rotation visible (owned.json).
				change, err := me.EnsurePlexConnection(ctx, apps.PlexConnection{
					Host:             plexHost,
					Port:             plexPort,
					Token:            plexToken,
					KnownFingerprint: owned.MaintainerrPlexTokenFingerprint(),
				})
				if err := r.ensure(res, "maintainerr/plex", change, err); err != nil {
					return err
				}
				// Only after the write and its proof succeeded, or a rotation would be
				// masked.
				owned.SetMaintainerrPlexTokenFingerprint(apps.PlexTokenFingerprint(plexToken))
			}
		}
	}

	if c.Seerr != nil {
		seerr := apps.NewSeerr(c.Seerr.URL)
		r.logf("waiting for Seerr at %s", c.Seerr.URL)
		if err := seerr.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "seerr/ready", apps.Unchanged, err)
		}
		r.okStep(res, "seerr/ready", c.Seerr.URL)
		public, err := seerr.Settings(ctx)
		if err != nil {
			return r.ensure(res, "seerr/settings", apps.Unchanged, err)
		}
		// Every settings endpoint needs an authenticated admin, and the only way to
		// create Seerr's first admin is an interactive Plex sign-in through a browser:
		// a claim token authenticates the server to plex.tv, not an account into
		// Seerr. So the only answer this run can give is whether an admin exists; a
		// migrated Seerr brings its admin in its snapshot, and that box gets no row.
		if !public.Initialized {
			res.Steps = append(res.Steps, Step{Name: "seerr/plex", Result: stepPending,
				Detail: fmt.Sprintf("Seerr has no admin account yet: sign in once at %s with Plex to let the wiring Job take over", c.Seerr.PublicURL)})
			r.stepLog("seerr/plex", "pending (needs one interactive Plex sign-in)")
		}
	}

	if r.Publish != nil {
		r.okStep(res, "publish", c.ConfigMap)
	}
	return nil
}

// publish writes status.json and the summary into the wiring ConfigMap, surviving an
// expired context: a failed run is when the receipt matters.
func (r *Runner) publish(ctx context.Context, res *Result) error {
	data, err := Render(res)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return r.Publish(pctx, data)
}

// Render builds the ConfigMap payload. owned.json is written with status.json and
// summary.txt in the same Upsert, including on a failed run.
func Render(res *Result) (map[string]string, error) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	out := map[string]string{
		"status.json": string(b),
		"summary.txt": Summary(res),
	}
	if res.OwnedRecord != nil {
		ob, err := json.MarshalIndent(res.OwnedRecord, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode owned record: %w", err)
		}
		out["owned.json"] = string(ob)
	}
	return out, nil
}

// Summary renders the receipt as lines a human can read with kubectl.
func Summary(res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "s5n wiring: %s at %s (%.1fs)\n", res.Status, res.FinishedAt.UTC().Format(time.RFC3339), res.DurationSeconds)
	fmt.Fprintf(&b, "writes: %d created, %d updated, %d unchanged\n", res.Writes.Created, res.Writes.Updated, res.Writes.Unchanged)
	for _, s := range res.Steps {
		fmt.Fprintf(&b, "  %-38s %-7s %s\n", s.Name, s.Result, s.Detail)
		for _, d := range s.Drift {
			fmt.Fprintf(&b, "      %s: %s -> %s\n", d.Field, d.Old, d.New)
		}
	}
	if res.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", res.Error)
	}
	return b.String()
}

// plexTokenPending says a Plex token cannot be used because the server is not
// signed in to plex.tv at all: it refuses every token until claimed, so a 401 there
// is the claim's absence.
//
// A claimed server that rejects the token is a credential error, and the caller
// stops the run.
func plexTokenPending(ident *apps.Identity, err error) (string, bool) {
	var rejected *apps.TokenRejectedError
	if ident == nil || ident.Claimed || !errors.As(err, &rejected) {
		return "", false
	}
	return "Plex is not claimed yet, so it refuses every server token: claim it at https://plex.tv/claim, then run box apply", true
}

// plexTokenMissing is the receipt's reason for a Plex-dependent row with no token,
// and it says what the person's part is: box supplies the token on every apply, so a
// claimed server only waits for the next run.
func plexTokenMissing(ident *apps.Identity, app string) string {
	if ident != nil && ident.Claimed {
		return "no plex-token in the release Secret: box reads it from Plex on the next apply"
	}
	return fmt.Sprintf("Plex is not claimed yet: claim it at https://plex.tv/claim, then apply again to connect %s", app)
}

// pbhReady names the one refusal PBH's readiness can produce: it answers 403 to
// everything when the Bearer token is not its WebUI token.
func pbhReady(err error) error {
	if !apps.IsForbidden(err) {
		return err
	}
	return fmt.Errorf("%w: PeerBanHelper refused the release Secret's pbh-token, so its WebUI token is something else", err)
}

// loginFailed turns an app's credential refusal into the sentence an operator can
// act on. Both apps take their password from the Secret only at pod start, so a
// password changed inside the running app is a pod that has not restarted. The Job
// names the cause and the command rather than restarting pods itself.
func loginFailed(err error, workload string) error {
	var rejected *apps.LoginRejectedError
	if !errors.As(err, &rejected) {
		return err
	}
	return fmt.Errorf("%s rejected the account %q: its password was changed inside the app since that pod started, so it no longer matches the release Secret's. Restart it to put the Secret's password back: kubectl rollout restart statefulset/%s",
		workload, rejected.Username, workload)
}

// hostPort splits a service URL into the host and port an Arr should dial.
func hostPort(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("parse url %q: %w", raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("url %q has no host", raw)
	}
	port := 80
	if u.Port() != "" {
		if port, err = strconv.Atoi(u.Port()); err != nil {
			return "", 0, fmt.Errorf("url %q has an invalid port: %w", raw, err)
		}
	} else if u.Scheme == "https" {
		port = 443
	}
	return host, port, nil
}
