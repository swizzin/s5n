// Command render-grant prints one user's admission params and bindings.
//
//	render-grant <chart-dir> <namespace> <torrent-node-port> <service-type>
//
// The multi-document YAML it writes goes through `kubectl apply -f -` as root, at `box
// adduser` and again on every apply, so a release that blesses a new repository reaches
// existing seedboxes. It reads the chart directory it is given for the same reason.
//
// It is a hack because the logic belongs to box (internal/admission) and this is the
// platform layer's caller until `box adduser` exists.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/swizzin/s5n/internal/admission"
)

const usage = "usage: render-grant <chart-dir> <namespace> <torrent-node-port> <service-type>"

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	port, err := strconv.Atoi(os.Args[3])
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-grant: torrent nodePort %q is not a number\n", os.Args[3])
		os.Exit(1)
	}
	docs, err := admission.RenderChart(os.Args[1], admission.Grant{
		Namespace:       os.Args[2],
		TorrentNodePort: port,
		ServiceType:     os.Args[4],
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-grant: %v\n", err)
		os.Exit(1)
	}
	out, err := docs.YAML()
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-grant: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintf(os.Stderr, "render-grant: write: %v\n", err)
		os.Exit(1)
	}
}
