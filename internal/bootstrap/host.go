package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// LinuxHost reports, read-only, the host-level signs PLAN.md step 3 lists: a
// running kubelet, containerd or Docker; a k3s/k0s/rke2/microk8s binary or
// unit; or anything listening on 6443, 10250 or 2379.
type LinuxHost struct{}

// Present scans /proc, PATH, the systemd unit directories and /proc/net for
// evidence of an existing cluster installation.
func (LinuxHost) Present() []Finding {
	var out []Finding
	out = append(out, runningProcesses(processNames()...)...)
	out = append(out, installedBinaries(binaryNames()...)...)
	out = append(out, installedUnits(unitNames()...)...)
	out = append(out, listeningOn(6443, 10250, 2379)...)
	return out
}

func processNames() []string {
	return []string{"kubelet", "containerd", "dockerd", "k3s", "k0s", "rke2", "microk8s", "microk8s.daemon-kubelite"}
}

func binaryNames() []string {
	return []string{"k3s", "k0s", "rke2", "microk8s"}
}

func unitNames() []string {
	return []string{
		"k3s.service", "k3s-agent.service",
		"rke2.service", "rke2-server.service", "rke2-agent.service",
		"k0scontroller.service", "k0sworker.service",
		"snap.microk8s.daemon-kubelite.service", "snap.microk8s.daemon-containerd.service",
	}
}

var unitDirs = []string{
	"/etc/systemd/system",
	"/run/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

// runningProcesses matches /proc/*/comm against the names.
func runningProcesses(names ...string) []Finding {
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	var out []Finding
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a PID
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if seen[name] {
			out = append(out, Finding{Kind: "process", Name: name})
		}
	}
	return out
}

// installedBinaries looks each name up on PATH.
func installedBinaries(names ...string) []Finding {
	var out []Finding
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			out = append(out, Finding{Kind: "binary", Name: p})
		}
	}
	return out
}

// installedUnits looks for a unit file in the systemd unit directories.
func installedUnits(names ...string) []Finding {
	var out []Finding
	for _, n := range names {
		for _, dir := range unitDirs {
			if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
				out = append(out, Finding{Kind: "unit", Name: n})
				break
			}
		}
	}
	return out
}

// listeningOn reports which of the given TCP ports are in LISTEN, from
// /proc/net/tcp and /proc/net/tcp6.
func listeningOn(ports ...int) []Finding {
	want := map[int]bool{}
	for _, p := range ports {
		want[p] = true
	}
	var out []Finding
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		out = append(out, listenPortsIn(f, want)...)
	}
	return out
}

func listenPortsIn(path string, want map[int]bool) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []Finding
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if i == 0 { // header
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "0A" { // TCP_LISTEN
			continue
		}
		local := fields[1] // HEXIP:HEXPORT
		idx := strings.LastIndexByte(local, ':')
		if idx < 0 {
			continue
		}
		port, err := strconv.ParseUint(local[idx+1:], 16, 32)
		if err != nil {
			continue
		}
		if want[int(port)] {
			out = append(out, Finding{Kind: "port", Name: strconv.Itoa(int(port))})
		}
	}
	return out
}
