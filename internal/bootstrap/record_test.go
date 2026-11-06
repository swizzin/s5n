package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	in := &Record{
		Source:       SourceExternal,
		Kubeconfig:   "/home/example/.kube/config",
		Distribution: DistK3s,
		Nodes:        3,
		Platform: PlatformConfig{
			GatewayAPI:   ModeAuto,
			LoadBalancer: ModeManaged,
			Gateway:      ModeExternal,
			CertManager:  ModeAuto,
		},
	}
	out, err := parseRecord(encodeRecord(in))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestRecordRoundTripK0sManaged(t *testing.T) {
	in := &Record{
		Source:       SourceK0sManaged,
		Kubeconfig:   K0sAdminConfig,
		Distribution: DistK0s,
		Nodes:        1,
		Platform:     DefaultPlatform(),
	}
	out, err := parseRecord(encodeRecord(in))
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestParseRecordRejectsBadInput(t *testing.T) {
	base := `source: external
kubeconfig: "/home/example/.kube/config"
distribution: k3s
nodes: 1
platform:
  gateway-api: auto
  loadbalancer: auto
  gateway: auto
  cert-manager: auto
`
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"unknown top key", func(s string) string { return s + "bogus: 1\n" }, "unknown key"},
		{"unknown source", func(s string) string { return strings.Replace(s, "source: external", "source: cloud", 1) }, "unknown source"},
		{"bad nodes", func(s string) string { return strings.Replace(s, "nodes: 1", "nodes: many", 1) }, "not a number"},
		{"missing platform key", func(s string) string { return strings.Replace(s, "  cert-manager: auto\n", "", 1) }, "missing"},
		{"bad mode", func(s string) string { return strings.Replace(s, "  gateway: auto", "  gateway: cloud", 1) }, "unknown mode"},
		{"unknown platform key", func(s string) string { return strings.Replace(s, "  gateway: auto", "  traefik: auto", 1) }, "unknown platform key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRecord([]byte(tc.mutate(base)))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestFilesystemRecordsLoadSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swizzin", "cluster.yaml")
	store := FilesystemRecords{Path: path}

	// absent -> (nil, nil)
	rec, err := store.Load()
	if err != nil || rec != nil {
		t.Fatalf("absent record: rec=%v err=%v, want nil,nil", rec, err)
	}

	want := &Record{Source: SourceExternal, Kubeconfig: "/x", Distribution: DistK0s, Nodes: 1, Platform: DefaultPlatform()}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip mismatch:\n want: %+v\n  got: %+v", want, got)
	}

	// mode 0600
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %o, want 600", fi.Mode().Perm())
	}
}
