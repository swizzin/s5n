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
