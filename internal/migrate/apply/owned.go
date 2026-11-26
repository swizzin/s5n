package apply

// owned.go is step 9: the connections a rewriter pointed at the box go into
// wiring's record, so wiring adopts them by id instead of creating a second one
// (AGENTS.md, invariant 11; docs/box.md, "Migrated connections are adopted by ID").
// The record is `<release>-wiring`'s owned.json, seeded before the Job first runs,
// which is why migrate needs an installed release: the ConfigMap is Helm's and its
// install-time patch would drop an unrendered key.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/wiring"
)

// OwnedStore is the record's `owned.json` key: Read returns it (nil when absent),
// Write replaces it; the implementation updates with the resourceVersion it read.
type OwnedStore interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
}

// seedOwned merges this run's adopted connections into the record. An entry
// already naming another id stops the run: wiring would update an object it does
// not own and leave behind the one it made.
func seedOwned(data []byte, reports []rewrite.Report, apps map[string]string) (*wiring.OwnedRecord, []rewrite.Owned, error) {
	rec, err := wiring.ParseOwnedRecord(data)
	if err != nil {
		return nil, nil, err
	}
	var seeded []rewrite.Owned
	for _, rep := range reports {
		for _, o := range rep.Owned {
			// Holder is the app storing the connection; Target is what it now points
			// at.
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

// place files one connection under wiring's key: the storing instance, or PBH's
// string id (internal/wiring/wiring.go).
func place(rec *wiring.OwnedRecord, holder string, o rewrite.Owned, apps map[string]string) error {
	app := apps[holder]
	switch o.Kind {
	case "downloadclient":
		if app != appSonarr && app != appRadarr && app != appLidarr {
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
		// Keyed by the Arr it reaches, not the Prowlarr that stores it.
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
	return ok && (app == appSonarr || app == appRadarr || app == appLidarr || app == appProwlarr)
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
