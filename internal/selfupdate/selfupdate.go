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
	// ReexecEnv carries the version a re-executed update now runs. Update
	// refuses when it equals the running version: updating again would loop.
	ReexecEnv = "S5N_BOX_REEXEC"

	// DefaultBaseURL is where box's releases are published; tests replace it.
	DefaultBaseURL = "https://github.com/swizzin/s5n"

	// tagPrefix is the tag family box releases live in; only one is latest
	// (docs/box.md, "Where a release lives").
	tagPrefix = "box-v"

	// sumsAsset, sigAsset and one binary per architecture are the release.
	sumsAsset = "sha256sums.txt"
	sigAsset  = "sha256sums.txt.sig"

	// defaultMaxBinaryBytes caps the download: a body ten times a box binary
	// is not a release, and reading it to find out fills a disk.
	defaultMaxBinaryBytes int64 = 256 << 20
	// defaultMaxMetadataBytes caps the checksums and the signature.
	defaultMaxMetadataBytes int64 = 64 << 10
)

// Options is what one check or update needs to know.
type Options struct {
	// Version is the box version now running, as -X main.version set it.
	Version string

	// BaseURL is the repository the releases live in; empty is DefaultBaseURL.
	BaseURL string

	// ExecutablePath is the binary to replace; empty means box's own with
	// symlinks resolved, so an installed link is not replaced.
	ExecutablePath string

	// Packaged names the package manager that installed box. A packaged box
	// never overwrites itself: the manager owns the file and would undo it.
	Packaged string

	// Hold is the operator's decision not to take this update.
	Hold bool

	// HTTPClient is the client the fetches use; empty retries transient
	// failures (internal/fetch). A test supplies its own.
	HTTPClient *http.Client

	// Geteuid reports the effective user id; nil means os.Geteuid.
	Geteuid func() int

	// MaxBinaryBytes and MaxMetadataBytes cap one download; zero means the
	// defaults.
	MaxBinaryBytes   int64
	MaxMetadataBytes int64
}

// Available is what Check found: the newest release, and whether it is newer.
type Available struct {
	// Current is the running version.
	Current string
	// Latest is the version the newest release carries.
	Latest string
	// Newer reports that Latest is greater than Current in semver order; an
	// older release is named but never installed.
	Newer bool
	// Held reports that the caller asked to hold the self-update.
	Held bool
}

// Result is what Update did.
type Result struct {
	// From is the version this run started on.
	From string
	// To is the version now in place, or the held release that was not taken.
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

// Check reports the newest box release and whether it is newer. It writes
// nothing and downloads only the release page's redirect, so a user's box may
// call it; it refuses a development build, which has no version to compare.
func Check(ctx context.Context, o Options) (Available, error) {
	if err := checkVersion(o.Version); err != nil {
		return Available{}, err
	}
	return available(ctx, o)
}

// Update replaces box's binary with the newest release it can verify. It
// refuses before fetching anything when this build has no version, this process
// is already a re-executed update, it is not root, or box came from a package;
// a box with no signing key refuses too and never skips verification.
//
// A release that is not newer, or is held, is reported and not taken. Otherwise
// the signature, the checksums and the binary are verified in that order and
// swapped in atomically; a failure leaves the running binary in place, and
// Reexec is the caller's next step.
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

// available answers what the newest release is, and whether it is newer.
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

// checkVersion refuses a build with no version to compare a release against.
func checkVersion(version string) error {
	if release.IsDevVersion(version) {
		return fmt.Errorf("this is a development build of box (version %q), so there is no released version to compare it with: self-update needs a release build, made with -X main.version", version)
	}
	if _, err := semver.NewVersion(version); err != nil {
		return fmt.Errorf("box's version %q is not a version, so it cannot be compared with a release: build box with -X main.version", version)
	}
	return nil
}

// checkLoop refuses the second update of one run: the re-executed process would
// otherwise replace the binary it is running and loop forever.
func checkLoop(o Options) error {
	if !Reexecuted(o.Version) {
		return nil
	}
	mark := strings.TrimSpace(os.Getenv(ReexecEnv))
	return fmt.Errorf("this process is already the box %s an update installed and re-executed (%s=%s), and the release page still names something newer: not updating again in the same run", mark, ReexecEnv, mark)
}

// Reexecuted reports whether this process is already the version an update
// installed and re-executed. A development build never re-executes.
func Reexecuted(version string) bool {
	if release.IsDevVersion(version) {
		return false
	}
	return strings.TrimSpace(os.Getenv(ReexecEnv)) == strings.TrimSpace(version)
}

// checkRoot refuses the replacement a user's box cannot make: the binary is
// root-owned.
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

// checkPackaged refuses a binary the package manager owns, whose next upgrade
// would undo the update.
func checkPackaged(o Options) error {
	manager := strings.TrimSpace(o.Packaged)
	if manager == "" {
		return nil
	}
	return fmt.Errorf("this box was installed by a package manager (%s), so the package owns the binary: upgrade it with %s, not with box", manager, manager)
}

// executablePath is the binary an update replaces. A symlink is resolved:
// replacing the link would leave the file it points at running.
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

// ownExecutable is os.Executable, a variable because the test binary is not box.
var ownExecutable = os.Executable

// ExecutablePath is the binary an update replaces. Rollback needs the resolved
// file, not an installed link.
func ExecutablePath(o Options) (string, error) {
	return executablePath(o)
}

// latestVersion reads the newest tag from the release page's redirect: no API
// call, token or rate limit, which matters when every host checks on a timer.
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
	// Only the tag matters, not the host: it is the path's last segment.
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

// assetURL is where one asset of a decided release lives, rather than under
// the latest-release redirect.
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

// assetClient follows redirects: a release download is a 302 to storage.
func assetClient(o Options) *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Transport: fetch.Transport{}}
}

// redirectClient does not follow: the wanted tag is in the Location header.
func redirectClient(o Options) *http.Client {
	client := *assetClient(o)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

// binaryAsset is the release's binary for this platform. An architecture the
// checksums do not name is refused there, never installed unseen.
func binaryAsset() string {
	return "box-" + runtime.GOOS + "-" + runtime.GOARCH
}

// sumFor reads one file's sha256 from sha256sum output (a `*` marks binary
// mode). A missing line is refused: there is nothing to install; a duplicate
// has no answer.
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
