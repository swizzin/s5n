package plex

import (
	"os"
	"path/filepath"
	"strings"
)

// ServerToken reads the Plex server token out of a Plex install: the
// PlexOnlineToken attribute of Preferences.xml.
//
// It is the token a claimed server is signed in to plex.tv with, and the same
// value the Plex API takes as X-Plex-Token — which is why the wiring Job hands
// it to Tautulli for its PMS link and to Maintainerr for its Plex connection.
// A migrated box's Plex has the account's token in its own settings file, and
// nothing else on the host does: the release Secret's plex-token is otherwise
// only ever supplied by hand, so a migration that does not read it here leaves
// every Plex-dependent app pending on a box that can answer.
//
// dir is the app's config directory: a staged copy for a migration
// (rewrite.Input.Dir), and the live one for `box apply`, which only ever reads
// it. Either way the file is Plex's own: the attribute is located by the same
// scanner the path rewrite uses, so the two read one file one way and neither
// reformats it.
//
// An absent, empty or whitespace-only attribute is "no token" and not an error:
// an install that was never signed in has an empty PlexOnlineToken, and that is
// the state the wiring reports pending for.
func ServerToken(dir string) (string, bool, error) {
	token, _, ok, err := ServerTokenFile(dir)
	return token, ok, err
}

// ServerTokenFile is ServerToken with the file it read, so a caller that has to
// name a Preferences.xml it could not parse has the path to name. file is ""
// when dir holds no Plex home at all, which is the state of a config directory
// Plex has not written yet.
func ServerTokenFile(dir string) (token, file string, ok bool, err error) {
	support, err := supportDirIn(dir)
	if err != nil {
		// No Plex home in this copy. The pre-install pass reads a copy before
		// Plex's own check has run, and that check is what refuses a config
		// directory that holds neither Preferences.xml nor the database; there
		// is nothing to seed from one, so it is "no token" rather than a second
		// failure of a pass that cannot check Plex at all.
		return "", "", false, nil
	}
	path := filepath.Join(dir, filepath.FromSlash(support), prefsName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", false, nil
		}
		return "", path, false, err
	}
	f, err := parsePrefs(raw)
	if err != nil {
		return "", path, false, err
	}
	a, ok := f.get("PlexOnlineToken")
	if !ok {
		return "", path, false, nil
	}
	token = strings.TrimSpace(a.value)
	if token == "" {
		return "", path, false, nil
	}
	return token, path, true, nil
}
