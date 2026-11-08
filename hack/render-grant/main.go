// Command render-grant prints one user's admission params and bindings.
//
//	render-grant <chart-dir> <namespace> <torrent-node-port> <service-type>
//
// The multi-document YAML it writes goes through `kubectl apply -f -` as root:
// at `box adduser`, and again at `box install`, `box update` and
// `box bootstrap`, so a chart release that blesses a new repository reaches
// existing seedboxes. That is also why it reads the chart directory it is given
// rather than a copy of the list: the params and the release have to agree.
//
// It is a hack because the logic belongs to box (internal/admission) and this
// program is the platform layer's caller until `box adduser` exists — at which
// point the e2e stops shelling out and box calls the same package.
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
