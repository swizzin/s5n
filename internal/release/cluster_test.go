package release

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
)

type fakeRecords struct {
	rec *bootstrap.Record
	err error
}

func (f fakeRecords) Load() (*bootstrap.Record, error) { return f.rec, f.err }
func (f fakeRecords) RecordPath() string               { return "/etc/swizzin/cluster.yaml" }
func (f fakeRecords) Save(*bootstrap.Record) error     { return nil }

func tempFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveKubeconfigPrefersTheFlag(t *testing.T) {
	flag := tempFile(t)
	got, err := ResolveKubeconfig(flag, "/does/not/matter", fakeRecords{})
	if err != nil || got != flag {
		t.Fatalf("ResolveKubeconfig = %q, %v; want the flag %q", got, err, flag)
	}
}

func TestResolveKubeconfigExplicitPathNeverFallsBack(t *testing.T) {
	recorded := tempFile(t)
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := ResolveKubeconfig(missing, "", fakeRecords{rec: &bootstrap.Record{Kubeconfig: recorded}})
	if err == nil || !strings.Contains(err.Error(), "explicit kubeconfig") {
		t.Fatalf("error = %v, want an explicit-path refusal instead of a fallback", err)
	}
}

func TestResolveKubeconfigUsesTheFirstEnvEntry(t *testing.T) {
	first := tempFile(t)
	second := tempFile(t)
	got, err := ResolveKubeconfig("", first+string(os.PathListSeparator)+second, fakeRecords{})
	if err != nil || got != first {
		t.Fatalf("ResolveKubeconfig = %q, %v; want the first KUBECONFIG entry %q", got, err, first)
	}
}

func TestResolveKubeconfigUsesBootstrapRecord(t *testing.T) {
	recorded := tempFile(t)
	got, err := ResolveKubeconfig("", "", fakeRecords{rec: &bootstrap.Record{
		Source:     bootstrap.SourceExternal,
		Kubeconfig: recorded,
	}})
	if err != nil || got != recorded {
		t.Fatalf("ResolveKubeconfig = %q, %v; want the recorded %q", got, err, recorded)
	}
}

func TestResolveKubeconfigWithNoRecordSaysBootstrap(t *testing.T) {
	_, err := ResolveKubeconfig("", "", fakeRecords{})
	if err == nil || !strings.Contains(err.Error(), "box bootstrap") {
		t.Fatalf("error = %v, want a run-box-bootstrap refusal", err)
	}
}

func TestResolveKubeconfigReadFailureIsNotAnAbsentRecord(t *testing.T) {
	_, err := ResolveKubeconfig("", "", fakeRecords{err: errors.New("corrupt")})
	if err == nil || !strings.Contains(err.Error(), "cluster record") {
		t.Fatalf("error = %v, want a record-read failure", err)
	}
}

func TestResolveKubeconfigRecordedPathMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := ResolveKubeconfig("", "", fakeRecords{rec: &bootstrap.Record{Kubeconfig: missing}})
	if err == nil || !strings.Contains(err.Error(), "recorded kubeconfig") {
		t.Fatalf("error = %v, want a missing-recorded-kubeconfig refusal", err)
	}
}
