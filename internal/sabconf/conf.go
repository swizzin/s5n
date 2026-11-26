package sabconf

import (
	"fmt"
	"strings"
)

// Settings is everything the chart owns in sabnzbd.ini before the app starts.
//
// SABnzbd reads the file once, at start-up, and these values cannot be set
// through the API in time: a wrong path is a copy instead of a hardlink,
// host_whitelist is checked on every request, and verify_xff_header decides
// whether the Gateway's X-Forwarded-For makes a request external (a 403).
// url_base is read when the web app is mounted, and the key is what a request
// would have to present to change anything.
type Settings struct {
	// URLBase is the prefix the chart serves SABnzbd under.
	URLBase string
	// CompleteDir is where a finished download is left for an Arr to import,
	// and DownloadDir where it is unpacked. Both are on the data claim, which is
	// what makes the import a hardlink.
	CompleteDir string
	DownloadDir string
	// APIKey is the release Secret's key. The image's entrypoint rewrites it
	// from SABNZBD__API_KEY on every start; sealing it means a file that
	// predates that is still the key the wiring Job holds.
	APIKey string
	// VerifyXFF is misc.verify_xff_header, true by default. The Gateway sends
	// X-Forwarded-For, so with it on every request through the edge is
	// external and answered 403. Off trusts the proxy that alone reaches this
	// pod.
	VerifyXFF bool
}

// The section and keys this package owns, as SABnzbd spells them
// (sabnzbd/cfg.py at 5.1.3 and constants.py's DEF_DOWNLOAD_DIR/DEF_COMPLETE_DIR,
// whose defaults this replaces).
const (
	sectionMisc    = "misc"
	keyURLBase     = "url_base"
	keyCompleteDir = "complete_dir"
	keyDownloadDir = "download_dir"
	keyAPIKey      = "api_key"
	keyVerifyXFF   = "verify_xff_header"
)

// Seal ensures sabnzbd.ini holds the settings the chart owns, and reports
// whether anything changed.
//
// An empty file is a first start without the image's defaults: [misc] is created
// with these keys and SABnzbd fills the rest. A section or key declared twice is
// refused: ConfigObj reads the last one, and writing the first would leave the
// value the app uses.
func Seal(existing []byte, s Settings) (out []byte, changed bool, err error) {
	f, err := Parse(existing)
	if err != nil {
		return existing, false, err
	}
	if !f.HasSection(sectionMisc) {
		if len(f.Keys(sectionMisc)) > 0 || len(strings.TrimSpace(string(existing))) > 0 {
			return existing, false, fmt.Errorf("has no [%s] section; the file is not the shape SABnzbd writes", sectionMisc)
		}
	}
	want := []struct{ key, value string }{
		{keyURLBase, s.URLBase},
		{keyCompleteDir, s.CompleteDir},
		{keyDownloadDir, s.DownloadDir},
		{keyAPIKey, s.APIKey},
		{keyVerifyXFF, boolValue(s.VerifyXFF)},
	}
	differs := false
	for _, w := range want {
		if got, ok := f.Get(sectionMisc, w.key); !ok || got != w.value {
			differs = true
			break
		}
	}
	if !differs {
		return existing, false, nil
	}
	for _, w := range want {
		if err := f.Set(sectionMisc, w.key, w.value); err != nil {
			return existing, false, err
		}
	}
	return f.Bytes(), true, nil
}

func boolValue(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
