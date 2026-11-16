package qbit

import (
	"encoding/json"
	"fmt"
)

// object is a JSON object kept as raw values, so a field this pass does not
// change is written back exactly as the app stored it, and a number keeps its
// own text rather than becoming a float.
type object map[string]json.RawMessage

func parseObject(raw []byte) (object, error) {
	var o object
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("is not JSON: %w", err)
	}
	if o == nil {
		return nil, fmt.Errorf("is not a JSON object")
	}
	return o, nil
}

// str reads a string field.
func (o object) str(key string) (string, bool) {
	var v string
	if err := json.Unmarshal(o[key], &v); err != nil {
		return "", false
	}
	return v, true
}

// child reads a nested object.
func (o object) child(key string) (object, bool) {
	raw, ok := o[key]
	if !ok {
		return nil, false
	}
	nested, err := parseObject(raw)
	if err != nil {
		return nil, false
	}
	return nested, true
}

// marshal renders the object.
func (o object) marshal() ([]byte, error) { return json.Marshal(o) }

func mustJSON(v string) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("qbit: encoding a string: " + err.Error())
	}
	return data
}

// jsonPath rewrites one path-valued field of a JSON object and reports whether
// it changed. The field is looked up by name: a path is one value among
// others, and replacing text would also hit a category name or a rule that
// happens to look like one.
func (a *analysis) jsonPath(file, where string, o object, key string) bool {
	stored, ok := o.str(key)
	if !ok || stored == "" {
		// An empty value means "use the default", which follows whatever the
		// app is given.
		return false
	}
	out, covered := a.path(file, where, stored, true)
	if !covered || out == stored {
		return false
	}
	o[key] = mustJSON(out)
	return true
}

// categories rewrites the save and download path of every category.
// qBittorrent's categories are what an Arr's download client names, so a
// category name never changes: only the directories behind it do
// (research: qbittorrent.md §Q3.3).
func (a *analysis) categories() error {
	raw, mode, ok, err := readJSON(a.lay.categories)
	if err != nil || !ok {
		return err
	}
	top, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", categoriesRel, err)
	}
	changedAny := false
	for name, value := range top {
		opts, err := parseObject(value)
		if err != nil {
			return fmt.Errorf("%s: category %q %w", categoriesRel, name, err)
		}
		changed := false
		for _, key := range []string{keySavePath, keyDownloadPath} {
			if a.jsonPath(categoriesRel, "categories."+key, opts, key) {
				changed = true
			}
		}
		if !changed {
			continue
		}
		blob, err := opts.marshal()
		if err != nil {
			return fmt.Errorf("%s: category %q: %w", categoriesRel, name, err)
		}
		top[name] = blob
		changedAny = true
	}
	if !changedAny {
		return nil
	}
	blob, err := top.marshal()
	if err != nil {
		return fmt.Errorf("%s: %w", categoriesRel, err)
	}
	a.writeFile(categoriesRel, blob, mode)
	return nil
}

// watchedFolders rewrites the watched folder paths, which are the JSON keys
// themselves, and the save and download path of the torrent parameters each
// one carries (torrentfileswatcher.cpp:64-67,71-84,196-206).
func (a *analysis) watchedFolders() error {
	raw, mode, ok, err := readJSON(a.lay.watched)
	if err != nil || !ok {
		return err
	}
	top, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", watchedRel, err)
	}
	out := make(object, len(top))
	changedAny := false
	for folder, value := range top {
		key, blob, changed, err := a.watchedFolder(folder, value)
		if err != nil {
			return err
		}
		if _, taken := out[key]; taken && key != folder {
			// Two watched folders landing on one directory would silently drop
			// one of them; the plan maps directories, so this only happens when
			// it maps two to the same place.
			return fmt.Errorf("%s: watched folders %q and another both rewrite to %s", watchedRel, folder, key)
		}
		out[key] = blob
		changedAny = changedAny || changed
	}
	if !changedAny {
		return nil
	}
	blob, err := out.marshal()
	if err != nil {
		return fmt.Errorf("%s: %w", watchedRel, err)
	}
	a.writeFile(watchedRel, blob, mode)
	return nil
}

// watchedFolder rewrites one watched folder's key and the paths inside it.
func (a *analysis) watchedFolder(folder string, value json.RawMessage) (string, json.RawMessage, bool, error) {
	opts, err := parseObject(value)
	if err != nil {
		return "", nil, false, fmt.Errorf("%s: watched folder %q %w", watchedRel, folder, err)
	}
	key, covered := a.path(watchedRel, "watched_folders.folder", folder, true)
	if !covered {
		key = folder
	}
	changed := covered && key != folder
	if params, ok := opts.child("add_torrent_params"); ok {
		paramsChanged := false
		for _, field := range []string{keySavePath, keyDownloadPath} {
			if a.jsonPath(watchedRel, "watched_folders.add_torrent_params."+field, params, field) {
				paramsChanged = true
			}
		}
		if paramsChanged {
			blob, err := params.marshal()
			if err != nil {
				return "", nil, false, fmt.Errorf("%s: watched folder %q: %w", watchedRel, folder, err)
			}
			opts["add_torrent_params"] = blob
			changed = true
		}
	}
	if !changed {
		return key, value, false, nil
	}
	blob, err := opts.marshal()
	if err != nil {
		return "", nil, false, fmt.Errorf("%s: watched folder %q: %w", watchedRel, folder, err)
	}
	return key, blob, true, nil
}

// downloadRules rewrites the RSS download rules' paths. A rule keeps its name;
// its save path sits both at the top (the deprecated spelling) and inside the
// torrent parameters it hands to the client
// (rss_autodownloadrule.cpp:117-120,470-486,575-593).
func (a *analysis) downloadRules() error {
	raw, mode, ok, err := readJSON(a.lay.rules)
	if err != nil || !ok {
		return err
	}
	top, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", rulesRel, err)
	}
	changedAny := false
	for name, value := range top {
		blob, changed, err := a.downloadRule(name, value)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		top[name] = blob
		changedAny = true
	}
	if !changedAny {
		return nil
	}
	blob, err := top.marshal()
	if err != nil {
		return fmt.Errorf("%s: %w", rulesRel, err)
	}
	a.writeFile(rulesRel, blob, mode)
	return nil
}

// downloadRule rewrites one rule's paths, at the rule's top level and inside
// its torrent parameters.
func (a *analysis) downloadRule(name string, value json.RawMessage) (json.RawMessage, bool, error) {
	rule, err := parseObject(value)
	if err != nil {
		return nil, false, fmt.Errorf("%s: rule %q %w", rulesRel, name, err)
	}
	changed := false
	for _, key := range []string{"savePath", keySavePath} {
		if a.jsonPath(rulesRel, "download_rules."+key, rule, key) {
			changed = true
		}
	}
	if params, ok := rule.child("torrentParams"); ok {
		paramsChanged := false
		for _, field := range []string{keySavePath, keyDownloadPath} {
			if a.jsonPath(rulesRel, "download_rules.torrentParams."+field, params, field) {
				paramsChanged = true
			}
		}
		if paramsChanged {
			blob, err := params.marshal()
			if err != nil {
				return nil, false, fmt.Errorf("%s: rule %q: %w", rulesRel, name, err)
			}
			rule["torrentParams"] = blob
			changed = true
		}
	}
	if !changed {
		return value, false, nil
	}
	blob, err := rule.marshal()
	if err != nil {
		return nil, false, fmt.Errorf("%s: rule %q: %w", rulesRel, name, err)
	}
	return blob, true, nil
}
