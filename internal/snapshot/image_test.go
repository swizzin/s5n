package snapshot

import (
	"strings"
	"testing"
)

// A reference is spelled the way a registry spells it, and its short label is the
// handle a person reads. A release may pin a tag, a digest or both.
func TestImageRefSpelling(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name       string
		ref        ImageRef
		wantString string
		wantLabel  string
	}{
		{
			name:       "tag and digest",
			ref:        ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.14", Digest: digest},
			wantString: "registry.example.com/media/sonarr:4.0.14@" + digest,
			wantLabel:  "4.0.14",
		},
		{
			name:       "digest only",
			ref:        ImageRef{Repository: "registry.example.com/media/sonarr", Digest: digest},
			wantString: "registry.example.com/media/sonarr@" + digest,
			wantLabel:  digest,
		},
		{
			name:       "tag only",
			ref:        ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.14"},
			wantString: "registry.example.com/media/sonarr:4.0.14",
			wantLabel:  "4.0.14",
		},
		{
			// Nothing to label: what a snapshot taken before box recorded images reads as.
			// Callers test IsZero instead of printing this.
			name:       "zero",
			wantString: "",
			wantLabel:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.String(); got != tc.wantString {
				t.Errorf("String() = %q, want %q", got, tc.wantString)
			}
			if got := tc.ref.Label(); got != tc.wantLabel {
				t.Errorf("Label() = %q, want %q", got, tc.wantLabel)
			}
			if zero := tc.ref.IsZero(); zero != (tc.ref == ImageRef{}) {
				t.Errorf("IsZero() = %v for %+v", zero, tc.ref)
			}
		})
	}
}

// What counts as a reference box can hold an app at: a repository and one of the
// two handles. A repository with neither is as useless as an empty one.
func TestCheckImage(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		ref     ImageRef
		wantErr string
	}{
		{name: "tag and digest", ref: ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.14", Digest: digest}},
		{name: "digest only", ref: ImageRef{Repository: "registry.example.com/media/sonarr", Digest: digest}},
		{name: "tag only", ref: ImageRef{Repository: "registry.example.com/media/sonarr", Tag: "4.0.14"}},
		{name: "zero", wantErr: "has no repository"},
		{name: "no repository", ref: ImageRef{Tag: "4.0.14"}, wantErr: "has no repository"},
		{name: "repository only", ref: ImageRef{Repository: "registry.example.com/media/sonarr"}, wantErr: "has neither a tag nor a digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkImage(tc.ref)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("%+v refused: %v", tc.ref, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%+v accepted", tc.ref)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
