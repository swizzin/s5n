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
	Qui      *QuiOwned           `json:"qui,omitempty"`
	Autobrr  *AutobrrOwned       `json:"autobrr,omitempty"`
	Pbh      *PbhOwned           `json:"pbh,omitempty"`
}

// PbhOwned is what the Job owns in PeerBanHelper: one qBittorrent downloader,
// recorded by the string id it lives under in PBH's own `client:` config (PBH
// numbers nothing, and a name is the user's to change), and with the sha256 of
// the qBittorrent password it was last written and proven with.
//
// PBH does echo the stored password back from its status endpoint, which would
// make a comparison possible -- and is exactly what invariant 10 forbids
// reading a secret to do. The fingerprint lives here for the same reason it
// does for qui's and autobrr's copies: a rotation in the release Secret has to
// be visible without ever handling the value.
type PbhOwned struct {
	Downloader              string `json:"downloader,omitempty"`
	QbitPasswordFingerprint string `json:"qbitPasswordFingerprint,omitempty"`
}

// QuiOwned is what the Job owns in qui, an app that keeps its own user table
// and hands out API keys only to a signed-in session, so its instance can be
// registered only with the account the chart owns (see docs/box.md, "An app
// that keeps its own user table has one account box owns").
//
// QbitPasswordFingerprint is sha256-hex of the qBittorrent password the
// instance was last written and proven with. qui never returns a stored
// password, so a password rotated in the release Secret is otherwise
// invisible: the same rule as Tautulli's Plex token, and the same reason it
// lives in owned.json (a read error stops the run rather than guessing).
type QuiOwned struct {
	Instance                int    `json:"instance,omitempty"`
	QbitPasswordFingerprint string `json:"qbitPasswordFingerprint,omitempty"`
}

// AutobrrOwned is what the Job owns in autobrr: one download client per Arr
// instance plus qBittorrent, each recorded by id (a rename is the user's
// business) and each with a fingerprint of the secret last written and proven
// — the qBittorrent password or that Arr's API key, neither of which autobrr
// returns. A changed fingerprint is a stale copy and gets an update.
type AutobrrOwned struct {
	Clients      map[string]int    `json:"clients,omitempty"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
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

// ArrOwned is what the Job owns on one Arr instance. The download client is
// the only object, and DownloadClientPasswordFingerprint is sha256-hex of the
// qBittorrent password it was last written and proven with: the Arr answers a
// mask for that field, so a password rotated in the release Secret is otherwise
// invisible and the client would keep dialling with the old one.
type ArrOwned struct {
	DownloadClient                    int    `json:"downloadClient,omitempty"`
	DownloadClientPasswordFingerprint string `json:"downloadClientPasswordFingerprint,omitempty"`
}

// ProwlarrOwned is what the Job owns in Prowlarr: one application per Arr
// instance, each recorded by id, and the sha256-hex of the Arr API key it was
// last written and proven with. Prowlarr answers a mask for a stored key, so the
// fingerprint is what makes a rotated key in the release Secret visible -- the
// same rule as the qBittorrent password in qui, autobrr and every Arr.
type ProwlarrOwned struct {
	Applications map[string]int    `json:"applications,omitempty"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
}

// NewOwnedRecord returns an empty record ready to be filled in during a run
// that has nothing to adopt yet (a brand-new release, or a ConfigMap the Job
// could not read).
func NewOwnedRecord() *OwnedRecord {
	return &OwnedRecord{
		Version:  1,
		Arr:      map[string]ArrOwned{},
		Prowlarr: ProwlarrOwned{Applications: map[string]int{}, Fingerprints: map[string]string{}},
		Autobrr:  &AutobrrOwned{Clients: map[string]int{}, Fingerprints: map[string]string{}},
		Pbh:      &PbhOwned{},
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
	if rec.Prowlarr.Fingerprints == nil {
		rec.Prowlarr.Fingerprints = map[string]string{}
	}
	if rec.Autobrr == nil {
		rec.Autobrr = &AutobrrOwned{}
	}
	if rec.Autobrr.Clients == nil {
		rec.Autobrr.Clients = map[string]int{}
	}
	if rec.Autobrr.Fingerprints == nil {
		rec.Autobrr.Fingerprints = map[string]string{}
	}
	return rec, nil
}

// ArrDownloadClientID returns the recorded id for an Arr instance's download
// client, or 0 when nothing is recorded.
func (o *OwnedRecord) ArrDownloadClientID(instance string) int {
	return o.Arr[instance].DownloadClient
}

// SetArrDownloadClientID records the id of the download client the Job
// created or adopted for an Arr instance, keeping the password fingerprint
// recorded beside it.
func (o *OwnedRecord) SetArrDownloadClientID(instance string, id int) {
	if o.Arr == nil {
		o.Arr = map[string]ArrOwned{}
	}
	rec := o.Arr[instance]
	rec.DownloadClient = id
	o.Arr[instance] = rec
}

// ArrDownloadClientPasswordFingerprint returns the fingerprint of the
// qBittorrent password the Arr's download client was last written and proven
// with, or "" when no successful write is on record (a first run, or an install
// predating this record).
func (o *OwnedRecord) ArrDownloadClientPasswordFingerprint(instance string) string {
	return o.Arr[instance].DownloadClientPasswordFingerprint
}

// SetArrDownloadClientPasswordFingerprint records the fingerprint of the
// qBittorrent password the Job just wrote into one Arr's download client and
// proved with the Arr's own connection test. Call it only after that succeeded:
// a fingerprint stored for a write the Arr did not accept would hide the next
// rotation and leave the client dialling with a password qBittorrent refuses.
func (o *OwnedRecord) SetArrDownloadClientPasswordFingerprint(instance, fingerprint string) {
	if o.Arr == nil {
		o.Arr = map[string]ArrOwned{}
	}
	rec := o.Arr[instance]
	rec.DownloadClientPasswordFingerprint = fingerprint
	o.Arr[instance] = rec
}

// ProwlarrApplicationID returns the recorded id for an Arr instance's
// Prowlarr application, or 0 when nothing is recorded.
func (o *OwnedRecord) ProwlarrApplicationID(instance string) int {
	return o.Prowlarr.Applications[instance]
}

// SetProwlarrApplicationID records the id of the Prowlarr application the Job
// created or adopted for an Arr instance, keeping the key fingerprint recorded
// beside it.
func (o *OwnedRecord) SetProwlarrApplicationID(instance string, id int) {
	if o.Prowlarr.Applications == nil {
		o.Prowlarr.Applications = map[string]int{}
	}
	o.Prowlarr.Applications[instance] = id
}

// ProwlarrApplicationFingerprint returns the fingerprint of the Arr API key the
// application was last written and proven with, or "" when no successful write
// is on record.
func (o *OwnedRecord) ProwlarrApplicationFingerprint(instance string) string {
	return o.Prowlarr.Fingerprints[instance]
}

// SetProwlarrApplicationFingerprint records the fingerprint of the Arr API key
// the Job just wrote into one Prowlarr application and proved with Prowlarr's
// own test endpoint. Call it only after that succeeded.
func (o *OwnedRecord) SetProwlarrApplicationFingerprint(instance, fingerprint string) {
	if o.Prowlarr.Fingerprints == nil {
		o.Prowlarr.Fingerprints = map[string]string{}
	}
	o.Prowlarr.Fingerprints[instance] = fingerprint
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

// QuiInstanceID returns the recorded qui instance id, or 0 when nothing is
// recorded (a first run, or an install predating this record).
func (o *OwnedRecord) QuiInstanceID() int {
	if o.Qui == nil {
		return 0
	}
	return o.Qui.Instance
}

// SetQuiInstanceID records the id of the instance the Job created or adopted.
func (o *OwnedRecord) SetQuiInstanceID(id int) {
	if o.Qui == nil {
		o.Qui = &QuiOwned{}
	}
	o.Qui.Instance = id
}

// QuiQbitPasswordFingerprint returns the fingerprint of the qBittorrent
// password the instance was last written and proven with, or "" when no
// successful write is on record (a first run, or an install predating it).
func (o *OwnedRecord) QuiQbitPasswordFingerprint() string {
	if o.Qui == nil {
		return ""
	}
	return o.Qui.QbitPasswordFingerprint
}

// SetQuiQbitPasswordFingerprint records the fingerprint of the qBittorrent
// password the Job just wrote into qui and proved with qui's own test. Call it
// only after that succeeded: a fingerprint stored for a write the app did not
// accept would hide the next rotation.
func (o *OwnedRecord) SetQuiQbitPasswordFingerprint(fingerprint string) {
	if o.Qui == nil {
		o.Qui = &QuiOwned{}
	}
	o.Qui.QbitPasswordFingerprint = fingerprint
}

// AutobrrClientID returns the recorded id for one autobrr download client, or 0
// when nothing is recorded.
func (o *OwnedRecord) AutobrrClientID(name string) int {
	if o.Autobrr == nil {
		return 0
	}
	return o.Autobrr.Clients[name]
}

// SetAutobrrClientID records the id of the download client the Job created or
// adopted under that name.
func (o *OwnedRecord) SetAutobrrClientID(name string, id int) {
	if o.Autobrr == nil {
		o.Autobrr = &AutobrrOwned{}
	}
	if o.Autobrr.Clients == nil {
		o.Autobrr.Clients = map[string]int{}
	}
	o.Autobrr.Clients[name] = id
}

// AutobrrClientFingerprint returns the fingerprint of the secret last written
// and proven into one autobrr download client, or "" when no successful write
// is on record.
func (o *OwnedRecord) AutobrrClientFingerprint(name string) string {
	if o.Autobrr == nil {
		return ""
	}
	return o.Autobrr.Fingerprints[name]
}

// SetAutobrrClientFingerprint records the fingerprint of the secret the Job
// just wrote into one autobrr download client and proved with autobrr's own
// test. Call it only after that succeeded.
func (o *OwnedRecord) SetAutobrrClientFingerprint(name, fingerprint string) {
	if o.Autobrr == nil {
		o.Autobrr = &AutobrrOwned{}
	}
	if o.Autobrr.Fingerprints == nil {
		o.Autobrr.Fingerprints = map[string]string{}
	}
	o.Autobrr.Fingerprints[name] = fingerprint
}

// PbhDownloaderID returns the recorded downloader id, or "" when nothing is
// recorded (a first run, or an install predating this record).
func (o *OwnedRecord) PbhDownloaderID() string {
	if o.Pbh == nil {
		return ""
	}
	return o.Pbh.Downloader
}

// SetPbhDownloaderID records the downloader the Job created or adopted.
func (o *OwnedRecord) SetPbhDownloaderID(id string) {
	if o.Pbh == nil {
		o.Pbh = &PbhOwned{}
	}
	o.Pbh.Downloader = id
}

// PbhQbitPasswordFingerprint returns the fingerprint of the qBittorrent
// password the downloader was last written and proven with, or "" when no
// successful write is on record.
func (o *OwnedRecord) PbhQbitPasswordFingerprint() string {
	if o.Pbh == nil {
		return ""
	}
	return o.Pbh.QbitPasswordFingerprint
}

// SetPbhQbitPasswordFingerprint records the fingerprint of the qBittorrent
// password the Job just wrote into PBH and proved with its own HEALTHY status.
// Call it only after that: a fingerprint stored for a write PBH never accepted
// would hide the next rotation and leave PBH dialling with a refused password.
func (o *OwnedRecord) SetPbhQbitPasswordFingerprint(fingerprint string) {
	if o.Pbh == nil {
		o.Pbh = &PbhOwned{}
	}
	o.Pbh.QbitPasswordFingerprint = fingerprint
}
