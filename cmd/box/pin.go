package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/pin"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// pinCommand and unpinCommand are the two spellings of one decision.
const (
	pinCommand   = "pin"
	unpinCommand = "unpin"
)

// valuesAppsKey is the values key every app's pin lives under.
const valuesAppsKey = "apps"

// pinRequest is one `box pin` or `box unpin` over the two files.
type pinRequest struct {
	GrantPath  string
	ValuesPath string
	App        string
	// Ref is a version, a tag, a digest or tag@digest; empty for unpin.
	Ref   string
	Unpin bool
}

// pinOutcome is what the files say after the edit.
type pinOutcome struct {
	App string
	// Tag and Digest are the values' pin; both empty means the app follows the
	// chart again.
	Tag, Digest string
	// Changed says the values or the record were written.
	Changed bool
	// Frozen says a digest is part of the pin, so nothing follows.
	Frozen bool
}

// pinFiles writes a pin into the values and records what a tag resolves to. It
// asks no cluster: the apply that follows is `box apply`'s.
type pinFiles struct {
	out      say.Reporter
	resolver pin.Resolver
	now      func() time.Time
}

// decide resolves the ref, writes the pin, drops what it replaces and records the
// digest a tag resolved to. A ref the registry does not have writes nothing.
func (p *pinFiles) decide(ctx context.Context, req pinRequest, chart release.Chart) (pinOutcome, error) {
	at := time.Now
	if p.now != nil {
		at = p.now
	}
	sb, err := seedbox.Load(req.GrantPath, req.ValuesPath)
	if err != nil {
		return pinOutcome{}, err
	}
	root, err := update.ConfigRoot(sb)
	if err != nil {
		return pinOutcome{}, err
	}
	if req.Unpin {
		return p.unpin(sb, root, req.App)
	}
	image, err := variantImage(chart, sb, req.App)
	if err != nil {
		return pinOutcome{}, err
	}
	ref, err := pin.ParseRef(req.Ref)
	if err != nil {
		return pinOutcome{}, fmt.Errorf("%s: %w", req.App, err)
	}
	ref = pin.ForVariant(pin.ShapeOf(image.Tag), ref)
	digest, err := p.resolved(ctx, image.Repository, ref)
	if err != nil {
		return pinOutcome{}, fmt.Errorf("%s: %w", req.App, err)
	}
	out := pinOutcome{App: req.App, Tag: ref.Tag, Digest: ref.Digest, Changed: true, Frozen: ref.Digest != ""}
	var edits []seedbox.Edit
	var deletes []string
	if ref.Tag != "" {
		edits = append(edits, seedbox.Edit{Path: "apps." + req.App + ".image.tag", Value: ref.Tag})
	} else {
		deletes = append(deletes, "apps."+req.App+".image.tag")
	}
	if ref.Digest != "" {
		edits = append(edits, seedbox.Edit{Path: "apps." + req.App + ".image.digest", Value: ref.Digest})
	} else {
		deletes = append(deletes, "apps."+req.App+".image.digest")
	}
	if err := sb.EditValues(edits, deletes); err != nil {
		return pinOutcome{}, err
	}
	// A frozen pin needs no record, and one left behind would describe a tag
	// nothing follows; a tag pin records the digest it renders.
	records, err := snapshot.LoadPins(root)
	if err != nil {
		return pinOutcome{}, err
	}
	if ref.Digest == "" {
		records = snapshot.WithPin(records, snapshot.Pin{
			App:        req.App,
			Repository: image.Repository,
			Tag:        ref.Tag,
			Digest:     digest,
			Resolved:   at(),
			Chart:      chart.Version,
		})
	} else {
		records = snapshot.WithoutPin(records, req.App)
	}
	if err := snapshot.SavePins(root, records); err != nil {
		return pinOutcome{}, err
	}
	// The pin is the stronger claim, so a hold that would lapse under it goes.
	if err := dropHold(root, req.App); err != nil {
		return pinOutcome{}, err
	}
	return out, nil
}

// unpin removes both halves of an app's pin, the empty keys they leave, and the
// record a tag pin kept.
func (p *pinFiles) unpin(sb *seedbox.Seedbox, root, app string) (pinOutcome, error) {
	doc := sb.Values.Map()
	apps, _ := doc[valuesAppsKey].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	_, tag := image["tag"]
	_, digest := image["digest"]
	if !tag && !digest {
		return pinOutcome{App: app}, nil
	}
	deletes := unpinDeletes(doc, app, image, entry, apps, tag, digest)
	if deletes == nil {
		return pinOutcome{}, fmt.Errorf("apps.%s holds nothing but this pin, and box cannot leave the values file empty: remove it by hand", app)
	}
	if err := sb.EditValues(nil, deletes); err != nil {
		return pinOutcome{}, err
	}
	records, err := snapshot.LoadPins(root)
	if err != nil {
		return pinOutcome{}, err
	}
	if _, ok := snapshot.PinFor(records, app); ok {
		if err := snapshot.SavePins(root, snapshot.WithoutPin(records, app)); err != nil {
			return pinOutcome{}, err
		}
	}
	return pinOutcome{App: app, Changed: true}, nil
}

// unpinDeletes is the shortest key list that removes the pin: just its halves when
// the image names something else (a variant), the whole image when it does not, and
// the app when the pin was all it had.
func unpinDeletes(doc map[string]any, app string, image, entry, apps map[string]any, tag, digest bool) []string {
	tagPath, digestPath := "apps."+app+".image.tag", "apps."+app+".image.digest"
	var halves []string
	if tag {
		halves = append(halves, tagPath)
	}
	if digest {
		halves = append(halves, digestPath)
	}
	if len(image) > len(halves) {
		return halves
	}
	if len(entry) > 1 {
		return []string{"apps." + app + ".image"}
	}
	if len(apps) > 1 {
		return []string{"apps." + app}
	}
	if len(doc) > 1 {
		return []string{valuesAppsKey}
	}
	return nil
}

// variantImage is the image the app's selected variant names, which is the
// repository a pin lives in and the tag shape it is written in.
func variantImage(chart release.Chart, sb *seedbox.Seedbox, app string) (imageVariant, error) {
	images, err := admission.Images(chart.Variants)
	if err != nil {
		return imageVariant{}, err
	}
	byName, ok := images[app]
	if !ok {
		return imageVariant{}, fmt.Errorf("apps.%s.image: this chart ships no image for %s", app, app)
	}
	variant := valuesVariant(sb.Values.Map(), app)
	image, ok := byName[variant]
	if !ok {
		return imageVariant{}, fmt.Errorf("apps.%s.image.variant: %q is not a blessed variant of %s", app, variant, app)
	}
	return imageVariant{Repository: image.Repository, Tag: image.Tag}, nil
}

// imageVariant is one blessed variant's reference, split the way a pin needs it.
type imageVariant struct {
	Repository string
	Tag        string
}

// valuesVariant is the variant the values select; the chart's default is "default".
func valuesVariant(values map[string]any, app string) string {
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	variant, _ := image["variant"].(string)
	if strings.TrimSpace(variant) == "" {
		return "default"
	}
	return strings.TrimSpace(variant)
}

// dropHold removes one app's rollback hold after a pin has taken its place.
func dropHold(root, app string) error {
	holds, err := snapshot.LoadHolds(root)
	if err != nil {
		return err
	}
	kept := snapshot.WithoutHold(holds, app)
	if len(kept) == len(holds) {
		return nil
	}
	return snapshot.SaveHolds(root, kept)
}

// resolved proves the ref exists and answers the digest a tag names now. A digest
// is asked for too: a pin box cannot pull is a pod that never starts.
func (p *pinFiles) resolved(ctx context.Context, repository string, ref pin.Ref) (string, error) {
	if ref.Tag == "" {
		if _, err := p.resolver.Resolve(ctx, repository, ref.Digest); err != nil {
			return "", err
		}
		return ref.Digest, nil
	}
	digest, err := p.resolver.Resolve(ctx, repository, ref.Tag)
	if err != nil {
		return "", err
	}
	if ref.Digest != "" && digest != ref.Digest {
		return "", fmt.Errorf("%s:%s is %s now, not %s: pin the digest on its own to freeze those bytes", repository, ref.Tag, digest, ref.Digest)
	}
	return digest, nil
}

// pinFlags are apply's flags plus pin's own.
type pinFlags struct {
	*applyFlags
	plain  *bool
	record *string
}

func addPinFlags(fs *flag.FlagSet) *pinFlags {
	o := &pinFlags{
		applyFlags: addApplyFlags(fs),
		plain:      fs.Bool("plain", false, "ask with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)"),
		record:     fs.String("record", bootstrap.RecordPath, "bootstrap's cluster record"),
	}
	// The pin applies the release that is installed; naming another is update's.
	if f := fs.Lookup("chart-version"); f != nil {
		f.Usage = "not used: a pin applies the release that is installed"
	}
	return o
}

// pinArgs parses one command's arguments: the app and, for pin, the ref come
// first, and flags may follow, which is why the parse is restarted after each.
func pinArgs(fs *flag.FlagSet, command string, args []string, wantRef bool) ([]string, error) {
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
		more := ""
		if wantRef {
			more = " <ref>"
		}
		return nil, fmt.Errorf("box %s takes an app: box %s <app>%s", command, command, more)
	}
	positional := []string{rest[0]}
	if err := fs.Parse(rest[1:]); err != nil {
		return nil, err
	}
	if wantRef {
		if fs.NArg() < 1 {
			return nil, errors.New("box pin takes a ref: box pin <app> <version|tag|digest|tag@digest>")
		}
		positional = append(positional, fs.Arg(0))
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return nil, err
		}
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("box %s takes no more arguments: %s", command, strings.Join(fs.Args(), " "))
	}
	return positional, nil
}

// runPin writes one app's pin and then runs the same apply `box apply` runs.
func runPin(ctx context.Context, args []string) {
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("pin", flag.ExitOnError)
	o := addPinFlags(fs)
	positional, err := pinArgs(fs, pinCommand, args, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.pathsInNode = pathsInNode
	runPinOnce(ctx, o, positional[0], positional[1], false)
}

// runUnpin removes one app's pin and applies the same way.
func runUnpin(ctx context.Context, args []string) {
	pathsInNode, args, err := takePathsInNode(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("unpin", flag.ExitOnError)
	o := addPinFlags(fs)
	positional, err := pinArgs(fs, unpinCommand, args, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.pathsInNode = pathsInNode
	runPinOnce(ctx, o, positional[0], "", true)
}

// runPinOnce is both commands: decide over the files, say what changed, then
// apply.
func runPinOnce(ctx context.Context, o *pinFlags, app, ref string, unpin bool) {
	if *o.valuesPath == "" {
		fmt.Fprintln(os.Stderr, "box: no values file: pass --values")
		os.Exit(2)
	}
	if version := *o.chart.version; version != "" {
		fmt.Fprintf(os.Stderr, "box: pin applies the release that is installed, so --chart-version %s has nothing to do\n", version)
		os.Exit(2)
	}
	out := newOutput(terminalSet(*o.plain), *o.yes, startupTheme(*o.valuesPath))
	defer out.Close()
	if err := hostFiles(out, *o.record, *o.grantPath); err != nil {
		out.Error(err)
		os.Exit(1)
	}

	kc, err := release.ResolveKubeconfig(*o.kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: *o.record})
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	// The chart is the installed release's: its variants are the repositories and
	// tag shapes the pin is written against.
	charts := chartSource(out)
	chart, err := installedChart(ctx, charts, kc, o)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	p := &pinFiles{out: out, resolver: release.Registry{}, now: time.Now}
	got, err := p.decide(ctx, pinRequest{
		GrantPath: *o.grantPath, ValuesPath: *o.valuesPath, App: app, Ref: ref, Unpin: unpin,
	}, chart)
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
	sayPin(out, got)
	if !got.Changed {
		return
	}

	res, err := applyRoot(ctx, o.applyFlags, *o.record, setup.NetResolver{}, *o.yes, out, &chart)
	if err != nil {
		if errors.Is(err, release.ErrCancelled) {
			out.Rows([]say.Row{{
				Mark:    say.MarkNeeds,
				Facts:   []string{fmt.Sprintf("%s is written and not applied", app)},
				Command: "box apply",
			}})
			os.Exit(1)
		}
		out.Error(portRangeHint(err))
		os.Exit(1)
	}
	if res.Host != "" {
		out.Banner("seedbox ready", res.Host)
	}
	showJellyfinPassword(out, res)
}

// sayPin is the one row after the files changed: what the app is on now.
func sayPin(out say.Reporter, got pinOutcome) {
	switch {
	case !got.Changed && got.App != "":
		out.Rows([]say.Row{{Mark: say.MarkNote, Name: got.App, Facts: []string{"is not pinned; nothing changed"}}})
	case got.Frozen:
		out.Rows([]say.Row{{Mark: say.MarkDone, Name: got.App, Facts: []string{"frozen at " + shortDigest(got.Digest)}}})
	case got.Tag != "":
		out.Rows([]say.Row{{Mark: say.MarkDone, Name: got.App, Facts: []string{"follows " + got.Tag}}})
	default:
		out.Rows([]say.Row{{Mark: say.MarkDone, Name: got.App, Facts: []string{"follows the chart again"}}})
	}
}

// shortDigest is the first eight hex characters, the handle a person reads.
func shortDigest(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 8 {
		return "sha256:" + hex[:8] + "…"
	}
	return digest
}

// installedChart is the release the box is on, which names the variant shapes and
// the chart a pin records. A box with no release applies the newest.
func installedChart(ctx context.Context, charts release.ChartSource, kubeconfig string, o *pinFlags) (release.Chart, error) {
	sb, err := seedbox.Load(*o.grantPath, *o.valuesPath)
	if err != nil {
		return release.Chart{}, err
	}
	namespace, err := sb.Namespace()
	if err != nil {
		return release.Chart{}, err
	}
	state, err := release.NewHelm(kubeconfig, namespace, release.ReleaseName)
	if err != nil {
		return release.Chart{}, err
	}
	installed, err := state.State(ctx)
	if err != nil {
		return release.Chart{}, err
	}
	version := installed.ChartVersion
	if !installed.Exists {
		version = ""
	}
	return charts.Load(ctx, release.ChartRef{Repo: *o.chart.repo, Version: version})
}
