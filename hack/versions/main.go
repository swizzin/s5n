// Command versions prints the platform versions box installs, one KEY=VALUE
// line per component, for the e2e scripts.
//
// It reads install/platform/versions.yaml through the same embedded bytes box
// compiles in, so a script cannot disagree with box about what is pinned, and
// no script needs a YAML parser of its own (yq is not installed on every host
// the e2e runs on). It is a reader, not a second list: the file is the only
// home for a version.
//
// Usage:
//
//	go run ./hack/versions
package main

import (
	"fmt"
	"os"

	platformassets "github.com/swizzin/s5n/install/platform"
)

func main() {
	pins := [][2]string{
		{"authelia", platformassets.Pins.Authelia},
		{"k0s", platformassets.Pins.K0s},
		{"envoyGateway", platformassets.Pins.EnvoyGateway},
		{"metallb", platformassets.Pins.MetalLB},
		{"certManager", platformassets.Pins.CertManager},
	}
	for _, pin := range pins {
		if pin[1] == "" {
			// The embedded parser already refuses an empty value; this is the
			// belt for a future component added to one list and not the other.
			fmt.Fprintf(os.Stderr, "versions: %s has no pin in install/platform/versions.yaml\n", pin[0])
			os.Exit(1)
		}
		fmt.Printf("%s=%s\n", pin[0], pin[1])
	}
}
