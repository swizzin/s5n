package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// chartDigest is one release's manifest digest, as a registry reports it.
const chartDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestChartSignatureNamesTheReleaseAndItsDigest(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	signature := SignChart(private, "0.6.3", chartDigest)
	if err := VerifyChart(public, "0.6.3", chartDigest, signature); err != nil {
		t.Fatalf("VerifyChart on a signature SignChart made: %v", err)
	}
	// The message is the chart's line, then the version and the digest, one per
	// line: this is what a signature over a box release must never read as.
	if want := "s5n chart s5n-v0.6.3\n" + chartDigest + "\n"; string(ChartMessage("0.6.3", chartDigest)) != want {
		t.Fatalf("the signed message is %q, want %q", ChartMessage("0.6.3", chartDigest), want)
	}
	// One changed byte of the digest is a different chart.
	other := strings.Replace(chartDigest, "0123", "0124", 1)
	if err := VerifyChart(public, "0.6.3", other, signature); err == nil {
		t.Fatal("VerifyChart accepted a signature over a different digest")
	}
	// Two releases can carry the same digest; only the version separates them.
	for _, version := range []string{"0.6.2", "0.6.4", "0.6.3+rebuilt", ""} {
		if err := VerifyChart(public, version, chartDigest, signature); err == nil {
			t.Fatalf("VerifyChart accepted this release's signature as %q's", version)
		}
	}
	// A box release's signature over the same digest is another release's.
	if err := VerifyChart(public, "0.6.3", chartDigest, SignSums(private, "0.6.3", []byte(chartDigest+"\n"))); err == nil {
		t.Fatal("VerifyChart accepted a box release's signature")
	}
	// The asset is one line, the form the release publishes.
	if strings.Count(strings.TrimSuffix(string(signature), "\n"), "\n") != 0 {
		t.Fatalf("the signature asset is more than one line: %q", signature)
	}
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature))); err != nil || len(raw) != ed25519.SignatureSize {
		t.Fatalf("the signature asset decodes to %d bytes (%v), want %d", len(raw), err, ed25519.SignatureSize)
	}
}

func TestAKeylessBoxRefusesEveryChart(t *testing.T) {
	// signing.pub is what box is built with, so a build without one has no key to
	// check a chart against: the refusal is its own sentence, never a skip.
	previous := signingKey
	signingKey = ""
	t.Cleanup(func() { signingKey = previous })
	err := VerifyChartSignature("0.6.3", chartDigest, []byte("c2ln"))
	if err == nil {
		t.Fatal("a box with no signing key verified a chart")
	}
	if !strings.Contains(err.Error(), "built without a signing key") {
		t.Fatalf("error = %q, want the missing-key sentence", err)
	}
}

func TestVerifyChartRefusesASignatureItCannotRead(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	signature := SignChart(private, "0.6.3", chartDigest)
	for name, bad := range map[string][]byte{
		"not base64":  []byte("not a signature\n"),
		"too short":   []byte(base64.StdEncoding.EncodeToString([]byte("short")) + "\n"),
		"another key": SignChart(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), "0.6.3", chartDigest),
	} {
		if err := VerifyChart(public, "0.6.3", chartDigest, bad); err == nil {
			t.Fatalf("VerifyChart accepted a signature that is %s", name)
		}
	}
	// A proxy's CR does not change the signature.
	if err := VerifyChart(public, "0.6.3", chartDigest, []byte(strings.TrimSpace(string(signature))+"\r\n")); err != nil {
		t.Fatalf("VerifyChart on a CRLF signature: %v", err)
	}
	// A box built without a key refuses every chart, in its own words.
	err = VerifyChart(nil, "0.6.3", chartDigest, signature)
	if err == nil {
		t.Fatal("VerifyChart accepted a chart with no key")
	}
	if !strings.Contains(err.Error(), "built without a signing key") {
		t.Fatalf("VerifyChart with no key returned %q, want the missing-key refusal", err)
	}
}
