package sabconf

import (
	"fmt"
	"strings"
)

// Settings is everything the chart owns in sabnzbd.ini before the app starts.
//
// SABnzbd keeps its whole configuration in one ConfigObj INI file, sabnzbd.ini,
// and reads it once at start-up. Three of these cannot be set through the API:
// the download paths are what the first job uses, host_whitelist is checked on
// every request, and verify_xff_header decides whether the Gateway's
// X-Forwarded-For makes a request "external" -- each one is a 403 or a copy
// instead of a hardlink if it is wrong. url_base is read when the web app is
// mounted, so the API itself lives under the prefix a running process never
// changes. The key is what a request would have to present to change anything
// at all.
type Settings struct {
	// URLBase is the prefix the chart serves SABnzbd under ("/sabnzbd").
	URLBase string
	// CompleteDir is where a finished download is left for an Arr to import,
	// and DownloadDir where it is unpacked while it runs. Both are on the data
	// claim, which is what makes the import a hardlink.
	CompleteDir string
	DownloadDir string
	// APIKey is the release Secret's key. The image's own entrypoint rewrites
	// it from SABNZBD__API_KEY on every start; sealing it here means a file
	// that predates that (or a run without the variable) is still the key the
	// wiring Job holds.
	APIKey string
	// VerifyXFF is SABnzbd's misc.verify_xff_header. It is true by default, and
	// the Gateway sends X-Forwarded-For carrying the person's real address, so
	// with it on every request through the edge is classified as external and
	// answered 403. Off means SAB trusts the proxy in front of it, which is the
	// only thing that reaches this pod.
	VerifyXFF bool
}

// The section and keys this package owns, as SABnzbd spells them
// (sabnzbd/cfg.py at 5.1.3: OptionStr/OptionBool/OptionNumber names under
// "misc", and constants.py's DEF_DOWNLOAD_DIR/DEF_COMPLETE_DIR for the
// defaults this replaces).
const (
	sectionMisc    = "misc"
	keyURLBase     = "url_base"
	keyCompleteDir = "complete_dir"
	keyDownloadDir = "download_dir"
	keyAPIKey      = "api_key"
	keyVerifyXFF   = "verify_xff_header"
)

// Seal ensures sabnzbd.ini holds the settings the chart owns, and returns
// whether anything had to change so a second start writes nothing.
//
// An empty file is a first start without the image's defaults: the [misc]
// section is created with these keys alone, and SABnzbd fills every other
// setting from its own defaults. A file with a section declared twice, or the
// same key twice in [misc], is refused rather than guessed at: ConfigObj reads
// the last one, and writing the first would leave the value the app uses.
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
