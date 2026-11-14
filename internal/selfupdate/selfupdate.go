package selfupdate

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/swizzin/s5n/internal/fetch"
	"github.com/swizzin/s5n/internal/release"
)

const (
	// ReexecEnv carries the version of the binary a re-executed update is now
	// running. Update refuses when it equals the running version: the release
	// page still naming something newer after box has just installed it is a
	// release's problem, and updating again would loop instead of fixing it.
	ReexecEnv = "S5N_BOX_REEXEC"

	// DefaultBaseURL is where box's releases are published. A release's assets
	// and its latest-release redirect both live under it; Options.BaseURL
	// replaces it so a test can serve a release of its own.
	DefaultBaseURL = "https://github.com/swizzin/s5n"

	// tagPrefix is the tag family box releases live in. releases/latest points
	// at one of these, because a box release is the only kind marked latest
	// (docs/box.md, "Where a release lives").
	tagPrefix = "box-v"

	// The three assets a box release carries for an update: the checksums, the
	// signature over them, and one binary per architecture.
	sumsAsset = "sha256sums.txt"
	sigAsset  = "sha256sums.txt.sig"

	// defaultMaxBinaryBytes caps the download. box's binary is tens of
	// megabytes; a body ten times that is not a box release, and reading it to
	// find out is how a disk fills. Options.MaxBinaryBytes makes the cap
	// something a test can reach without writing 256 MiB.
	defaultMaxBinaryBytes int64 = 256 << 20
	// defaultMaxMetadataBytes caps the checksums and the signature: two short
	// files, and 64 KiB is already far past either.
	defaultMaxMetadataBytes int64 = 64 << 10
)

// Options is what one check or update needs to know. The zero value is a
// release box on a host whose release page is GitHub's; a test fills in a
// BaseURL, a Version and an ExecutablePath.
type Options struct {
	// Version is the box version now running, as -X main.version set it. It is
	// read and never written; Update replaces the binary, not the version
	// compiled into it.
	Version string

	// BaseURL is the repository the releases live in. Empty means
	// DefaultBaseURL.
	BaseURL string

	// ExecutablePath is the binary to replace. Empty means box's own: whatever
	// os.Executable names, with symlinks resolved, so a binary installed
	// behind a link is replaced rather than the link. Tests set it.
	ExecutablePath string

	// Packaged names the package manager that installed box, when one did. A
	// packaged box never overwrites itself: the package manager owns the file,
	// and would undo the update on its next run. Naming it is the whole point
	// of the refusal, so the zero value means "not packaged".
	Packaged string

	// Hold is the operator's decision not to take this update. The release is
	// named and nothing else about it is fetched.
	Hold bool

	// HTTPClient is the client the fetches use. Empty means one whose transport
	// retries a transient failure (internal/fetch) — GitHub being briefly
	// unwell is not something an operator can fix. A test supplies its own.
	HTTPClient *http.Client

	// Geteuid reports the effective user id; nil means os.Geteuid. Update has
	// to run as root, so a test substitutes a uid rather than privileges.
	Geteuid func() int

	// MaxBinaryBytes and MaxMetadataBytes cap one download. Zero means
	// defaultMaxBinaryBytes and defaultMaxMetadataBytes.
	MaxBinaryBytes   int64
	MaxMetadataBytes int64
}

// Available is what Check found: the release the page names, and whether it is
// newer than this box.
type Available struct {
	// Current is the running version, as Options.Version gave it.
	Current string
	// Latest is the version the newest release carries.
	Latest string
	// Newer reports that Latest is greater than Current in semver order. An
	// older release (a tag published after a newer one) is named but never
	// installed.
	Newer bool
	// Held reports that the caller asked to hold the self-update.
	Held bool
}

// Result is what Update did.
type Result struct {
	// From is the version this run started on.
	From string
	// To is the version now in place. When Held is set it is instead the newer
	// release that was not taken, and nothing was written.
	To string
	// Path is the binary Update would have replaced, or did.
	Path string
	// Previous is the path the replaced binary was kept at, when Changed.
	Previous string
	// Changed reports that the binary on disk is now To.
	Changed bool
	// Held reports that a newer release was named and not taken.
	Held bool
}

// Check reports the newest box release and whether it is newer than this one.
// It writes nothing and downloads only the release page's redirect, so it is
// safe for a user's box: a user cannot replace the binary, and this is how
// their box learns root's next update has something to bring.
//
// It refuses a development build: without a version there is nothing to
// compare a release against. Hold is passed through in the answer; whether to
// take the release is Update's question.
func Check(ctx context.Context, o Options) (Available, error) {
	if err := checkVersion(o.Version); err != nil {
		return Available{}, err
	}
	return available(ctx, o)
}

// Update replaces box's binary with the newest release it can verify.
//
// It refuses in this order, each before anything is fetched or written: this
// build has no version to compare (a development build), this process is
// already the new version re-executed, this is not root, box came from a
// package manager. A box with no signing key compiled in refuses too, and never
// skips verification to get past it.
//
// A release that is not newer, or one the caller holds, is reported and not
// taken. Otherwise the checksums and their signature are fetched, the
// signature verified, the binary for this architecture downloaded and checked
// against the checksum, and the swap made: box.previous beside the binary, the
// new binary renamed over it, the directory fsynced. A failure at any point
// leaves the running binary in place and no temporary file behind.
//
// Re-executing into the new binary is the caller's step (Reexec): Update
// returns, and the update that asked for it carries on under the version it
// just installed.
func Update(ctx context.Context, o Options) (Result, error) {
	if err := checkVersion(o.Version); err != nil {
		return Result{}, err
	}
	if err := checkLoop(o); err != nil {
		return Result{}, err
	}
	if err := checkRoot(o); err != nil {
		return Result{}, err
	}
	if err := checkPackaged(o); err != nil {
		return Result{}, err
	}
	binary, err := executablePath(o)
	if err != nil {
		return Result{}, err
	}
	// The key is read before anything is fetched: a box built without one
	// refuses in the same words whether or not the network happens to be up.
	key, err := ParsePublicKey(signingKey)
	if err != nil {
		return Result{}, err
	}

	found, err := available(ctx, o)
	if err != nil {
		return Result{}, err
	}
	if !found.Newer {
		return Result{From: o.Version, To: o.Version, Path: binary}, nil
	}
	if found.Held {
		return Result{From: o.Version, To: found.Latest, Path: binary, Held: true}, nil
	}

	if err := install(ctx, o, found.Latest, binary, key); err != nil {
		return Result{}, fmt.Errorf("updating box %s to %s: %w", o.Version, found.Latest, err)
	}
	return Result{
		From:     o.Version,
		To:       found.Latest,
		Path:     binary,
		Previous: previousPath(binary),
		Changed:  true,
	}, nil
}

// available is the version question both entry points ask: what the newest
// release is, and whether it is newer than this box.
func available(ctx context.Context, o Options) (Available, error) {
	latest, err := latestVersion(ctx, o)
	if err != nil {
		return Available{}, err
	}
	newest, err := semver.NewVersion(latest)
	if err != nil {
		return Available{}, fmt.Errorf("the newest box release is %s, whose tag is not a version: %v", tagPrefix+latest, err)
	}
	own, err := semver.NewVersion(o.Version)
	if err != nil {
		return Available{}, fmt.Errorf("box's version %q is not a version: %v", o.Version, err)
	}
	return Available{
		Current: o.Version,
		Latest:  latest,
		Newer:   newest.GreaterThan(own),
		Held:    o.Hold,
	}, nil
}

// checkVersion refuses a build that cannot be compared with a release. A
// development build says so instead of pretending it is up to date: it is the
// version the release page would be compared against, and there isn't one.
func checkVersion(version string) error {
	if release.IsDevVersion(version) {
		return fmt.Errorf("this is a development build of box (version %q), so there is no released version to compare it with: self-update needs a release build, made with -X main.version", version)
	}
	if _, err := semver.NewVersion(version); err != nil {
		return fmt.Errorf("box's version %q is not a version, so it cannot be compared with a release: build box with -X main.version", version)
	}
	return nil
}

// checkLoop refuses the second update of one run. Update installs a binary and
// the caller re-execs it with the same arguments; if that process still found
// something newer than itself, updating again would replace the binary it is
// running and re-exec forever.
func checkLoop(o Options) error {
	if !Reexecuted(o.Version) {
		return nil
	}
	mark := strings.TrimSpace(os.Getenv(ReexecEnv))
	return fmt.Errorf("this process is already the box %s an update installed and re-executed (%s=%s), and the release page still names something newer: not updating again in the same run", mark, ReexecEnv, mark)
}

// Reexecuted reports whether this process is already the version an update
// installed and re-executed: the mark Reexec leaves in the environment. A caller
// asks before it offers to update at all, because "the update has already run in
// this run" is a fact about the run and not a failure to report.
//
// A development build never re-executes -- it has no version to compare a
// release with, so it has none to be marked with -- and an empty or "dev"
// version is never a re-execution.
func Reexecuted(version string) bool {
	if release.IsDevVersion(version) {
		return false
	}
	return strings.TrimSpace(os.Getenv(ReexecEnv)) == strings.TrimSpace(version)
}

// checkRoot refuses the replacement a user's box cannot make. The binary is
// root-owned, so this is not a policy box could relax: naming root's update is
// the only way forward, and it is the sentence a user reads.
func checkRoot(o Options) error {
	if euid(o) == 0 {
		return nil
	}
	return fmt.Errorf("box replaces its own binary as root, and this process is not root: root's next `box update` installs the new box")
}

func euid(o Options) int {
	if o.Geteuid != nil {
		return o.Geteuid()
	}
	return os.Geteuid()
}

// checkPackaged refuses to overwrite a binary the package manager owns, and
// names what does own it: an update here would be undone by the package's next
// upgrade, or would leave the package database describing a file that is not
// there.
func checkPackaged(o Options) error {
	manager := strings.TrimSpace(o.Packaged)
	if manager == "" {
		return nil
	}
	return fmt.Errorf("this box was installed by a package manager (%s), so the package owns the binary: upgrade it with %s, not with box", manager, manager)
}

// executablePath is the binary an update replaces. A packaging script may
// install it behind a symlink (or a maintainer may), and replacing the link
// would leave the file it points at running: the path resolved here is the
// file, and the rename has to land on it.
func executablePath(o Options) (string, error) {
	if o.ExecutablePath != "" {
		return o.ExecutablePath, nil
	}
	exe, err := ownExecutable()
	if err != nil {
		return "", fmt.Errorf("finding box's own binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolving box's own path %s: %w", exe, err)
	}
	return resolved, nil
}

// ownExecutable is os.Executable. It is a variable so a test can hand the
// resolver a path of its own, since the test binary is not box.
var ownExecutable = os.Executable

// ExecutablePath is the binary an update replaces: box's own, with symlinks
// resolved, unless Options.ExecutablePath names one.
//
// Rollback needs it -- it puts the previous binary back over the file that runs,
// and the link a packaging script installed is not that file -- so it is
// exported rather than resolved a second way by the caller.
func ExecutablePath(o Options) (string, error) {
	return executablePath(o)
}

// latestVersion reads the newest release's tag from the release page's
// redirect. GitHub answers /releases/latest with a 302 to the tag, so the tag
// is in a header box already has: there is no API call, no token and no rate
// limit, which matters when every host on a fleet checks on a timer.
func latestVersion(ctx context.Context, o Options) (string, error) {
	page := o.baseURL() + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return "", err
	}
	resp, err := redirectClient(o).Do(req)
	if err != nil {
		return "", err
	}
	discard(resp.Body)
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", fmt.Errorf("fetching %s: %s, want a redirect to the newest box release", page, resp.Status)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("%s redirected without a Location, so box cannot tell which release is newest", page)
	}
	// Only the tag matters, not the host: a test's server and GitHub both put
	// it in the path's last segment.
	target, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("%s redirected to %q, which is not a URL: %v", page, location, err)
	}
	tag := path.Base(strings.TrimSuffix(target.Path, "/"))
	version, ok := strings.CutPrefix(tag, tagPrefix)
	if !ok || version == "" || version == "." || version == "/" {
		return "", fmt.Errorf("the newest release is %s, not %s<version>: there is no box release to install", tag, tagPrefix)
	}
	return version, nil
}

// assetURL is where one asset of one release lives. The assets sit beside the
// tag rather than under the latest-release redirect, so a version box has
// already decided on is the version it downloads.
func (o Options) assetURL(version, name string) string {
	return fmt.Sprintf("%s/releases/download/%s%s/%s", o.baseURL(), tagPrefix, version, name)
}

func (o Options) baseURL() string {
	base := strings.TrimSuffix(o.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return base
}

// assetClient follows redirects: a release download is a 302 to storage, and
// the bytes box verifies are what comes back from there.
func assetClient(o Options) *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Transport: fetch.Transport{}}
}

// redirectClient does not follow: the tag box wants is in the redirect's
// Location, and following it would throw away the only copy of the answer.
func redirectClient(o Options) *http.Client {
	client := *assetClient(o)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

// binaryAsset is the release's binary for the platform box is running on. The
// releases carry linux binaries only, and an architecture or platform the
// checksums do not name is refused there, by name: the alternative is a build
// that installs a binary it cannot run over itself.
func binaryAsset() string {
	return "box-" + runtime.GOOS + "-" + runtime.GOARCH
}

// sumFor reads one file's sha256 from a release's checksums. The format is
// sha256sum's: the hex digest, whitespace, and the file name (a `*` marks a
// binary-mode line). A missing line is refused rather than skipped, because a
// release that does not name this architecture's binary has nothing box can
// install, and a duplicated one has no answer at all.
func sumFor(sums []byte, name string) (string, error) {
	var found string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest := strings.ToLower(fields[0])
		if raw, err := hex.DecodeString(digest); err != nil || len(raw) != 32 {
			return "", fmt.Errorf("%s: %q is not a sha256 digest", name, fields[0])
		}
		if found != "" {
			return "", fmt.Errorf("%s is listed twice in the release's checksums", name)
		}
		found = digest
	}
	if found == "" {
		return "", fmt.Errorf("the release has no %s: its %s has no line for it", name, sumsAsset)
	}
	return found, nil
}

// maxBinaryBytes and maxMetadataBytes are the caps Options left unset.
func (o Options) maxBinaryBytes() int64 {
	if o.MaxBinaryBytes > 0 {
		return o.MaxBinaryBytes
	}
	return defaultMaxBinaryBytes
}

func (o Options) maxMetadataBytes() int64 {
	if o.MaxMetadataBytes > 0 {
		return o.MaxMetadataBytes
	}
	return defaultMaxMetadataBytes
}
