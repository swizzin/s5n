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

// Step is one line of the receipt: what was checked and what it cost.
type Step struct {
	Name   string `json:"name"`
	Result string `json:"result"` // ok | skipped | failed
	Detail string `json:"detail,omitempty"`
}

// Result is the whole receipt, written to the wiring ConfigMap so that `box
// status` and a human with kubectl can both read it.
type Result struct {
	Status          string          `json:"status"` // ok | failed
	StartedAt       time.Time       `json:"startedAt"`
	FinishedAt      time.Time       `json:"finishedAt"`
	DurationSeconds float64         `json:"durationSeconds"`
	Writes          Writes          `json:"writes"`
	Steps           []Step          `json:"steps"`
	Hardlinks       []HardlinkCheck `json:"hardlinks,omitempty"`
	Error           string          `json:"error,omitempty"`
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
			change, err := arr.EnsureDownloadClient(ctx, apps.DownloadClientSpec{
				Name:     "qBittorrent",
				Category: inst.Category,
				Host:     host,
				Port:     port,
				Username: c.Qbittorrent.Username,
				Password: qbitPassword,
			})
			if err := r.ensure(res, inst.Name+"/downloadclient", change, err); err != nil {
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
			impl := "Sonarr"
			if inst.Kind == "radarr" {
				impl = "Radarr"
			}
			change, err := p.EnsureApplication(ctx, apps.ApplicationSpec{
				Name:           inst.Name,
				Implementation: impl,
				BaseURL:        strings.TrimRight(inst.URL, "/") + inst.URLBase,
				APIKey:         arrKeys[inst.Name],
			})
			if err := r.ensure(res, "prowlarr/application/"+inst.Name, change, err); err != nil {
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

// Render builds the ConfigMap payload for a receipt.
func Render(res *Result) (map[string]string, error) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return map[string]string{
		"status.json": string(b),
		"summary.txt": Summary(res),
	}, nil
}

// Summary renders the receipt as lines a human can read with kubectl.
func Summary(res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "s5n wiring: %s at %s (%.1fs)\n", res.Status, res.FinishedAt.UTC().Format(time.RFC3339), res.DurationSeconds)
	fmt.Fprintf(&b, "writes: %d created, %d updated, %d unchanged\n", res.Writes.Created, res.Writes.Updated, res.Writes.Unchanged)
	for _, s := range res.Steps {
		fmt.Fprintf(&b, "  %-38s %-7s %s\n", s.Name, s.Result, s.Detail)
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
