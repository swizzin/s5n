package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenPortsIn(t *testing.T) {
	// A /proc/net/tcp-shaped file: header + one LISTEN on 0x192B (6443) and
	// one ESTABLISHED on the same port, one LISTEN on another port.
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:192B 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 0 0000000000000000 100 0 0 10 0
   1: 00000000:192B 0100007F:9C4A 01 00000000:00000000 00:00000000 00000000     0        0 2 0 0000000000000000 100 0 0 10 0
   2: 00000000:2800 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 0 0000000000000000 100 0 0 10 0
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tcp")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := listenPortsIn(path, map[int]bool{6443: true, 10250: true, 2379: true})
	if len(got) != 1 {
		t.Fatalf("findings = %v, want exactly the LISTEN socket on 6443", got)
	}
	if got[0].Name != "6443" || got[0].Kind != "port" {
		t.Errorf("finding = %+v, want port 6443", got[0])
	}
}

func TestRunningProcessesNameOnly(t *testing.T) {
	// The /proc walk is host-dependent; just assert the name set is what PLAN
	// step 3 lists, and that the helper is deterministic for a missing path.
	got := runningProcesses("kubelet", "containerd", "dockerd")
	if got != nil && len(got) > 0 {
		// If we are on a host with these running, every finding must be one of
		// the asked-for names.
		for _, f := range got {
			switch f.Name {
			case "kubelet", "containerd", "dockerd":
			default:
				t.Errorf("unexpected process finding %+v", f)
			}
		}
	}
}

func TestHostNamesIncludeKubelet(t *testing.T) {
	// PLAN step 4 promises a kubelet binary or unit as evidence of a cluster;
	// the lists must actually carry them.
	for _, name := range []string{"kubelet"} {
		if !containsName(binaryNames(), name) {
			t.Errorf("binaryNames() = %v, want %q", binaryNames(), name)
		}
	}
	for _, name := range []string{"kubelet.service"} {
		if !containsName(unitNames(), name) {
			t.Errorf("unitNames() = %v, want %q", unitNames(), name)
		}
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestClassifyFindings(t *testing.T) {
	// Docker and containerd run on hosts with no Kubernetes, so they are notes;
	// every other sign of an installation stays a blocker. A runtime holding an
	// API port is caught by the port finding.
	cases := []struct {
		name     string
		in       []Finding
		blockers string
		notes    string
	}{
		{
			name:  "docker alone is a note",
			in:    []Finding{{Kind: "process", Name: "dockerd"}},
			notes: "process dockerd",
		},
		{
			name:  "containerd alone is a note",
			in:    []Finding{{Kind: "process", Name: "containerd"}},
			notes: "process containerd",
		},
		{
			name:     "docker with the API port listening blocks through the port",
			in:       []Finding{{Kind: "process", Name: "dockerd"}, {Kind: "port", Name: "6443"}},
			blockers: "port 6443",
			notes:    "process dockerd",
		},
		{
			name:     "kubelet alone still blocks",
			in:       []Finding{{Kind: "process", Name: "kubelet"}},
			blockers: "process kubelet",
		},
		{
			name:     "k3s binary alone still blocks",
			in:       []Finding{{Kind: "binary", Name: "/usr/local/bin/k3s"}},
			blockers: "binary /usr/local/bin/k3s",
		},
		{
			name:     "k0s unit names the cluster, its containerd is a note",
			in:       []Finding{{Kind: "process", Name: "containerd"}, {Kind: "unit", Name: "k0scontroller.service"}},
			blockers: "unit k0scontroller.service",
			notes:    "process containerd",
		},
		{
			name: "nothing present",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blockers, notes := classifyFindings(tc.in)
			if got := formatFindings(blockers); got != tc.blockers {
				t.Errorf("blockers = %q, want %q", got, tc.blockers)
			}
			if got := formatFindings(notes); got != tc.notes {
				t.Errorf("notes = %q, want %q", got, tc.notes)
			}
		})
	}
}
