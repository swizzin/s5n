package autobrr

import (
	"context"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// An Arr is a client row autobrr dials by its host column alone: the downloader
// service builds the Arr's config from Hostname: cfg.Host and never reads the
// port column (internal/downloader/service.go:514-515 at v1.90.0), and the Arr
// client parses that host as a URL and joins /api/v3 onto it
// (pkg/arr/sonarr/client.go:19-24). So autobrr stores an Arr as a whole URL in
// host, and the port column keeps whatever the form left there, 10000.
const arrRowAsStored = `INSERT INTO client (id, name, enabled, type, host, port, tls)
VALUES (2, 'Sonarr', 1, 'SONARR', 'http://sonarr:8989/sonarr-old', 10000, 0);`

func arrFixture() string {
	return strings.Replace(dbFixture,
		`INSERT INTO client (id, name, enabled, type, host, port, tls)
VALUES (2, 'Sonarr', 1, 'SONARR', 'sonarr.default.svc', 8989, 1);`, arrRowAsStored, 1)
}

// TestArrClientPortIsTheURLs: discover writes every Service rule narrowed to
// the Service's port, as discover does. The Arr's port is the one in
// its URL; the port column is not an address autobrr ever dials.
func TestArrClientPortIsTheURLs(t *testing.T) {
	in := setup(t, arrFixture())
	in.Plan.Hosts = []plan.HostRule{{Service: "sonarr.default", Port: 8989, App: "sonarr"}}
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) > 0 {
		t.Fatalf("an Arr stored as http://sonarr:8989/... was read at the port column's 10000: %v", rep.Unclassified)
	}
}

// TestArrClientHostStaysAURL: a rewritten Arr row is still a URL autobrr can
// dial, with the box's scheme, Service, port and URL base, because the host
// column is all autobrr reads for it.
func TestArrClientHostStaysAURL(t *testing.T) {
	ctx := context.Background()
	// The fixture's rule has no port, so this test does not depend on the
	// port fix above.
	in := setup(t, strings.Replace(arrFixture(), "http://sonarr:8989/", "http://sonarr.default.svc:8989/", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("Blocked: %v %v", rep.Uncovered, rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got, want := readClients(t, in.Dir)[2].host, "http://sonarr:8989/sonarr"; got != want {
		t.Errorf("Arr client host = %q, want %q: autobrr parses this column as the Arr's URL", got, want)
	}
}

// TestQbittorrentURLHostKeepsItsPortColumn: qBittorrent is dialled at its host
// plus the port column, even when host holds a URL (BuildLegacyHost,
// internal/domain/downloader.go:245-281 at v1.90.0). Only the Arr-style rows
// that autobrr dials by host alone are URL rows.
func TestQbittorrentURLHostKeepsItsPortColumn(t *testing.T) {
	ctx := context.Background()
	in := setup(t, strings.Replace(dbFixture,
		"VALUES (1, 'qBittorrent', 1, 'QBITTORRENT', 'localhost', 8081, 1);",
		"VALUES (1, 'qBittorrent', 1, 'QBITTORRENT', 'http://localhost', 8081, 1);", 1))
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a qBittorrent row at http://localhost, port column 8081, was not read at 8081: %v", rep.Unclassified)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got, want := readClients(t, in.Dir)[1], (client{"qbittorrent", 8080, 0}); got != want {
		t.Errorf("qBittorrent client = %+v, want %+v: autobrr builds its URL from host and port", got, want)
	}
}
