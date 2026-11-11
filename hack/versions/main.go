// Command versions prints the platform versions box installs, one KEY=VALUE
// line per component, for the e2e scripts.
//
// It reads charts/s5n/platform.yaml from this checkout through the parser box
// uses (internal/release.ParseVersions), which is the file `helm package` puts
// inside the chart release box pulls. The e2e of HEAD therefore tests the pins
// of HEAD, and no script needs a YAML parser of its own (yq is not installed on
// every host the e2e runs on). It is a reader, not a second list: the chart
// file is the only home for a version.
//
// Usage:
//
//	go run ./hack/versions [path to platform.yaml]
package main

import (
	"fmt"
	"os"

	"github.com/swizzin/s5n/internal/release"
)

// defaultFile is the chart's pins, relative to the repository root, which is
// where the e2e scripts run this from.
const defaultFile = "charts/s5n/platform.yaml"

func main() {
	file := defaultFile
	switch len(os.Args) {
	case 1:
	case 2:
		file = os.Args[1]
	default:
		fmt.Fprintln(os.Stderr, "usage: versions [path to platform.yaml]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "versions: %v\n", err)
		os.Exit(1)
	}
	pins, err := release.ParseVersions(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "versions: %s: %v\n", file, err)
		os.Exit(1)
	}
	values := []struct {
		key   string
		value string
	}{
		{"authelia", pins.Authelia},
		{"k0s", pins.K0s},
		{"envoyGateway", pins.EnvoyGateway},
		{"metallb", pins.MetalLB},
		{"certManager", pins.CertManager},
	}
	for _, pin := range values {
		if pin.value == "" {
			// The parser already refuses an empty value; this is the belt for
			// a future component added to one list and not the other.
			fmt.Fprintf(os.Stderr, "versions: %s has no pin in %s\n", pin.key, file)
			os.Exit(1)
		}
		fmt.Printf("%s=%s\n", pin.key, pin.value)
	}
}
