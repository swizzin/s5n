package release

import (
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
)

func TestTheApplyScreenIsARowPerThingThatHappened(t *testing.T) {
	f := newFakes(t)
	// Installed: one value moves, the diff is the review, one row each after.
	f.install(t)
	f.rel.chartVersion = "0.0.1"
	f.out.Reset()
	// One value changes: the diff is the review.
	f.writeValues(t, "wiring:\n  timeoutSeconds: 600\n")

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	lines := strings.Split(strings.TrimRight(f.out.String(), "\n"), "\n")
	want := []string{
		"apply",
		"! namespace testbox does not enforce Pod Security restricted",
		"s5n in testbox · 1 change",
		"~ wiring.timeoutSeconds: 300 -> 600",
	}
	if len(lines) < len(want) {
		t.Fatalf("screen is %d lines, want at least %d:\n%s", len(lines), len(want), f.out)
	}
	for i, line := range want {
		if !strings.HasPrefix(lines[i], line) {
			t.Fatalf("line %d = %q, want it to start %q:\n%s", i+1, lines[i], line, f.out)
		}
	}
	// The release is one row; the plumbing is not on screen.
	release := lineWith(lines, "✓ s5n ")
	if release == "" {
		t.Fatalf("the release has no row:\n%s", f.out)
	}
	if !strings.Contains(release, "2 apps") {
		t.Fatalf("the release row does not say what it runs: %q", release)
	}
	for _, gone := range []string{"applied:", "revision", "chart:"} {
		if strings.Contains(f.out.String(), gone) {
			t.Fatalf("the screen still carries %q:\n%s", gone, f.out)
		}
	}
}

func TestAnUnchangedApplyIsOneNote(t *testing.T) {
	f := newFakes(t)
	f.install(t)
	f.out.Reset()
	if _, err := f.apply(t); err != nil {
		t.Fatalf("second apply: %v\n%s", err, f.out)
	}
	want := "apply\n" +
		"! namespace testbox does not enforce Pod Security restricted (no pod-security.kubernetes.io/enforce label): root's labels win\n" +
		"· s5n            nothing to change · testbox\n"
	if got := f.out.String(); got != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", got, want)
	}
}

func TestARefusedApplyIsOneCrossRow(t *testing.T) {
	f := newFakes(t)
	f.writeValues(t, readFile(t, f.values)+"\nstorage:\n  data:\n    existingClaim: mine\n")

	_, err := f.apply(t)
	if err == nil {
		t.Fatalf("a values file carrying a grant key was applied:\n%s", f.out)
	}
	if f.openCalls != 0 {
		t.Fatalf("the refusal opened a cluster client %d times", f.openCalls)
	}
	var out strings.Builder
	(&say.Plain{W: &out, Err: &out}).Error(err)
	if !strings.HasPrefix(out.String(), "✗ ") {
		t.Fatalf("the refusal is not one ✗ row:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "storage.data.existingClaim") {
		t.Fatalf("the refusal does not name the key:\n%s", out.String())
	}
}

// install runs the first apply, so the next is a change against it.
func (f *fakes) install(t *testing.T) {
	t.Helper()
	if _, err := f.apply(t); err != nil {
		t.Fatalf("first apply: %v\n%s", err, f.out)
	}
}

// readFile is one fixture file.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := readFileBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// lineWith is the first line starting with prefix, or "".
func lineWith(lines []string, prefix string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// readFileBytes is os.ReadFile behind one name.
func readFileBytes(path string) ([]byte, error) { return os.ReadFile(path) }
