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
