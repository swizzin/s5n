package kube

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRecords is the cluster record as the resolver sees it: the kubeconfig an
// earlier `box bootstrap` wrote, or no record at all.
type fakeRecords struct {
	path     string
	recorded bool
	err      error
}

func (f fakeRecords) RecordedKubeconfig() (string, bool, error) { return f.path, f.recorded, f.err }

func tempKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveKubeconfigPrecedence is the rule in one table: the flag, then the
// first KUBECONFIG entry, then the recorded cluster -- and an explicit path is
// never silently replaced by a later source.
func TestResolveKubeconfigPrecedence(t *testing.T) {
	flag := tempKubeconfig(t)
	fromEnv := tempKubeconfig(t)
	recorded := tempKubeconfig(t)
	emptyFirst := string(os.PathListSeparator) + fromEnv

	for _, tc := range []struct {
		name  string
		flag  string
		env   string
		rec   fakeRecords
		want  string
		wantE string
	}{
		{
			name: "the flag beats the environment and the record",
			flag: flag,
			env:  fromEnv,
			rec:  fakeRecords{path: recorded, recorded: true},
			want: flag,
		},
		{
			name: "the environment beats the record",
			env:  fromEnv,
			rec:  fakeRecords{path: recorded, recorded: true},
			want: fromEnv,
		},
		{
			name: "the first non-empty environment entry wins",
			env:  emptyFirst,
			rec:  fakeRecords{path: recorded, recorded: true},
			want: fromEnv,
		},
		{
			name: "the record is the last resort",
			rec:  fakeRecords{path: recorded, recorded: true},
			want: recorded,
		},
		{
			name:  "nothing anywhere names a bootstrap run",
			wantE: "box bootstrap",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveKubeconfig(tc.flag, tc.env, tc.rec)
			if tc.wantE != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantE) {
					t.Fatalf("ResolveKubeconfig = %q, %v; want an error naming %q", got, err, tc.wantE)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ResolveKubeconfig = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestResolveKubeconfigPrefersTheFlag(t *testing.T) {
	flag := tempKubeconfig(t)
	got, err := ResolveKubeconfig(flag, "/does/not/matter", fakeRecords{})
	if err != nil || got != flag {
		t.Fatalf("ResolveKubeconfig = %q, %v; want the flag %q", got, err, flag)
	}
}

func TestResolveKubeconfigExplicitPathNeverFallsBack(t *testing.T) {
	recorded := tempKubeconfig(t)
	missing := filepath.Join(t.TempDir(), "gone")
	_, err := ResolveKubeconfig(missing, "", fakeRecords{path: recorded, recorded: true})
	if err == nil || !strings.Contains(err.Error(), "explicit kubeconfig") {
		t.Fatalf("error = %v, want an explicit-path refusal instead of a fallback", err)
	}
}

func TestResolveKubeconfigUsesTheFirstEnvEntry(t *testing.T) {
	first := tempKubeconfig(t)
	second := tempKubeconfig(t)
	got, err := ResolveKubeconfig("", first+string(os.PathListSeparator)+second, fakeRecords{})
	if err != nil || got != first {
		t.Fatalf("ResolveKubeconfig = %q, %v; want the first KUBECONFIG entry %q", got, err, first)
	}
}

func TestResolveKubeconfigUsesBootstrapRecord(t *testing.T) {
	recorded := tempKubeconfig(t)
	got, err := ResolveKubeconfig("", "", fakeRecords{path: recorded, recorded: true})
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
	_, err := ResolveKubeconfig("", "", fakeRecords{path: missing, recorded: true})
	if err == nil || !strings.Contains(err.Error(), "recorded kubeconfig") {
		t.Fatalf("error = %v, want a missing-recorded-kubeconfig refusal", err)
	}
}

// TestResolveKubeconfigRecordWithoutAPath is the case the old signature could
// not distinguish from "no record": a record that exists but names nothing.
func TestResolveKubeconfigRecordWithoutAPath(t *testing.T) {
	_, err := ResolveKubeconfig("", "", fakeRecords{recorded: true})
	if err == nil || !strings.Contains(err.Error(), "names no kubeconfig") {
		t.Fatalf("error = %v, want a record-names-nothing refusal", err)
	}
}
