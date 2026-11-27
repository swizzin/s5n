package wiring

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OwnedRecord is the owned.json key in the wiring ConfigMap: the evidence of which
// Arr client or Prowlarr application the Job created or adopted. Ownership is by id,
// because a rename in the app's UI is the user's business (AGENTS.md invariant 11).
type OwnedRecord struct {
	Version     int                   `json:"version"`
	Arr         map[string]ArrOwned   `json:"arr,omitempty"`
	Prowlarr    ProwlarrOwned         `json:"prowlarr,omitempty"`
	Tautulli    *TautulliOwned        `json:"tautulli,omitempty"`
	Maintainerr *MaintainerrOwned     `json:"maintainerr,omitempty"`
	Qui         *QuiOwned             `json:"qui,omitempty"`
	Autobrr     *AutobrrOwned         `json:"autobrr,omitempty"`
	Pbh         *PbhOwned             `json:"pbh,omitempty"`
	Seerr       map[string]SeerrOwned `json:"seerr,omitempty"`
}

// SeerrOwned is the one thing the Job leaves in a Seerr that the app cannot show it set:
// that this Job initialized that instance. Seerr's own initialized flag says a Seerr is
// set up, not who did it, and a person who deletes the admin it made must not have a
// whole setup written under them on the next run.
type SeerrOwned struct {
	Initialized bool `json:"initialized,omitempty"`
}

// PbhOwned is what the Job owns in PeerBanHelper: one downloader, recorded by the
// string id in PBH's own config. No fingerprint: it holds no secret.
type PbhOwned struct {
	Downloader string `json:"downloader,omitempty"`
}

// QuiOwned is what the Job owns in qui, whose instance is registered with the account
// the chart owns (docs/box.md). Its qBittorrent login is empty, so only the Arr rows
// carry fingerprints: qui never answers a stored key
// (internal/models/arr_instance.go marks it json:"-"), so the sha256 of the Arr key is
// the only way a rotation is visible.
type QuiOwned struct {
	Instance        int               `json:"instance,omitempty"`
	ArrInstances    map[string]int    `json:"arrInstances,omitempty"`
	ArrFingerprints map[string]string `json:"arrFingerprints,omitempty"`
}

// AutobrrOwned is what the Job owns in autobrr: one client per Arr instance plus
// qBittorrent, each by id and with a fingerprint of the secret last written, which
// autobrr never returns. A changed fingerprint means a stale copy.
type AutobrrOwned struct {
	Clients      map[string]int    `json:"clients,omitempty"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
}

// TautulliOwned is what the Job last wrote that the app will not read back:
// Tautulli lists PMS_TOKEN among its do-not-download keys, so get_settings answers
// null and this fingerprint is the only way a rotated token is visible. It is never
// the token itself (AGENTS.md invariant 7), and it lives in owned.json because that
// record fails closed.
type TautulliOwned struct {
	PlexTokenFingerprint string `json:"plexTokenFingerprint,omitempty"`
}

// MaintainerrOwned is what the Job last wrote that the app will not read back:
// Maintainerr masks plex_auth_token, so this fingerprint is the only way a rotation
// is visible -- never the token itself (AGENTS.md invariant 7).
type MaintainerrOwned struct {
	PlexTokenFingerprint string `json:"plexTokenFingerprint,omitempty"`
}

// ArrOwned is what the Job owns on one Arr instance: the qBittorrent client and,
// when the release runs it, the SABnzbd one. Neither stores a credential the Job can
// read back, so there is no fingerprint.
type ArrOwned struct {
	DownloadClient int `json:"downloadClient,omitempty"`
	Sabnzbd        int `json:"sabnzbd,omitempty"`
}

// ProwlarrOwned is what the Job owns in Prowlarr: one application per Arr instance,
// by id, with the sha256 of the Arr key it was written with. Prowlarr answers a mask,
// so the fingerprint is what makes a rotation visible.
type ProwlarrOwned struct {
	Applications map[string]int    `json:"applications,omitempty"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
}

// NewOwnedRecord returns an empty record for a run with nothing to adopt yet.
func NewOwnedRecord() *OwnedRecord {
	return &OwnedRecord{
		Version:  1,
		Arr:      map[string]ArrOwned{},
		Prowlarr: ProwlarrOwned{Applications: map[string]int{}, Fingerprints: map[string]string{}},
		Autobrr:  &AutobrrOwned{Clients: map[string]int{}, Fingerprints: map[string]string{}},
		Pbh:      &PbhOwned{},
	}
}

// ParseOwnedRecord decodes owned.json. Empty input is not an error: it is a fresh
// release's ConfigMap before the first successful run.
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
	if rec.Qui != nil {
		if rec.Qui.ArrInstances == nil {
			rec.Qui.ArrInstances = map[string]int{}
		}
		if rec.Qui.ArrFingerprints == nil {
			rec.Qui.ArrFingerprints = map[string]string{}
		}
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
	rec := o.Arr[instance]
	rec.DownloadClient = id
	o.Arr[instance] = rec
}

// ArrSabnzbdClientID returns the recorded id for an Arr instance's SABnzbd
// download client, or 0 when nothing is recorded.
func (o *OwnedRecord) ArrSabnzbdClientID(instance string) int {
	return o.Arr[instance].Sabnzbd
}

// SetArrSabnzbdClientID records the id of the SABnzbd client the Job created or
// adopted for an Arr instance.
func (o *OwnedRecord) SetArrSabnzbdClientID(instance string, id int) {
	if o.Arr == nil {
		o.Arr = map[string]ArrOwned{}
	}
	rec := o.Arr[instance]
	rec.Sabnzbd = id
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

// SetProwlarrApplicationFingerprint records the fingerprint of the Arr key just
// written and proved with Prowlarr's own test endpoint. Call it only after it
// succeeded.
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

// SetTautulliPlexTokenFingerprint records the fingerprint of the token just written
// and proved. Call it only after that succeeded: one stored for a write the app did
// not accept would hide a rotation.
func (o *OwnedRecord) SetTautulliPlexTokenFingerprint(fingerprint string) {
	if o.Tautulli == nil {
		o.Tautulli = &TautulliOwned{}
	}
	o.Tautulli.PlexTokenFingerprint = fingerprint
}

// MaintainerrPlexTokenFingerprint returns the fingerprint of the Plex server token
// last written and proven into Maintainerr's connection, or "" when no successful
// write is on record.
func (o *OwnedRecord) MaintainerrPlexTokenFingerprint() string {
	if o.Maintainerr == nil {
		return ""
	}
	return o.Maintainerr.PlexTokenFingerprint
}

// SetMaintainerrPlexTokenFingerprint records the fingerprint of the Plex server
// token the Job just wrote and had Maintainerr prove. Call it only after that
// succeeded.
func (o *OwnedRecord) SetMaintainerrPlexTokenFingerprint(fingerprint string) {
	if o.Maintainerr == nil {
		o.Maintainerr = &MaintainerrOwned{}
	}
	o.Maintainerr.PlexTokenFingerprint = fingerprint
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

// QuiArrInstanceID returns the recorded id for one Arr instance's row in qui, or
// 0 when nothing is recorded.
func (o *OwnedRecord) QuiArrInstanceID(instance string) int {
	if o.Qui == nil {
		return 0
	}
	return o.Qui.ArrInstances[instance]
}

// SetQuiArrInstanceID records the id of the Arr row the Job created or adopted
// for one instance, keeping the key fingerprint recorded beside it.
func (o *OwnedRecord) SetQuiArrInstanceID(instance string, id int) {
	if o.Qui == nil {
		o.Qui = &QuiOwned{}
	}
	if o.Qui.ArrInstances == nil {
		o.Qui.ArrInstances = map[string]int{}
	}
	o.Qui.ArrInstances[instance] = id
}

// QuiArrInstanceFingerprint returns the fingerprint of the Arr API key one row
// in qui was last written and proven with, or "" when no successful write is on
// record.
func (o *OwnedRecord) QuiArrInstanceFingerprint(instance string) string {
	if o.Qui == nil {
		return ""
	}
	return o.Qui.ArrFingerprints[instance]
}

// SetQuiArrInstanceFingerprint records the fingerprint of the Arr key just written
// into a qui row and proved with qui's own test. Call it only after it succeeded.
func (o *OwnedRecord) SetQuiArrInstanceFingerprint(instance, fingerprint string) {
	if o.Qui == nil {
		o.Qui = &QuiOwned{}
	}
	if o.Qui.ArrFingerprints == nil {
		o.Qui.ArrFingerprints = map[string]string{}
	}
	o.Qui.ArrFingerprints[instance] = fingerprint
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

// SetAutobrrClientFingerprint records the fingerprint of the secret just written and
// proved with autobrr's own test. Call it only after it succeeded.
func (o *OwnedRecord) SetAutobrrClientFingerprint(name, fingerprint string) {
	if o.Autobrr == nil {
		o.Autobrr = &AutobrrOwned{}
	}
	if o.Autobrr.Fingerprints == nil {
		o.Autobrr.Fingerprints = map[string]string{}
	}
	o.Autobrr.Fingerprints[name] = fingerprint
}

// SeerrInitialized reports whether an earlier run of this Job set one Seerr instance up,
// by the instance's name.
func (o *OwnedRecord) SeerrInitialized(instance string) bool {
	return o.Seerr[instance].Initialized
}

// SetSeerrInitialized records that this run initialized one instance. Call it only after
// the app answered the initialize call.
func (o *OwnedRecord) SetSeerrInitialized(instance string) {
	if o.Seerr == nil {
		o.Seerr = map[string]SeerrOwned{}
	}
	rec := o.Seerr[instance]
	rec.Initialized = true
	o.Seerr[instance] = rec
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
