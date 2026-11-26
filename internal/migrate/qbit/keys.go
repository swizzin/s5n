package qbit

// The sections path keys live in, and the two field names JSON and the resume
// store share.
const (
	sectionBitTorrent  = "BitTorrent"
	sectionPreferences = "Preferences"
	sectionWebUI       = "WebUI"
	sectionRSS         = "RSS"
	keySavePath        = "save_path"
	keyDownloadPath    = "download_path"

	// keySessionPort is qBittorrent 5's peer port; keyLegacyPortRangeMin is the
	// spelling every version keeps for it, and the session key wins when both exist.
	keySessionPort        = `Session\Port`
	keyLegacyPortRangeMin = `Connection\PortRangeMin`
)

// valueKind is how a key's value holds its paths.
type valueKind int

const (
	// scalarPath is one absolute path.
	scalarPath valueKind = iota
	// pathList is a QSettings string list of absolute paths.
	pathList
	// legacyMap is a binary @Variant(…) blob this tool does not decode.
	legacyMap
)

// confKey is one key conf stores an absolute path in: "A/B/C" is section [A], key
// "B\C".
type confKey struct {
	section string
	name    string
	kind    valueKind
	// live marks a key the app acts on, where an uncovered path stops the
	// migration; a GUI-history key only fills a dialog, so it is left alone.
	live bool
}

// confKeys is every path-valued key in qBittorrent 5.3.0 (research:
// qbittorrent.md §Q3.2). The torrenting port is absent: the chart sets it from the
// environment.
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

	// An upgraded conf can still carry these; rewriting them makes the app's
	// startup rename carry the box's paths (upgrade.cpp:302,307,318,340).
	{sectionPreferences, `Downloads\SavePath`, scalarPath, true},
	{sectionPreferences, `Downloads\TempPath`, scalarPath, true},
	{sectionPreferences, `IPFilter\File`, scalarPath, true},
	{sectionPreferences, `Downloads\FinishedTorrentExportDir`, scalarPath, true},
	{sectionPreferences, `Downloads\FinishedTorrentExportDirectory`, scalarPath, true},
	{sectionPreferences, `Downloads\TorrentExportDirectory`, scalarPath, true},

	// GUI history, cosmetic: never reported uncovered.
	{sectionPreferences, `General\CustomUIThemePath`, scalarPath, false},  // preferences.cpp:124
	{sectionPreferences, `Downloads\ScanDirsLastPath`, scalarPath, false}, // preferences.cpp:519
	{"MainWindow", "LastDir", scalarPath, false},                          // preferences.cpp:1698
	{"TorrentImport", "LastContentDir", scalarPath, false},                // preferences.cpp:1880
	{"AddNewTorrentDialog", "SavePathHistory", pathList, false},           // addnewtorrentdialog.cpp:79
	{"AddNewTorrentDialog", "DownloadPathHistory", pathList, false},       // addnewtorrentdialog.cpp:80

	// The two maps qBittorrent 5 converts on first start (sessionimpl.cpp:5889-5901,
	// torrentfileswatcher.cpp:207,239), read only when the JSON beside them is
	// missing, so live when it is.
	{sectionBitTorrent, `Session\Categories`, legacyMap, true},
	{sectionPreferences, `Downloads\ScanDirsV2`, legacyMap, true},
}

// categoriesKey and watchedKey are the two legacy maps, named for the code
// deciding whether a JSON file supersedes them.
const (
	categoriesKey = `Session\Categories`
	watchedKey    = `Downloads\ScanDirsV2`
)

// rehearsalConfKeys are the two settings a rehearsal writes: AddTorrentStopped
// lands everything autobrr and the Arrs push stopped, and EnableProcessing is
// RSS's own switch, which no other key implies.
var rehearsalConfKeys = []struct {
	section, key, value string
}{
	{sectionBitTorrent, `Session\AddTorrentStopped`, "true"},
	{sectionRSS, `AutoDownloader\EnableProcessing`, "false"},
}
