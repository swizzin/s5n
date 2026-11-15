// Package peerbanhelper is PeerBanHelper's rewriter: it registers `pbh` so
// apply never meets an app without one, and changes nothing.
//
// PeerBanHelper is the one app in the catalog that stores no path and no
// connection in any file migrate copies. Its whole state lives under the
// directory `-Dpbh.datadir` names, and the chart passes `/config`
// (charts/s5n/templates/workloads.yaml, PBH_DATADIR), which is exactly the
// staged directory (PBH-BTN/PeerBanHelper@v9.5.4:Main.java:367-378 reads the
// property and puts `config/`, `persist/`, `logs/` and `plugins/` under it).
// Its config.yml holds no filesystem path at all: the keys that look like paths
// are the IP-database *names* and the `database:` block, which holds a host,
// not a path (src/main/resources/config.yml at v9.5.4). Its downloader
// connections live in the database, in a module table the app rewrites itself
// on first start, not in the copied config.
//
// So the honest answer for pbh is "copied as it is". Registering it anyway is
// the point: rewrite.For refuses an app with no rewriter, because every app's
// paths are rewritten with no exceptions, and a copied database under a data
// root the chart moves to /config needs no rewrite to be right.
package peerbanhelper

import (
	"context"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

func init() { rewrite.Register("pbh", rewriter{}) }

type rewriter struct{}

// Check reads nothing: the note is the whole answer, and it is on Check's
// report so the plan shows it before anything is copied.
func (rewriter) Check(_ context.Context, in rewrite.Input) (rewrite.Report, error) {
	return rewrite.Report{
		App: in.App,
		Notes: []string{
			"its data directory is the chart's /config (-Dpbh.datadir), " +
				"and it stores no path of its own: the copied config and database are used as they are",
		},
	}, nil
}

// Write makes no change, which is exactly what Check reported.
func (rw rewriter) Write(ctx context.Context, in rewrite.Input) (rewrite.Report, error) {
	return rw.Check(ctx, in)
}
