package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/apps"
)

// Writes counts what the run actually changed. The e2e asserts that a second run
// reports zero created and zero updated.
type Writes struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
}

// Step is one line of the receipt: what was checked and what it cost. Drift
// lists owned fields that had been changed on the live object and were
// restored; a secret field is never represented in it (see apps.ownedField).
type Step struct {
	Name   string             `json:"name"`
	Result string             `json:"result"` // ok | skipped | failed
	Detail string             `json:"detail,omitempty"`
	Drift  []apps.FieldChange `json:"drift,omitempty"`
}

// Result is the whole receipt, written to the wiring ConfigMap so that `box
// status` and a human with kubectl can both read it. OwnedRecord is published
// separately as owned.json (see Render) rather than embedded in status.json,
// so it is excluded from this struct's own JSON.
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
	// HardlinkCheck is injectable so tests can avoid needing a real filesystem
	// proof; production uses the real thing.
	HardlinkCheck func(savePath, rootFolder string) *HardlinkCheck
	// ContractRetry bounds how long qBittorrent gets to settle on the data
	// contract, since its image can re-apply its own default after start-up.
	// Zero means 90 seconds.
	ContractRetry time.Duration
	// LoadOwned reads owned.json from the wiring ConfigMap, returning nil with
	// no error when the key or the ConfigMap is absent. It is injectable, like
	// Publish, so unit tests need no cluster; nil means ownership is never
	// recorded, so every run adopts by name.
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

// Run performs the wiring and always returns a receipt. A non-nil error means the
// release is not wired correctly; the receipt says where it stopped.
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
			r.logf("  %-38s failed: %v", "publish", perr)
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
		r.logf("  %-38s failed: %v", name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	switch change {
	case apps.Created:
		res.Writes.Created++
	case apps.Updated:
		res.Writes.Updated++
	case apps.Skipped:
		res.Steps = append(res.Steps, Step{Name: name, Result: "skipped"})
		r.logf("  %-38s skipped", name)
		return nil
	default:
		res.Writes.Unchanged++
	}
	res.Steps = append(res.Steps, Step{Name: name, Result: "ok", Detail: change.String()})
	r.logf("  %-38s %s", name, change)
	return nil
}

// ensureResult is ensure's counterpart for an owned object: it records drift
// (an owned field that was restored) and a recreated object's previous id in
// the step detail, on top of counting the write.
func (r *Runner) ensureResult(res *Result, name string, result apps.EnsureResult, err error) error {
	if err != nil {
		res.Steps = append(res.Steps, Step{Name: name, Result: "failed", Detail: err.Error()})
		r.logf("  %-38s failed: %v", name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	switch result.Change {
	case apps.Created:
		res.Writes.Created++
	case apps.Updated:
		res.Writes.Updated++
	case apps.Skipped:
		res.Steps = append(res.Steps, Step{Name: name, Result: "skipped"})
		r.logf("  %-38s skipped", name)
		return nil
	default:
		res.Writes.Unchanged++
	}
	detail := result.Change.String()
	if result.Recreated {
		detail = fmt.Sprintf("recreated (previous id %d)", result.PreviousID)
	}
	step := Step{Name: name, Result: "ok", Detail: detail, Drift: result.Drift}
	res.Steps = append(res.Steps, step)
	if len(result.Drift) > 0 {
		r.logf("  %-38s %s (%d field(s) restored)", name, detail, len(result.Drift))
	} else {
		r.logf("  %-38s %s", name, detail)
	}
	return nil
}

// loadOwned reads owned.json through LoadOwned. A record that is absent (a new
// release, or a ConfigMap `helm uninstall` removed) or unparsable falls back to
// an empty record, which makes every lookup below adopt by name; note says why,
// so the receipt can show it.
//
// A failed *read* is different and stops the run. Adopting by name after a
// transient API error would miss any owned object the user has renamed, create
// a second one beside it, and then overwrite owned.json with the duplicate's id,
// so a blip would turn into a permanent duplicate.
func (r *Runner) loadOwned(ctx context.Context) (rec *OwnedRecord, note string, err error) {
	if r.LoadOwned == nil {
		// Ownership tracking is not wired up at all (e.g. --no-publish): there is
		// no ConfigMap to read, so adopt by name without a receipt line.
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
	r.logf("  %-38s ok %s", name, detail)
}

func (r *Runner) work(ctx context.Context, res *Result) error {
	c := r.Config
	timeout := timeoutDuration(c.TimeoutSeconds)

	r.logf("s5n wiring: %d Arr instance(s), qBittorrent=%v, Prowlarr=%v",
		len(c.Instances), c.Qbittorrent != nil, c.Prowlarr != nil)

	// Ownership (AGENTS.md invariant 11): only an object whose id is recorded
	// here is "ours" to update, drift-correct or recreate. Everything else,
	// including an object merely named the same, is the user's.
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

	var qbit *apps.Qbit
	var qbitPassword string
	if c.Qbittorrent != nil {
		pw, err := ReadSecret(c.Qbittorrent.PasswordFile)
		if err != nil {
			return r.ensure(res, "qbittorrent/credentials", apps.Unchanged, err)
		}
		qbitPassword = pw
		qbit = apps.NewQbit(c.Qbittorrent.URL, c.Qbittorrent.Username, qbitPassword)
		r.logf("waiting for qBittorrent at %s", c.Qbittorrent.URL)
		if err := qbit.WaitReady(ctx, timeout); err != nil {
			return r.ensure(res, "qbittorrent/ready", apps.Unchanged, err)
		}
		version, _ := qbit.Version(ctx)
		r.okStep(res, "qbittorrent/ready", version)

		// qBittorrent's image re-applies its own save path at runtime, so the config
		// seed cannot be trusted alone: set it through the API, then assert.
		change := apps.Unchanged
		err = apps.Retry(ctx, r.contractRetry(), func(ctx context.Context) error {
			var e error
			change, e = qbit.EnsureDataContract(ctx)
			return e
		})
		if err := r.ensure(res, "qbittorrent/data-contract", change, err); err != nil {
			return err
		}

		seen := map[string]bool{}
		for _, inst := range c.Instances {
			if seen[inst.Category] {
				continue
			}
			seen[inst.Category] = true
			change, err := qbit.EnsureCategory(ctx, inst.Category, inst.SavePath)
			if err := r.ensure(res, "qbittorrent/category/"+inst.Category, change, err); err != nil {
				return err
			}
		}
	}

	// Every Arr gets the same qBittorrent, its own category, its root folder and
	// hardlink imports. The Arr API keys are collected for Prowlarr.
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
				Name:     "qBittorrent",
				Category: inst.Category,
				Host:     host,
				Port:     port,
				Username: c.Qbittorrent.Username,
				Password: qbitPassword,
			}, owned.ArrDownloadClientID(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetArrDownloadClientID(inst.Name, result.ID)
			}
			if err := r.ensureResult(res, inst.Name+"/downloadclient", result, err); err != nil {
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
			default:
				return r.ensure(res, "prowlarr/application/"+inst.Name, apps.Unchanged,
					fmt.Errorf("instance %q: unknown Arr kind %q has no Prowlarr application implementation", inst.Name, inst.Kind))
			}
			result, err := p.EnsureApplication(ctx, apps.ApplicationSpec{
				Name:           inst.Name,
				Implementation: impl,
				BaseURL:        strings.TrimRight(inst.URL, "/") + inst.URLBase,
				APIKey:         arrKeys[inst.Name],
			}, owned.ProwlarrApplicationID(inst.Name))
			if err == nil && result.ID != 0 {
				owned.SetProwlarrApplicationID(inst.Name, result.ID)
			}
			if err := r.ensureResult(res, "prowlarr/application/"+inst.Name, result, err); err != nil {
				return err
			}
		}
	}

	// Prove every distinct download/library pair, not just the first. One claim is
	// not one filesystem: a disk mounted on the host at, say, /data/media/movies-4k
	// sits inside the claim, passes a tv check, and turns every 4K import into a copy.
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

	// Plex, then the two apps that attach to it (PLAN.md's catalog order).
	// A missing or not-yet-successful claim token leaves Plex unclaimed —
	// /identity still answers, so that alone is not a failure — and the media
	// rows below report pending rather than failing the release. Tautulli needs
	// its own Plex server token for the PMS link (the release Secret's
	// plex-token), which is independent of the claim token: claim signs the
	// server in to Plex.tv, plex-token authorizes a client against the server.
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
			res.Steps = append(res.Steps, Step{Name: "plex/claim", Result: "pending",
				Detail: "no claim token (or it expired before Plex started): Plex is not signed in to a Plex.tv account"})
			r.logf("  %-38s pending: no claim token", "plex/claim")
		}
	}

	if c.Tautulli != nil {
		if plexIdentity == nil {
			res.Steps = append(res.Steps, Step{Name: "tautulli/plex", Result: "pending", Detail: "plex is not deployed in this release"})
		} else {
			key, err := ReadSecret(c.Tautulli.APIKeyFile)
			if err != nil {
				return r.ensure(res, "tautulli/credentials", apps.Unchanged, err)
			}
			// The PMS link is token-based. Without a Plex server token the only
			// alternative would be Plex trusting a network range, and the
			// Gateway's Envoy reaches Plex from a pod IP inside the pod CIDR —
			// that would expose the server unauthenticated through the Gateway.
			// No token therefore means pending, not a tokenless write.
			plexToken, haveToken, err := ReadOptionalSecret(c.Tautulli.PlexTokenFile)
			if err != nil {
				return r.ensure(res, "tautulli/plex", apps.Unchanged, err)
			}
			if haveToken {
				// Prove the token against Plex itself, and before touching
				// Tautulli at all. Tautulli's own get_server_identity reads
				// Plex's unauthenticated /identity, so a bogus token would
				// otherwise "prove" a link Plex refuses on every real request;
				// /library/sections answers 401 to a token Plex does not
				// accept. A rejected token is a release credential error like
				// any other: no Tautulli write, and the run stops.
				if err := plex.VerifyToken(ctx, plexToken); err != nil {
					return r.ensure(res, "tautulli/plex", apps.Unchanged, err)
				}
			}
			tt := apps.NewTautulli(c.Tautulli.URL, key)
			r.logf("waiting for Tautulli at %s", c.Tautulli.URL)
			if err := tt.WaitReady(ctx, timeout); err != nil {
				return r.ensure(res, "tautulli/ready", apps.Unchanged, err)
			}
			r.okStep(res, "tautulli/ready", c.Tautulli.URL)
			if !haveToken {
				const detail = "no Plex token: set plex-token (from a claimed server) to let the wiring Job connect Tautulli"
				res.Steps = append(res.Steps, Step{Name: "tautulli/plex", Result: "pending", Detail: detail})
				r.logf("  %-38s pending: no Plex token", "tautulli/plex")
			} else {
				plexHost, plexPort, err := hostPort(c.Plex.URL)
				if err != nil {
					return r.ensure(res, "tautulli/plex", apps.Unchanged, err)
				}
				// Tautulli withholds pms_token from get_settings, so the
				// fingerprint of the token last written and proven is what
				// makes a rotation in the Secret visible (owned.json).
				change, err := tt.EnsurePlexConnection(ctx, plexHost, plexPort,
					plexIdentity.MachineIdentifier, plexToken, owned.TautulliPlexTokenFingerprint())
				if err := r.ensure(res, "tautulli/plex", change, err); err != nil {
					return err
				}
				// Only after the write and its proof succeeded: a fingerprint
				// recorded for an unaccepted write would mask a rotation.
				owned.SetTautulliPlexTokenFingerprint(apps.PlexTokenFingerprint(plexToken))
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
		// Every settings endpoint in Seerr needs an authenticated admin
		// session, and the only way to create its first admin user is an
		// interactive Plex (or Jellyfin) OAuth sign-in through a browser — a
		// claim token does not help, because it authenticates the Plex
		// *server* to plex.tv, not a plex.tv *account* into Seerr. This is
		// always pending until a human signs in once, regardless of Plex's
		// claim state.
		res.Steps = append(res.Steps, Step{Name: "seerr/plex", Result: "pending",
			Detail: "Seerr has no admin account yet: sign in once at /seerr with Plex to let the wiring Job take over"})
		r.logf("  %-38s pending: needs one interactive Plex sign-in", "seerr/plex")
	}

	if r.Publish != nil {
		r.okStep(res, "publish", c.ConfigMap)
	}
	return nil
}

// publish writes status.json and a readable summary into the wiring ConfigMap. It
// survives an expired context: a failed run is exactly when the receipt matters.
func (r *Runner) publish(ctx context.Context, res *Result) error {
	data, err := Render(res)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return r.Publish(pctx, data)
}

// Render builds the ConfigMap payload for a receipt. owned.json is written
// alongside status.json and summary.txt in the same Upsert — including on a
// failed run, so the record reflects whatever ids were confirmed before the
// run stopped (AGENTS.md invariant 11).
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
