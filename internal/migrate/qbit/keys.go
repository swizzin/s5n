package qbit

// The sections a migrated conf's path keys live in, and the two field names
// the JSON files and the resume store share.
const (
	sectionBitTorrent  = "BitTorrent"
	sectionPreferences = "Preferences"
	sectionWebUI       = "WebUI"
	keySavePath        = "save_path"
	keyDownloadPath    = "download_path"
)

// valueKind is how a key's value holds its paths.
type valueKind int

const (
	// scalarPath is one absolute path.
	scalarPath valueKind = iota
	// pathList is a QSettings string list of absolute paths.
	pathList
	// legacyMap is a QVariant map, which QSettings writes as a binary
	// @Variant(…) blob; this tool does not decode it.
	legacyMap
)

// confKey is one key qBittorrent.conf stores an absolute path in, as the
// section and key QSettings writes: a settings key "A/B/C" is section [A],
// key "B\C".
type confKey struct {
	section string
	name    string
	kind    valueKind
	// live marks a key the app reads back to act on a file. A path no rule
	// covers there stops the migration. A GUI-history key only fills a dialog,
	// so a path no rule covers is left alone and never reported: it is the
	// same judgement as a History row in an Arr.
	live bool
}

// confKeys is every path-valued key in qBittorrent 5.3.0 (research:
// qbittorrent.md §Q3.2). The torrenting port is deliberately absent: the
// chart sets it from the environment, and a rewritten value here would only
// fight it.
var confKeys = []confKey{
	// The data contract: where torrents and their partial files go.
	{sectionBitTorrent, `Session\DefaultSavePath`, scalarPath, true},                // sessionimpl.cpp:610
	{sectionBitTorrent, `Session\TempPath`, scalarPath, true},                       // sessionimpl.cpp:611
	{sectionBitTorrent, `Session\TorrentBackupDirectory`, scalarPath, true},         // sessionimpl.cpp:578
	{sectionBitTorrent, `Session\FinishedTorrentBackupDirectory`, scalarPath, true}, // sessionimpl.cpp:579
	{sectionBitTorrent, `Session\IPFilter`, scalarPath, true},                       // sessionimpl.cpp:492
	{sectionPreferences, `Search\pythonExecutablePath`, scalarPath, true},           // preferences.cpp:1509
	{sectionWebUI, "RootFolder", scalarPath, true},                                  // preferences.cpp:1088
	{sectionWebUI, `HTTPS\CertificatePath`, scalarPath, true},                       // preferences.cpp:1049
	{sectionWebUI, `HTTPS\KeyPath`, scalarPath, true},                               // preferences.cpp:1062
	{"FileLogger", "Path", scalarPath, true},                                        // application.cpp:307
	{"AutoRun", `OnTorrentAdded\Program`, scalarPath, true},                         // preferences.cpp:1258
	{"AutoRun", "program", scalarPath, true},                                        // preferences.cpp:1284

	// A conf upgraded through older versions can still carry these; the app
	// renames them at startup, and rewriting them is what makes that rename
	// carry the box's paths (upgrade.cpp:302,307,318,340).
	{sectionPreferences, `Downloads\SavePath`, scalarPath, true},
	{sectionPreferences, `Downloads\TempPath`, scalarPath, true},
	{sectionPreferences, `IPFilter\File`, scalarPath, true},
	{sectionPreferences, `Downloads\FinishedTorrentExportDir`, scalarPath, true},
	{sectionPreferences, `Downloads\FinishedTorrentExportDirectory`, scalarPath, true},
	{sectionPreferences, `Downloads\TorrentExportDirectory`, scalarPath, true},

	// GUI history: the last directory a dialog opened in, and the list of
	// save paths it offers. Cosmetic, so never reported uncovered.
	{sectionPreferences, `General\CustomUIThemePath`, scalarPath, false},  // preferences.cpp:124
	{sectionPreferences, `Downloads\ScanDirsLastPath`, scalarPath, false}, // preferences.cpp:519
	{"MainWindow", "LastDir", scalarPath, false},                          // preferences.cpp:1698
	{"TorrentImport", "LastContentDir", scalarPath, false},                // preferences.cpp:1880
	{"AddNewTorrentDialog", "SavePathHistory", pathList, false},           // addnewtorrentdialog.cpp:79
	{"AddNewTorrentDialog", "DownloadPathHistory", pathList, false},       // addnewtorrentdialog.cpp:80

	// The two maps qBittorrent 5 converts on its first start: categories into
	// categories.json (sessionimpl.cpp:5889-5901) and watched folders into
	// watched_folders.json (torrentfileswatcher.cpp:207,239). Both are read only
	// when the JSON file beside them is missing, so they are live when it is.
	{sectionBitTorrent, `Session\Categories`, legacyMap, true},
	{sectionPreferences, `Downloads\ScanDirsV2`, legacyMap, true},
}

// categoriesKey and watchedKey are the two legacy maps above, named for the
// code that decides whether a JSON file supersedes them.
const (
	categoriesKey = `Session\Categories`
	watchedKey    = `Downloads\ScanDirsV2`
)
