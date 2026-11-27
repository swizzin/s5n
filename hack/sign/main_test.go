package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/selfupdate"
)

// The point of these tests is the contract between the release and box: a
// signature this command makes is one internal/selfupdate verifies, for the
// version it was made for and no other, and only a seed whose public half is the
// committed key can make one.

// newKey generates a key into dir and returns its path, the seed it wrote and
// the public key the command printed.
func newKey(t *testing.T, dir string) (path, seed, public string) {
	t.Helper()
	return newKeyNamed(t, dir, "box-signing.key")
}

func newKeyNamed(t *testing.T, dir, name string) (path, seed, public string) {
	t.Helper()
	path = filepath.Join(dir, name)
	var out bytes.Buffer
	if err := keygen([]string{path}, &out); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the seed: %v", err)
	}
	return path, strings.TrimSpace(string(raw)), strings.TrimSpace(out.String())
}

// usePublicKey commits a public key where sign looks for one. The path is a
// variable, so a test need not run from the repository root.
func usePublicKey(t *testing.T, dir, public string) string {
	t.Helper()
	path := filepath.Join(dir, "signing.pub")
	if err := os.WriteFile(path, []byte(public+"\n"), 0o644); err != nil {
		t.Fatalf("writing the public key: %v", err)
	}
	previous := signingPubPath
	signingPubPath = path
	t.Cleanup(func() { signingPubPath = previous })
	return path
}

// writeSums writes a release's checksums and returns its path.
func writeSums(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "sha256sums.txt")
	content := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  box-linux-amd64\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing the checksums: %v", err)
	}
	return path
}

func TestKeygenWritesAPrivateSeedAndPrintsThePublicKey(t *testing.T) {
	dir := t.TempDir()
	path, seed, printed := newKey(t, dir)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the seed was written with mode %o, want 600", perm)
	}
	// What the command printed is the public half of what it wrote, so the two
	// halves the maintainer has to keep apart come from one key.
	private, err := selfupdate.ParseSeed(seed)
	if err != nil {
		t.Fatalf("the seed it wrote is not one it can read: %v", err)
	}
	if have := base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)); have != printed {
		t.Fatalf("keygen printed %s, but wrote the seed of %s", printed, have)
	}
	// The seed is a secret: it is in the file, and nowhere else.
	if strings.Contains(printed, seed) {
		t.Fatal("keygen printed the private seed")
	}
	if _, err := selfupdate.ParsePublicKey(printed); err != nil {
		t.Fatalf("what it printed is not a public key box can read: %v", err)
	}
}

func TestPublicPrintsWhatKeygenPrinted(t *testing.T) {
	// A seed kept somewhere safe is enough to write signing.pub again: public
	// prints exactly the line keygen printed for it, and never the seed.
	dir := t.TempDir()
	path, seed, printed := newKey(t, dir)
	var out bytes.Buffer
	if err := publicKey([]string{path}, &out); err != nil {
		t.Fatalf("public: %v", err)
	}
	if have := strings.TrimSpace(out.String()); have != printed {
		t.Fatalf("public printed %q, want keygen's %q", have, printed)
	}
	if strings.Contains(out.String(), seed) {
		t.Fatal("public printed the private seed")
	}
	bad := filepath.Join(dir, "not-a-seed")
	if err := os.WriteFile(bad, []byte("c2hvcnQ=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publicKey([]string{bad}, &bytes.Buffer{}); err == nil {
		t.Fatal("public accepted a file that is not a seed")
	}
}

func TestKeygenRefusesToOverwriteAKey(t *testing.T) {
	dir := t.TempDir()
	path, seed, _ := newKey(t, dir)

	err := keygen([]string{path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("keygen overwrote a signing key")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("keygen refused with %q, want the overwrite refusal", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("reading the seed: %v", readErr)
	}
	if strings.TrimSpace(string(raw)) != seed {
		t.Fatal("the key changed under a refused overwrite")
	}
}

func TestSignWritesASignatureBoxVerifies(t *testing.T) {
	dir := t.TempDir()
	_, seed, public := newKey(t, dir)
	keyFile := usePublicKey(t, dir, public)
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", seed)

	var out bytes.Buffer
	if err := sign([]string{"0.5.0", sums}, &out); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.Contains(out.String(), "signed sha256sums.txt for box-v0.5.0") {
		t.Fatalf("sign printed %q", out.String())
	}
	// The verifier is internal/selfupdate's: the same code box runs, against
	// the key, which is the whole point of the command living in this repo.
	if err := verify([]string{"0.5.0", sums, keyFile}, &bytes.Buffer{}); err != nil {
		t.Fatalf("verify on the signature sign made: %v", err)
	}

	// One changed byte in the checksums is a different release.
	content, err := os.ReadFile(sums)
	if err != nil {
		t.Fatalf("reading the checksums: %v", err)
	}
	if err := os.WriteFile(sums, []byte(strings.Replace(string(content), "0123", "0124", 1)), 0o644); err != nil {
		t.Fatalf("editing the checksums: %v", err)
	}
	if err := verify([]string{"0.5.0", sums, keyFile}, &bytes.Buffer{}); err == nil {
		t.Fatal("verify accepted checksums that changed after signing")
	}
}

func TestVerifyRefusesAReleaseTheSignatureIsNotFor(t *testing.T) {
	// The replay the version in the signed message stops: one release's
	// checksums, signed by the release key, checked as another's. Nothing in
	// the file itself tells the two apart.
	dir := t.TempDir()
	_, seed, public := newKey(t, dir)
	keyFile := usePublicKey(t, dir, public)
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", seed)
	if err := sign([]string{"0.5.0", sums}, &bytes.Buffer{}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	err := verify([]string{"0.4.0", sums, keyFile}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("verify accepted a signature made for another release")
	}
	if !strings.Contains(err.Error(), "does not verify") || !strings.Contains(err.Error(), "box-v0.4.0") {
		t.Fatalf("verify refused with %q, want the signature's refusal naming box-v0.4.0", err)
	}
}

func TestSignRefusesAnEmptyKey(t *testing.T) {
	dir := t.TempDir()
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", "  ")

	err := sign([]string{"0.5.0", sums}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("sign published without a key")
	}
	if !strings.Contains(err.Error(), "BOX_SIGNING_KEY is empty") {
		t.Fatalf("sign refused with %q, want the empty-key refusal", err)
	}
	if _, statErr := os.Stat(sums + ".sig"); statErr == nil {
		t.Fatal("sign wrote a signature without a key")
	}
}

func TestSignRefusesAKeyThatIsNotASeed(t *testing.T) {
	dir := t.TempDir()
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", base64.StdEncoding.EncodeToString([]byte("too short")))

	err := sign([]string{"0.5.0", sums}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("sign accepted a key that is not a seed")
	}
	if !strings.Contains(err.Error(), "BOX_SIGNING_KEY") || !strings.Contains(err.Error(), "want 32") {
		t.Fatalf("sign refused with %q, want the seed's shape named", err)
	}
}

func TestSignRefusesASeedThatIsNotTheCommittedKeys(t *testing.T) {
	// A release signed with a seed whose public half is not signing.pub
	// produces boxes that can never verify another release: the key compiled
	// into them is not the one anything is signed with. The refusal has to
	// name both, because either one can be the mistake.
	dir := t.TempDir()
	_, seed, _ := newKey(t, dir)
	_, _, committed := newKeyNamed(t, dir, "other.key")
	keyFile := usePublicKey(t, dir, committed)
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", seed)

	err := sign([]string{"0.5.0", sums}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("sign published with a seed that is not the committed key's")
	}
	derived, parseErr := selfupdate.ParseSeed(seed)
	if parseErr != nil {
		t.Fatalf("reading the seed it was given: %v", parseErr)
	}
	derivedText := base64.StdEncoding.EncodeToString(derived.Public().(ed25519.PublicKey))
	for _, want := range []string{derivedText, committed, keyFile} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("sign refused with %q, want it to name %q", err, want)
		}
	}
	if _, statErr := os.Stat(sums + ".sig"); statErr == nil {
		t.Fatal("sign wrote a signature its boxes could never verify")
	}
}

func TestSignRefusesAnUncommittedPublicKey(t *testing.T) {
	// signing.pub is committed empty for the maintainer to fill in, and signing
	// while it is empty would publish a release nothing can verify. That is its
	// own sentence: it is a missing step, not a wrong key.
	dir := t.TempDir()
	_, seed, _ := newKey(t, dir)
	keyFile := filepath.Join(dir, "signing.pub")
	if err := os.WriteFile(keyFile, nil, 0o644); err != nil {
		t.Fatalf("writing the empty public key: %v", err)
	}
	previous := signingPubPath
	signingPubPath = keyFile
	t.Cleanup(func() { signingPubPath = previous })
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", seed)

	err := sign([]string{"0.5.0", sums}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("sign published with no committed public key")
	}
	if !strings.Contains(err.Error(), "commit the public key first") {
		t.Fatalf("sign refused with %q, want the commit-the-key-first refusal", err)
	}
	if _, statErr := os.Stat(sums + ".sig"); statErr == nil {
		t.Fatal("sign wrote a signature with no committed public key")
	}
}

// chartDigest is one chart release's manifest digest, as a registry reports it.
const chartDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSignChartWritesASignatureBoxVerifies(t *testing.T) {
	dir := t.TempDir()
	_, seed, public := newKey(t, dir)
	keyFile := usePublicKey(t, dir, public)
	t.Setenv("BOX_SIGNING_KEY", seed)

	var out bytes.Buffer
	if err := signChart([]string{"0.6.3", chartDigest, dir}, &out); err != nil {
		t.Fatalf("sign-chart: %v", err)
	}
	if !strings.Contains(out.String(), "signed chart.sig for s5n-v0.6.3") {
		t.Fatalf("sign-chart printed %q", out.String())
	}
	// The asset is where a chart release publishes it, and box's own verifier
	// accepts it: a signature this command makes is one box accepts.
	path := filepath.Join(dir, release.ChartSignatureAsset)
	signature, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	key, err := selfupdate.ParsePublicKey(public)
	if err != nil {
		t.Fatalf("reading the public key: %v", err)
	}
	if err := selfupdate.VerifyChart(key, "0.6.3", chartDigest, signature); err != nil {
		t.Fatalf("VerifyChart on the signature sign-chart made: %v", err)
	}
	// The chart a signature is not for, and another chart's digest: both refused.
	if err := selfupdate.VerifyChart(key, "0.6.4", chartDigest, signature); err == nil {
		t.Fatal("the signature verified as another release's")
	}
	other := strings.Replace(chartDigest, "0123", "0124", 1)
	if err := selfupdate.VerifyChart(key, "0.6.3", other, signature); err == nil {
		t.Fatal("the signature verified over another digest")
	}
	// verify-chart is the release's own check that the asset is one box accepts.
	if err := verifyChart([]string{"0.6.3", chartDigest, path, keyFile}, &bytes.Buffer{}); err != nil {
		t.Fatalf("verify-chart on the signature sign-chart made: %v", err)
	}
	if err := verifyChart([]string{"0.6.4", chartDigest, path, keyFile}, &bytes.Buffer{}); err == nil {
		t.Fatal("verify-chart accepted the release's signature as another's")
	}
}

func TestSignChartRefusesADigestThatIsNotOne(t *testing.T) {
	// A chart is named by its manifest digest, and box compares what it pulled
	// against it: a signature over a tag, a length or a typo signs nothing.
	dir := t.TempDir()
	_, seed, public := newKey(t, dir)
	usePublicKey(t, dir, public)
	t.Setenv("BOX_SIGNING_KEY", seed)

	err := signChart([]string{"0.6.3", "0.6.3", dir}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("sign-chart signed something that is not a digest")
	}
	if !strings.Contains(err.Error(), "sha256:") {
		t.Fatalf("sign-chart refused with %q, want the digest's shape named", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, release.ChartSignatureAsset)); statErr == nil {
		t.Fatal("sign-chart wrote a signature over a non-digest")
	}
}

func TestSignChartRefusesAnEmptyKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOX_SIGNING_KEY", " ")

	err := signChart([]string{"0.6.3", chartDigest, dir}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("sign-chart signed without a key")
	}
	if !strings.Contains(err.Error(), "BOX_SIGNING_KEY is empty") {
		t.Fatalf("sign-chart refused with %q, want the empty-key refusal", err)
	}
}

func TestVerifyRejectsAnotherKey(t *testing.T) {
	dir := t.TempDir()
	_, seed, public := newKey(t, dir)
	usePublicKey(t, dir, public)
	_, _, otherPublic := newKeyNamed(t, dir, "second.key")
	sums := writeSums(t, dir)
	t.Setenv("BOX_SIGNING_KEY", seed)
	if err := sign([]string{"0.5.0", sums}, &bytes.Buffer{}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	keyFile := filepath.Join(dir, "other.pub")
	if err := os.WriteFile(keyFile, []byte(otherPublic), 0o644); err != nil {
		t.Fatalf("writing the other public key: %v", err)
	}
	err := verify([]string{"0.5.0", sums, keyFile}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("verify accepted a signature from another key")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("verify refused with %q, want the signature's refusal", err)
	}
}
