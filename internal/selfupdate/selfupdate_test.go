package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The tests serve a release page of their own, so a refusal is proved by what
// the server was asked for, not by what the code says it would ask.

// testKey puts a generated public key where the package reads its own.
func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a test key: %v", err)
	}
	previous := signingKey
	signingKey = base64.StdEncoding.EncodeToString(public)
	t.Cleanup(func() { signingKey = previous })
	return private
}

// fakeRelease is one box release a test serves.
type fakeRelease struct {
	version string
	binary  []byte
	sums    []byte
	// signature is sha256sums.txt.sig; empty means the test key's good one.
	signature []byte
	// tag overrides the latest redirect's tag; empty means this release's.
	tag string
	// asked is every path answered, in order: a refusal that fetches nothing
	// shows only the release page.
	asked []string
}

// releaseOf builds a release for this architecture, signed by seed.
func releaseOf(t *testing.T, seed ed25519.PrivateKey, version string) *fakeRelease {
	t.Helper()
	binary := []byte("#!/bin/sh\necho box " + version + "\n")
	sums := []byte(sumsLine(binary, binaryAsset()))
	return &fakeRelease{
		version:   version,
		binary:    binary,
		sums:      sums,
		signature: SignSums(seed, version, sums),
	}
}

// sumsLine is one line the way `sha256sum` writes it.
func sumsLine(data []byte, name string) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x  %s\n", sum, name)
}

func (f *fakeRelease) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.asked = append(f.asked, r.URL.Path)
		tag := f.tag
		if tag == "" {
			tag = tagPrefix + f.version
		}
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	assets := map[string][]byte{
		sumsAsset:     f.sums,
		sigAsset:      f.signature,
		binaryAsset(): f.binary,
	}
	prefix := "/releases/download/" + tagPrefix + f.version + "/"
	for name, body := range assets {
		mux.HandleFunc(prefix+name, func(w http.ResponseWriter, r *http.Request) {
			f.asked = append(f.asked, r.URL.Path)
			_, _ = w.Write(body)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// installDir makes a host-like directory holding a box binary.
func installDir(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "box")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing the running box: %v", err)
	}
	return path
}

// options is root's update against one test release.
func options(server *httptest.Server, path string) Options {
	return Options{
		Version:        "0.4.0",
		BaseURL:        server.URL,
		ExecutablePath: path,
		Geteuid:        func() int { return 0 },
	}
}

// assertDir pins the directory exactly: a leftover temporary file is a hazard.
func assertDir(t *testing.T, path string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("reading the install directory: %v", err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("install directory holds %v, want %v", got, want)
	}
}

func assertContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s holds %q, want %q", path, got, want)
	}
}

func TestUpdateInstallsTheSignedRelease(t *testing.T) {
	seed := testKey(t)
	release := releaseOf(t, seed, "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")

	result, err := Update(context.Background(), options(server, path))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := Result{From: "0.4.0", To: "0.5.0", Path: path, Previous: path + ".previous", Changed: true}
	if result != want {
		t.Fatalf("Update returned %+v, want %+v", result, want)
	}
	assertContent(t, path, release.binary)
	assertDir(t, path, "box", "box.previous")
	assertContent(t, path+".previous", []byte("old box"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("the new box has mode %o, want 755", perm)
	}
}

func TestUpdateRefusesABadSignature(t *testing.T) {
	testKey(t)             // the key box verifies with
	other := testKeyFor(t) // ... and a key the release was not signed with
	release := releaseOf(t, other, "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("Update accepted a release it did not sign")
	}
	if !strings.Contains(err.Error(), "does not verify against the signing key") {
		t.Fatalf("Update refused with %q, want the signature's refusal", err)
	}
	assertContent(t, path, []byte("old box"))
	assertDir(t, path, "box")
}

// testKeyFor generates a second key without touching the compiled-in one.
func testKeyFor(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a second test key: %v", err)
	}
	return private
}

func TestUpdateRefusesAnOlderReleasesSignature(t *testing.T) {
	// An older release's sums, signature and binary are all genuine and verify;
	// only the tag in the signed message refuses this downgrade.
	seed := testKey(t)
	older := releaseOf(t, seed, "0.4.1")
	release := releaseOf(t, seed, "0.5.0")
	release.binary = older.binary
	release.sums = older.sums
	release.signature = older.signature
	server := release.serve(t)
	path := installDir(t, "old box")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("Update installed an older release served under a newer tag")
	}
	if !strings.Contains(err.Error(), "does not verify against the signing key") ||
		!strings.Contains(err.Error(), tagPrefix+"0.5.0") {
		t.Fatalf("Update refused with %q, want the signature's refusal naming %s0.5.0", err, tagPrefix)
	}
	assertContent(t, path, []byte("old box"))
	assertDir(t, path, "box")
}

func TestUpdateRefusesABinaryItsChecksumsDontName(t *testing.T) {
	seed := testKey(t)
	release := releaseOf(t, seed, "0.5.0")
	// The signature is good, but the served binary is not what it names.
	wrong := []byte("some other binary")
	release.sums = []byte(sumsLine(wrong, binaryAsset()))
	release.signature = SignSums(seed, release.version, release.sums)
	server := release.serve(t)
	path := installDir(t, "old box")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("Update installed a binary whose digest does not match its checksums")
	}
	if !strings.Contains(err.Error(), "is not the binary its sha256sums.txt names") {
		t.Fatalf("Update refused with %q, want the digest's refusal", err)
	}
	assertContent(t, path, []byte("old box"))
	assertDir(t, path, "box")
}

func TestUpdateRefusesAChecksumsFileWithoutThisArchitecture(t *testing.T) {
	seed := testKey(t)
	release := releaseOf(t, seed, "0.5.0")
	other := "box-linux-otherarch"
	release.sums = []byte(sumsLine(release.binary, other))
	release.signature = SignSums(seed, release.version, release.sums)
	server := release.serve(t)
	path := installDir(t, "old box")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("Update accepted a release with no binary for this architecture")
	}
	if !strings.Contains(err.Error(), "has no "+binaryAsset()) {
		t.Fatalf("Update refused with %q, want the missing-architecture refusal naming %s", err, binaryAsset())
	}
	assertDir(t, path, "box")
}

func TestUpdateRefusesABoxWithNoSigningKey(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	previous := signingKey
	signingKey = ""
	t.Cleanup(func() { signingKey = previous })

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("a box with no signing key updated itself")
	}
	if !strings.Contains(err.Error(), "built without a signing key") ||
		!strings.Contains(err.Error(), "replace it by hand") {
		t.Fatalf("Update refused with %q, want the missing-key refusal", err)
	}
	// A box that cannot verify a release fetches none.
	if len(release.asked) != 0 {
		t.Fatalf("a box with no key asked for %v, want nothing", release.asked)
	}
	assertDir(t, path, "box")
}

func TestUpdateRefusesAnInvalidSigningKey(t *testing.T) {
	for name, broken := range map[string]string{
		"not base64":   "not base64 at all!",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("too short")),
	} {
		t.Run(name, func(t *testing.T) {
			release := releaseOf(t, testKey(t), "0.5.0")
			server := release.serve(t)
			path := installDir(t, "old box")
			previous := signingKey
			signingKey = broken
			t.Cleanup(func() { signingKey = previous })

			_, err := Update(context.Background(), options(server, path))
			if err == nil {
				t.Fatal("a box with a broken signing key updated itself")
			}
			if !strings.Contains(err.Error(), "replace it by hand") {
				t.Fatalf("Update refused with %q, want the replace-by-hand refusal", err)
			}
			if len(release.asked) != 0 {
				t.Fatalf("a box with a broken key asked for %v, want nothing", release.asked)
			}
		})
	}
}

func TestUpdateRefusesWithoutRoot(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	opts := options(server, path)
	opts.Geteuid = func() int { return 1000 }

	_, err := Update(context.Background(), opts)
	if err == nil {
		t.Fatal("a non-root update replaced the binary")
	}
	if !strings.Contains(err.Error(), "root's next `box update`") {
		t.Fatalf("Update refused with %q, want the refusal naming root's update", err)
	}
	if len(release.asked) != 0 {
		t.Fatalf("a non-root update asked for %v, want nothing", release.asked)
	}
	assertDir(t, path, "box")
}

func TestUpdateRefusesAPackagedBox(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	opts := options(server, path)
	opts.Packaged = "apt"

	_, err := Update(context.Background(), opts)
	if err == nil {
		t.Fatal("a packaged box replaced its own binary")
	}
	if !strings.Contains(err.Error(), "upgrade it with apt") {
		t.Fatalf("Update refused with %q, want the package manager's refusal", err)
	}
	if len(release.asked) != 0 {
		t.Fatalf("a packaged box asked for %v, want nothing", release.asked)
	}
	assertDir(t, path, "box")
}

func TestUpdateHoldsTheRelease(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	opts := options(server, path)
	opts.Hold = true

	result, err := Update(context.Background(), opts)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !result.Held || result.Changed || result.To != "0.5.0" || result.From != "0.4.0" {
		t.Fatalf("Update returned %+v, want the held release named and nothing changed", result)
	}
	// Holding asks the version question and fetches no release.
	if want := []string{"/releases/latest"}; !slices.Equal(release.asked, want) {
		t.Fatalf("a held update asked for %v, want %v", release.asked, want)
	}
	assertContent(t, path, []byte("old box"))
	assertDir(t, path, "box")
}

func TestUpdateDoesNothingWhenTheReleaseIsNotNewer(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.4.0")
	server := release.serve(t)
	path := installDir(t, "old box")

	result, err := Update(context.Background(), options(server, path))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Changed || result.From != "0.4.0" || result.To != "0.4.0" {
		t.Fatalf("Update returned %+v, want nothing to change", result)
	}
	if want := []string{"/releases/latest"}; !slices.Equal(release.asked, want) {
		t.Fatalf("an up-to-date box asked for %v, want %v", release.asked, want)
	}
	assertDir(t, path, "box")
}

func TestUpdateRefusesADevBuild(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	opts := options(server, path)
	opts.Version = ""

	_, err := Update(context.Background(), opts)
	if err == nil {
		t.Fatal("a development build updated itself")
	}
	if !strings.Contains(err.Error(), "development build") || !strings.Contains(err.Error(), "-X main.version") {
		t.Fatalf("Update refused with %q, want the development-build refusal", err)
	}
	if len(release.asked) != 0 {
		t.Fatalf("a development build asked for %v, want nothing", release.asked)
	}
}

func TestUpdateRefusesToUpdateTwiceInOneRun(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	t.Setenv(ReexecEnv, "0.4.0")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("a re-executed update updated itself again")
	}
	if !strings.Contains(err.Error(), "not updating again in the same run") {
		t.Fatalf("Update refused with %q, want the loop guard", err)
	}
	if len(release.asked) != 0 {
		t.Fatalf("a re-executed update asked for %v, want nothing", release.asked)
	}
}

func TestUpdateRefusesABinaryOverTheCap(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	opts := options(server, path)
	opts.MaxBinaryBytes = 8

	_, err := Update(context.Background(), opts)
	if err == nil {
		t.Fatal("Update accepted a binary larger than its cap")
	}
	if !strings.Contains(err.Error(), "is larger than 8 bytes") {
		t.Fatalf("Update refused with %q, want the size cap's refusal", err)
	}
	assertContent(t, path, []byte("old box"))
	assertDir(t, path, "box")
}

func TestUpdateRefusesChecksumsOverTheCap(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	path := installDir(t, "old box")
	opts := options(server, path)
	opts.MaxMetadataBytes = 8

	_, err := Update(context.Background(), opts)
	if err == nil {
		t.Fatal("Update accepted checksums larger than its cap")
	}
	if !strings.Contains(err.Error(), "is larger than 8 bytes") {
		t.Fatalf("Update refused with %q, want the size cap's refusal", err)
	}
	assertDir(t, path, "box")
}

func TestUpdateRefusesAnOlderRelease(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.3.0")
	server := release.serve(t)
	path := installDir(t, "old box")

	result, err := Update(context.Background(), options(server, path))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Changed || result.To != "0.4.0" {
		t.Fatalf("Update returned %+v, want an older release left alone", result)
	}
	assertContent(t, path, []byte("old box"))
}

func TestUpdateRefusesALatestReleaseFromTheOtherFamily(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	release.tag = "s5n-v0.5.0"
	server := release.serve(t)
	path := installDir(t, "old box")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("Update followed a latest release that is not a box release")
	}
	if !strings.Contains(err.Error(), "not box-v<version>") {
		t.Fatalf("Update refused with %q, want the wrong-family refusal", err)
	}
}

func TestUpdateReportsAMissingReleasePage(t *testing.T) {
	testKey(t) // a key, so the refusal that comes is the release page's
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	path := installDir(t, "old box")

	_, err := Update(context.Background(), options(server, path))
	if err == nil {
		t.Fatal("Update accepted a missing release page")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("Update refused with %q, want the status in the message", err)
	}
}

func TestCheckReportsTheNewestRelease(t *testing.T) {
	release := releaseOf(t, testKey(t), "0.5.0")
	server := release.serve(t)
	opts := Options{Version: "0.4.0", BaseURL: server.URL}

	// No euid and no executable: Check writes nothing.
	found, err := Check(context.Background(), opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := Available{Current: "0.4.0", Latest: "0.5.0", Newer: true}
	if found != want {
		t.Fatalf("Check returned %+v, want %+v", found, want)
	}
	if want := []string{"/releases/latest"}; !slices.Equal(release.asked, want) {
		t.Fatalf("Check asked for %v, want %v", release.asked, want)
	}
}

func TestCheckRefusesADevBuild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a development build asked for %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	if _, err := Check(context.Background(), Options{BaseURL: server.URL}); err == nil {
		t.Fatal("Check answered for a development build")
	}
}

func TestReexecutedReportsTheMark(t *testing.T) {
	// A caller asks before offering an update, so the mark must be read
	// correctly for versions that can carry one and for those that cannot.
	for _, tc := range []struct {
		version string
		mark    string
		want    bool
	}{
		{version: "0.5.0", mark: "0.5.0", want: true},
		{version: "0.5.0", mark: " 0.5.0\n", want: true},
		{version: "0.5.0", mark: "0.4.0"},
		{version: "0.5.0", mark: ""},
		// A development build has no version to be marked with.
		{version: "", mark: ""},
		{version: "dev", mark: "dev"},
	} {
		t.Run(tc.version+"|"+tc.mark, func(t *testing.T) {
			t.Setenv(ReexecEnv, tc.mark)
			if got := Reexecuted(tc.version); got != tc.want {
				t.Fatalf("Reexecuted(%q) with %s=%q = %v, want %v", tc.version, ReexecEnv, tc.mark, got, tc.want)
			}
		})
	}
}

func TestExecutablePathResolvesASymlink(t *testing.T) {
	// Replacing the link rather than the file it names leaves the old binary
	// running.
	real := installDir(t, "old box")
	link := filepath.Join(t.TempDir(), "box")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	previous := ownExecutable
	ownExecutable = func() (string, error) { return link, nil }
	t.Cleanup(func() { ownExecutable = previous })

	resolved, err := executablePath(Options{})
	if err != nil {
		t.Fatalf("executablePath: %v", err)
	}
	if resolved != real {
		t.Fatalf("executablePath returned %s, want the file it resolves to, %s", resolved, real)
	}
	// The override is used as given.
	if got, err := executablePath(Options{ExecutablePath: "/opt/box/box"}); err != nil || got != "/opt/box/box" {
		t.Fatalf("executablePath with an override returned %q, %v", got, err)
	}
}

func TestBinaryAssetNamesTheArchitecture(t *testing.T) {
	if got, want := binaryAsset(), "box-linux-"+runtime.GOARCH; got != want {
		t.Fatalf("binaryAsset is %s, want %s", got, want)
	}
}

func TestSumFor(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		sums string
		want string
		err  string
	}{
		{name: "the line sha256sum writes", sums: digest + "  box-linux-amd64\n", want: digest},
		{name: "a binary-mode line", sums: digest + " *box-linux-amd64\n", want: digest},
		{name: "an upper-case digest", sums: strings.ToUpper(digest) + "  box-linux-amd64\n", want: digest},
		{name: "another architecture's line", sums: digest + "  box-linux-arm64\n", err: "has no box-linux-amd64"},
		{name: "two lines for one file", sums: digest + "  box-linux-amd64\n" + digest + "  box-linux-amd64\n", err: "listed twice"},
		{name: "a digest that is not hex", sums: strings.Repeat("z", 64) + "  box-linux-amd64\n", err: "not a sha256 digest"},
		{name: "a short digest", sums: "abc  box-linux-amd64\n", err: "not a sha256 digest"},
		{name: "an empty file", sums: "", err: "has no box-linux-amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sumFor([]byte(tc.sums), "box-linux-amd64")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("sumFor returned %q, %v; want an error saying %q", got, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("sumFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("sumFor returned %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSignSumsRoundTripsThroughVerifySums(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	sums := []byte("0123  box-linux-amd64\n")
	signature := SignSums(private, "0.5.0", sums)
	if err := VerifySums(public, "0.5.0", sums, signature); err != nil {
		t.Fatalf("VerifySums on a signature SignSums made: %v", err)
	}
	// The message is the tag, then the checksums.
	if want := "s5n box release box-v0.5.0\n" + string(sums); string(signedMessage("0.5.0", sums)) != want {
		t.Fatalf("the signed message is %q, want %q", signedMessage("0.5.0", sums), want)
	}
	// The asset is one line.
	if lines := strings.Split(strings.TrimSuffix(string(signature), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("the signature asset is %d lines, want 1", len(lines))
	}
	if n, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature))); err != nil || len(n) != ed25519.SignatureSize {
		t.Fatalf("the signature asset decodes to %d bytes (%v), want %d", len(n), err, ed25519.SignatureSize)
	}
	// One byte of the checksums changing is a different release.
	tampered := []byte("0124  box-linux-amd64\n")
	if err := VerifySums(public, "0.5.0", tampered, signature); err == nil {
		t.Fatal("VerifySums accepted a signature over different checksums")
	}
	// Two releases can publish the same binaries; only the tag separates them.
	for _, other := range []string{"0.4.9", "0.5.1", "0.5.0+rebuilt", ""} {
		if err := VerifySums(public, other, sums, signature); err == nil {
			t.Fatalf("VerifySums accepted this release's signature as box-v%s's", other)
		}
	}
	if err := VerifySums(public, "0.5.0", sums, []byte("not base64\n")); err == nil {
		t.Fatal("VerifySums accepted a signature that is not base64")
	}
	if err := VerifySums(public, "0.5.0", sums, []byte(base64.StdEncoding.EncodeToString([]byte("short")))); err == nil {
		t.Fatal("VerifySums accepted a short signature")
	}
	if err := VerifySums(nil, "0.5.0", sums, signature); err == nil || !strings.Contains(err.Error(), "built without a signing key") {
		t.Fatalf("VerifySums with no key returned %v, want the missing-key refusal", err)
	}
	// A proxy's CR does not change the signature.
	if err := VerifySums(public, "0.5.0", sums, []byte(strings.TrimSpace(string(signature))+"\r\n")); err != nil {
		t.Fatalf("VerifySums on a CRLF signature: %v", err)
	}
}

func TestParsePublicKey(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	text := base64.StdEncoding.EncodeToString(public) + "\n"
	got, err := ParsePublicKey(text)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !got.Equal(public) {
		t.Fatal("ParsePublicKey returned another key")
	}
	// Both are 32 base64 bytes, so the pair proves the seed rebuilt the key.
	seed, err := ParseSeed(base64.StdEncoding.EncodeToString(private.Seed()))
	if err != nil {
		t.Fatalf("ParseSeed: %v", err)
	}
	if !seed.Public().(ed25519.PublicKey).Equal(public) {
		t.Fatal("ParseSeed did not rebuild the same key")
	}
	if _, err := ParsePublicKey(""); err == nil || !strings.Contains(err.Error(), "built without a signing key") {
		t.Fatalf("ParsePublicKey on an empty key returned %v, want the missing-key refusal", err)
	}
}
