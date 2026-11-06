package wiring

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OwnedRecord is the `owned.json` key in the `<release>-wiring` ConfigMap: the
// only evidence of which Arr download client or Prowlarr application the
// wiring Job itself created or adopted. The live object's name is not part of
// its identity — a rename in the app's own UI is the user's business — so
// ownership is tracked by id instead (see AGENTS.md invariant 11).
type OwnedRecord struct {
	Version  int                 `json:"version"`
	Arr      map[string]ArrOwned `json:"arr,omitempty"`
	Prowlarr ProwlarrOwned       `json:"prowlarr,omitempty"`
	Tautulli *TautulliOwned      `json:"tautulli,omitempty"`
}

// TautulliOwned is what the Job last wrote into Tautulli that the app will not
// read back. PlexTokenFingerprint is sha256-hex of the Plex server token the
// PMS link was last written and proven with: Tautulli lists PMS_TOKEN among its
// do-not-download keys, so get_settings answers null and the fingerprint is the
// only way a rotated token in the release Secret can be detected. It is a
// fingerprint, never the token itself (AGENTS.md invariant 7), and it lives in
// owned.json because that record is already the Job's durable, fail-closed
// state: a read error stops the run rather than guessing.
type TautulliOwned struct {
	PlexTokenFingerprint string `json:"plexTokenFingerprint,omitempty"`
}

// ArrOwned is what the Job owns on one Arr instance.
type ArrOwned struct {
	DownloadClient int `json:"downloadClient,omitempty"`
}

// ProwlarrOwned is what the Job owns in Prowlarr.
type ProwlarrOwned struct {
	Applications map[string]int `json:"applications,omitempty"`
}

// NewOwnedRecord returns an empty record ready to be filled in during a run
// that has nothing to adopt yet (a brand-new release, or a ConfigMap the Job
// could not read).
func NewOwnedRecord() *OwnedRecord {
	return &OwnedRecord{
		Version:  1,
		Arr:      map[string]ArrOwned{},
		Prowlarr: ProwlarrOwned{Applications: map[string]int{}},
	}
}

// ParseOwnedRecord decodes owned.json. Empty input is not an error: it is
// what a fresh release's ConfigMap looks like before the first successful
// run, and callers should adopt existing objects by name.
func ParseOwnedRecord(raw []byte) (*OwnedRecord, error) {
	rec := NewOwnedRecord()
	if len(bytes.TrimSpace(raw)) == 0 {
		return rec, nil
	}
	if err := json.Unmarshal(raw, rec); err != nil {
		return nil, fmt.Errorf("parse owned.json: %w", err)
	}
	if rec.Arr == nil {
		rec.Arr = map[string]ArrOwned{}
	}
	if rec.Prowlarr.Applications == nil {
		rec.Prowlarr.Applications = map[string]int{}
	}
	return rec, nil
}

// ArrDownloadClientID returns the recorded id for an Arr instance's download
// client, or 0 when nothing is recorded.
func (o *OwnedRecord) ArrDownloadClientID(instance string) int {
	return o.Arr[instance].DownloadClient
}

// SetArrDownloadClientID records the id of the download client the Job
// created or adopted for an Arr instance.
func (o *OwnedRecord) SetArrDownloadClientID(instance string, id int) {
	if o.Arr == nil {
		o.Arr = map[string]ArrOwned{}
	}
	o.Arr[instance] = ArrOwned{DownloadClient: id}
}

// ProwlarrApplicationID returns the recorded id for an Arr instance's
// Prowlarr application, or 0 when nothing is recorded.
func (o *OwnedRecord) ProwlarrApplicationID(instance string) int {
	return o.Prowlarr.Applications[instance]
}

// SetProwlarrApplicationID records the id of the Prowlarr application the Job
// created or adopted for an Arr instance.
func (o *OwnedRecord) SetProwlarrApplicationID(instance string, id int) {
	if o.Prowlarr.Applications == nil {
		o.Prowlarr.Applications = map[string]int{}
	}
	o.Prowlarr.Applications[instance] = id
}

// TautulliPlexTokenFingerprint returns the fingerprint of the Plex server token
// last written and proven into Tautulli's PMS link, or "" when no successful
// write is on record (a first run, or an install predating this record).
func (o *OwnedRecord) TautulliPlexTokenFingerprint() string {
	if o.Tautulli == nil {
		return ""
	}
	return o.Tautulli.PlexTokenFingerprint
}

// SetTautulliPlexTokenFingerprint records the fingerprint of the Plex server
// token the Job just wrote and proved. Call it only after that succeeded: a
// fingerprint stored for a write the app did not accept would hide a rotation.
func (o *OwnedRecord) SetTautulliPlexTokenFingerprint(fingerprint string) {
	if o.Tautulli == nil {
		o.Tautulli = &TautulliOwned{}
	}
	o.Tautulli.PlexTokenFingerprint = fingerprint
}
