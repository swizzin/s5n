package snapshot

import "errors"

// ImageRef is the image one app ran, as the chart pinned it. Both handles are kept
// because both are facts: the tag says which release, the digest says which bytes,
// and a later hold compares the digest while printing the tag a person recognises.
type ImageRef struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

// String is the reference as a registry spells it: repository[:tag][@digest].
func (r ImageRef) String() string {
	out := r.Repository
	if r.Tag != "" {
		out += ":" + r.Tag
	}
	if r.Digest != "" {
		out += "@" + r.Digest
	}
	return out
}

// Label is the short handle box says in a sentence: the tag when the release named
// one, so the operator reads "sonarr 4.0.14" rather than a digest, and the digest
// when it did not.
func (r ImageRef) Label() string {
	if r.Tag != "" {
		return r.Tag
	}
	return r.Digest
}

// IsZero reports that it names no image at all: a snapshot taken before box
// recorded images holds one, and the caller decides what that means.
func (r ImageRef) IsZero() bool {
	return r.Repository == "" && r.Tag == "" && r.Digest == ""
}

// Images are the two references one update step knows about an app: what it ran when
// the copy was taken, and what the step moves it to. A snapshot naming only one
// cannot be rolled back: the first is what the hold keeps, the second what it avoids.
type Images struct {
	Ran     ImageRef
	MovedTo ImageRef
}

// checkImage refuses a reference that would not name a runnable image. These are
// read back weeks later, so half a reference is a refusal rather than a guess at
// which half was meant.
func checkImage(r ImageRef) error {
	switch {
	case r.Repository == "":
		return errors.New("has no repository")
	case r.Tag == "" && r.Digest == "":
		return errors.New("has neither a tag nor a digest")
	}
	return nil
}
