package plex

import (
	"os"
	"path/filepath"
	"strings"
)

// ServerToken reads the PlexOnlineToken attribute of Preferences.xml: the token a
// claimed server is signed in to plex.tv with, and what the Plex API takes as
// X-Plex-Token, which is why wiring hands it to Tautulli and Maintainerr. Nothing
// else on the host holds it, so without this every Plex-dependent app stays
// pending. dir is a staged copy or the live config, only read, through the same
// scanner the rewrite uses. An absent or empty token is "no token", not an error:
// an install never signed in has an empty attribute.
func ServerToken(dir string) (string, bool, error) {
	token, _, ok, err := ServerTokenFile(dir)
	return token, ok, err
}

// ServerTokenFile is ServerToken with the file it read, for a caller naming a
// Preferences.xml it could not parse; file is "" when there is no Plex home.
func ServerTokenFile(dir string) (token, file string, ok bool, err error) {
	support, err := supportDirIn(dir)
	if err != nil {
		// No Plex home: the pre-install pass reads a copy before Plex's own check
		// runs, so this is "no token", not a second failure.
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
