package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/notice"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// healthRead is the timer's read: every app, a notice per readiness flip, and
// the new reading kept for the next wake.
type healthRead struct {
	// Read is one reading, keyed by app.
	Read func(context.Context) (map[string]update.AppHealth, error)
	// State is update.json; this reading replaces the last.
	State update.StateStore
	// Poster is where a flip is told; empty means nowhere.
	Poster notice.Poster
	// Options is the box and seedbox of every notice.
	Options notice.Options
	Out     say.Reporter
	// ConfigRoot holds snapshots: an app with a copy from the last step can be
	// rolled back.
	ConfigRoot string
	// Namespace is for the command a notice names.
	Namespace string
}

func (h healthRead) report() say.Reporter { return say.Or(h.Out, nil) }

// run reads, compares, posts and records; a failed check is one line, never a
// failed wake.
func (h healthRead) run(ctx context.Context) {
	now, err := h.Read(ctx)
	if err != nil {
		h.report().Warn(fmt.Sprintf("not checking on the apps: %v", err))
		return
	}
	state, err := h.State.Load()
	if err != nil {
		h.report().Warn(fmt.Sprintf("not checking on the apps: %v", err))
		return
	}
	before := readings(state.Health)
	for _, app := range notice.Flips(before, readings(now)) {
		h.post(ctx, app, now[app], state)
	}
	// Keep the reading even when posting failed, so the next wake does not report
	// the flip again. LastCheck is untouched: this wake checked nothing.
	state.Health = now
	if err := h.State.Save(state); err != nil {
		h.report().Warn(fmt.Sprintf("keeping what this wake saw of the apps: %v", err))
	}
}

// post tells someone about one app's change.
func (h healthRead) post(ctx context.Context, app string, health update.AppHealth, state update.State) {
	command := ""
	if !health.Ready {
		command = h.command(app, state)
	}
	if err := h.Poster.Post(ctx, notice.Health(h.Options, app, health.Ready, health.Reason, command)); err != nil {
		h.report().Warn(err.Error())
	}
}

// command is the rollback when the app moved in the last step and that step's
// copy is newest, else the logs. There is no `box logs` yet, so logs are
// kubectl's; a second instance is its own StatefulSet (`radarr-4k`).
func (h healthRead) command(app string, state update.State) string {
	if h.rollbackable(app, state) {
		return "box rollback " + app
	}
	return fmt.Sprintf("kubectl -n %s logs statefulset/%s", h.Namespace, app)
}

// rollbackable: the app moved in the last step, was not put back, and that
// step's copy is newest.
func (h healthRead) rollbackable(app string, state update.State) bool {
	step := state.LastStep
	if step == nil || h.ConfigRoot == "" {
		return false
	}
	if !contains(step.Moved, app) || contains(step.RolledBack, app) {
		return false
	}
	path, _, err := snapshot.Newest(snapshot.Root(h.ConfigRoot), app)
	if err != nil {
		return false
	}
	return strings.HasPrefix(snapshot.StepName(path), step.From+"-"+step.To+"-")
}

// readings is update.json's map as the notice package compares it.
func readings(in map[string]update.AppHealth) map[string]notice.Reading {
	out := make(map[string]notice.Reading, len(in))
	for app, h := range in {
		out[app] = notice.Reading{Ready: h.Ready, Reason: h.Reason}
	}
	return out
}

// healthOnly is a not-due wake's read: no registry, no self-update. It resolves
// the cluster and files itself, because the chart half does not run.
func healthOnly(ctx context.Context, o *updateFlags, out say.Reporter, conf hostconf.Config) {
	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.record})
	if err != nil {
		out.Warn(fmt.Sprintf("not checking on the apps: %v", err))
		return
	}
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		out.Warn(fmt.Sprintf("not checking on the apps: %v", err))
		return
	}
	namespace, err := sb.Namespace()
	if err != nil {
		out.Warn(fmt.Sprintf("not checking on the apps: %v", err))
		return
	}
	configRoot, err := update.ConfigRoot(sb)
	if err != nil {
		out.Warn(fmt.Sprintf("not checking on the apps: %v", err))
		return
	}
	boxHealth(kc, namespace, configRoot, out, conf, o.statePath(), grantDomain(sb)).run(ctx)
}

// boxHealth is one box's read, record and notices.
func boxHealth(kc, namespace, configRoot string, out say.Reporter, conf hostconf.Config, statePath string, domain string) healthRead {
	return healthRead{
		Read: func(ctx context.Context) (map[string]update.AppHealth, error) {
			w, err := update.NewWorkloads(kc, namespace, release.ReleaseName)
			if err != nil {
				return nil, err
			}
			return w.Readiness(ctx)
		},
		State:      update.FileState{Path: statePath},
		Poster:     notice.Poster{URL: conf.NoticeURL},
		Options:    notice.Options{Box: domain, Seedbox: namespace},
		Out:        out,
		ConfigRoot: configRoot,
		Namespace:  namespace,
	}
}

// noticeFrom is update's report of an event as the body box posts.
func noticeFrom(o notice.Options, n update.Notice) notice.Notice {
	apps := make([]notice.App, 0, len(n.Apps))
	for _, a := range n.Apps {
		apps = append(apps, notice.App{App: a.App, From: a.From, To: a.To})
	}
	switch n.Event {
	case update.EventRolledBack:
		if len(n.Apps) == 0 {
			return notice.RolledBack(o, n.Name, "", "", "")
		}
		a := n.Apps[0]
		return notice.RolledBack(o, n.Name, a.App, a.From, a.To)
	case update.EventStopped:
		return notice.Stopped(o, n.Name, n.From, n.To, n.Gate, n.Error, n.On)
	default:
		return notice.Updated(o, n.Name, n.From, n.To, apps)
	}
}

// grantDomain is gateway.host, the name a notice calls the box.
func grantDomain(sb *seedbox.Seedbox) string {
	gw, _ := sb.Grant.Map()["gateway"].(map[string]any)
	host, _ := gw["host"].(string)
	return strings.TrimSpace(host)
}

// contains is one app in a list box recorded.
func contains(list []string, app string) bool {
	for _, v := range list {
		if v == app {
			return true
		}
	}
	return false
}
