package apply

// owned.go is step 9: the connections a rewriter pointed at the box are handed
// to wiring's ownership record.
//
// Wiring adopts an object by name on its first run and by id afterwards, so a
// download client or a Prowlarr application that already exists under another
// name would otherwise get a second one created beside it (AGENTS.md, invariant
// 11; docs/box.md, "Migrated connections are adopted by ID"). A rewriter reports
// those objects as Report.Owned, and this file puts them where
// internal/wiring looks them up.
//
// The record is `<release>-wiring`'s owned.json, a key Helm never renders and
// the wiring Job upserts at run time. It is seeded here, before the apply that
// first runs the Job, which is why migrate only works against a release that is
// already installed: the ConfigMap is Helm's, and its install-time adopt patch
// would drop a key it never rendered.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/wiring"
)

// OwnedStore is the wiring ownership record of the release being migrated into:
// the `owned.json` key of `<release>-wiring`.
//
// Read returns the raw value, nil when the key is absent (a release whose Job
// has never run). Write replaces it. The implementation owns reading the
// ConfigMap twice and updating it with the resourceVersion it read, so two
// writers cannot silently drop each other's ids.
type OwnedStore interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
}

// seedOwned merges every connection this run's reports adopted into the
// release's record.
//
// An entry that already names a different id for the same key stops the run:
// the object was wired by someone else, and adopting this run's id would make
// wiring update an object it does not own while the one it created is left
// behind.
func seedOwned(data []byte, reports []rewrite.Report, apps map[string]string) (*wiring.OwnedRecord, []rewrite.Owned, error) {
	rec, err := wiring.ParseOwnedRecord(data)
	if err != nil {
		return nil, nil, err
	}
	var seeded []rewrite.Owned
	for _, rep := range reports {
		for _, o := range rep.Owned {
			// The holder is the app that stores the connection, which is the
			// app this report came from; Target is what it now points at.
			if err := place(rec, rep.App, o, apps); err != nil {
				return nil, nil, err
			}
			seeded = append(seeded, o)
		}
	}
	sort.Slice(seeded, func(i, j int) bool {
		if seeded[i].Kind != seeded[j].Kind {
			return seeded[i].Kind < seeded[j].Kind
		}
		return seeded[i].ID < seeded[j].ID
	})
	return rec, seeded, nil
}

// place files one adopted connection under the key wiring looks it up by
// (internal/wiring/wiring.go): an Arr's download client and a Prowlarr
// application are both keyed by the instance that stores them, and PBH's
// downloader is one string id on the app itself.
func place(rec *wiring.OwnedRecord, holder string, o rewrite.Owned, apps map[string]string) error {
	app := apps[holder]
	switch o.Kind {
	case "downloadclient":
		if app != appSonarr && app != appRadarr {
			return fmt.Errorf("%s (%s): this box has no place in owned.json for a %s on %s", holder, app, o.Kind, app)
		}
		if prev := rec.ArrDownloadClientID(holder); prev != 0 && prev != o.ID {
			return ownedConflict(holder, "download client", prev, o.ID)
		}
		rec.SetArrDownloadClientID(holder, o.ID)
		return nil
	case "application":
		if app != appProwlarr {
			return fmt.Errorf("%s (%s): a Prowlarr application can only be recorded on Prowlarr, not on %s", holder, app, app)
		}
		// The application lives in Prowlarr and is keyed by the Arr it reaches,
		// which is the box instance the rewrite pointed it at.
		if !isArr(o.Target, apps) {
			return fmt.Errorf("%s: the application %q now points at %q, which is not an Arr instance this migration adopts", holder, o.Name, o.Target)
		}
		if prev := rec.ProwlarrApplicationID(o.Target); prev != 0 && prev != o.ID {
			return ownedConflict(o.Target, "Prowlarr application", prev, o.ID)
		}
		rec.SetProwlarrApplicationID(o.Target, o.ID)
		return nil
	case "pbh-downloader":
		if app != appPBH {
			return fmt.Errorf("%s (%s): a downloader can only be recorded on PeerBanHelper", holder, app)
		}
		if prev := rec.PbhDownloaderID(); prev != "" && prev != o.StringID {
			return fmt.Errorf("%s: owned.json already records downloader %q, and this migration adopts %q; someone wired it first", holder, prev, o.StringID)
		}
		rec.SetPbhDownloaderID(o.StringID)
		return nil
	}
	return fmt.Errorf("%s: owned.json has no place for a %s", holder, o.Kind)
}

// isArr reports whether target is an Arr instance of this migration, which is
// what a Prowlarr application has to point at to be keyed by it.
func isArr(target string, apps map[string]string) bool {
	app, ok := apps[target]
	return ok && (app == appSonarr || app == appRadarr || app == appProwlarr)
}

// ownedConflict refuses a record that already names another id.
func ownedConflict(key, kind string, prev, now int) error {
	if prev == 0 {
		return fmt.Errorf("%s: owned.json already records a different %s than this migration adopts; someone wired it first, so remove the record or the plan entry", key, kind)
	}
	return fmt.Errorf("%s: owned.json already records %s id %d, and this migration adopts id %d; someone wired it first", key, kind, prev, now)
}

// encodeOwned is the record as owned.json holds it: the Job's own encoding,
// which the record's tags pin.
func encodeOwned(rec *wiring.OwnedRecord) ([]byte, error) {
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode owned.json: %w", err)
	}
	return append(raw, '\n'), nil
}
