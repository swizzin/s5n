// Package fastresume rewrites the paths qBittorrent stored in a download
// directory's resume files, ahead of migrating it onto box.
//
// qBittorrent keeps one <info-hash>.fastresume per torrent beside the torrent's
// own <info-hash>.torrent, and the resume file is where the paths the app saw
// are recorded. A migrated qBittorrent that kept the source's paths looks for
// its files under directories the new host does not have, finds none of them,
// and sets about downloading everything again — from zero, with the copies it
// already made sitting right there. With them rewritten it carries on seeding
// where it was.
//
// The keys that hold a path are fixed by what libtorrent and qBittorrent read,
// and they are the same in libtorrent 1.2 and 2.0:
//
//   - save_path: libtorrent's own (write_resume_data.cpp:116 and :123,
//     read_resume_data.cpp:171 and :287). qBittorrent refuses to load a resume
//     file whose save_path is empty (bencoderesumedatastorage.cpp:355-358).
//   - qBt-savePath and qBt-downloadPath: qBittorrent's per-torrent overrides
//     (bencoderesumedatastorage.cpp:260-266 read them, :479-480 write them only
//     when automatic torrent management is off). An absent qBt-savePath means
//     the torrent follows the app's download path, so an empty or missing value
//     is left exactly as it is.
//   - mapped_files: a list of renamed files, normally relative to save_path
//     (write_resume_data.cpp:217, read_resume_data.cpp:179), so only an
//     absolute entry — one another client wrote — is rewritten.
//
// Nothing else in the file is a filesystem path: the rest is integers,
// bitfields, peer lists, trackers, URLs and text.
package fastresume

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/migrate/bencode"
	"github.com/swizzin/s5n/internal/migrate/plan"
)

// resumeExt is the suffix qBittorrent gives a torrent's resume data
// (bencoderesumedatastorage.cpp:483).
const resumeExt = ".fastresume"

// dirPerm is what a directory this package creates gets: the same mode `box
// init` gives the config root, so the app's own user can write into it. Handing
// the tree to puid:pgid is the caller's job, which is the same deal
// atomicfile.Write makes about its parent directory.
const dirPerm fs.FileMode = 0o775

// pathKeys are the resume keys libtorrent and qBittorrent read as an absolute
// filesystem path. part_file_dir is libtorrent 2's own: qBittorrent leaves it
// empty, but a resume file another client wrote can carry one, and an empty
// value is left exactly as it is (libtorrent@v2.1.2:src/write_resume_data.cpp:109,
// src/read_resume_data.cpp:288).
var pathKeys = []string{"save_path", "qBt-savePath", "qBt-downloadPath", "part_file_dir"}

// Table is the plan's per-app path rules: the app whose files are being
// rewritten, and the path it stored, mapped to the one it will store. The
// source install's paths differ per app, so a table is never applied to another
// app's files.
type Table interface {
	Rewrite(app, stored string) (string, bool)
}

// Report is what a run did, or would do with DryRun.
type Report struct {
	Files     int            // every file the source directory held
	Resume    int            // the *.fastresume files among them
	Copied    int            // files copied through without being decoded
	Rewritten int            // resume files whose bytes changed
	Unchanged int            // resume files written through as they were
	Stopped   int            // torrents this run marked stopped (Stop)
	Rewrites  map[string]int // path rewrites per resume key
}

// Option varies a run.
type Option func(*config)

type config struct {
	dryRun bool
	// stop marks every torrent stopped as well as rewriting its paths: what a
	// rehearsal does so a copy cannot seed, recheck or fetch metadata.
	stop bool
}

// DryRun computes the report and writes nothing, not even the destination
// directory, which is ignored.
func DryRun() Option {
	return func(c *config) { c.dryRun = true }
}

// Stop marks every torrent in the run stopped: paused=1 and auto_managed=0,
// which qBittorrent 5.3 reads as a stopped torrent
// (bencoderesumedatastorage.cpp:360). It is the one app-specific half of a
// rehearsal's lock: with the data absent or shared, a running torrent would
// recheck to 0% and churn the disk, and a seeding one would announce.
//
// It is the same pass as the path rewrite, so a resume file is decoded, both
// edits made and it is written once; every key this package does not name comes
// back byte for byte.
func Stop() Option {
	return func(c *config) { c.stop = true }
}

// RewriteDir copies src into dst, rewriting the paths in every resume file
// through app's table.
//
// src is only ever read. It belongs to the source install, which keeps running
// until the operator tears it down, and it is the rollback if the migration goes
// wrong; dst must not exist or must be empty, because a destination holding
// anything else could not be told apart from a finished copy.
//
// Every file is read and decoded before the first write, so a path no rule
// covers refuses the whole run and leaves dst as it found it. The error is a
// *plan.UncoveredError, which lists every gap at once: a torrent client's resume
// directory is thousands of files and the operator should not have to discover
// them one run at a time.
func RewriteDir(src, dst, app string, table Table, opts ...Option) (Report, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	rep := Report{Rewrites: make(map[string]int)}

	if !cfg.dryRun {
		if err := checkDst(src, dst); err != nil {
			return rep, err
		}
	}
	files, gaps, err := read(src, app, table, &rep, cfg)
	if err != nil {
		return rep, err
	}
	if len(gaps) > 0 {
		return rep, &plan.UncoveredError{Paths: gaps}
	}
	if cfg.dryRun {
		return rep, nil
	}

	written, created, err := writeAll(dst, files)
	if err != nil {
		undo(dst, written, created)
		return rep, err
	}
	return rep, nil
}

// outFile is one file pass two writes: the source's bytes, or a resume file's
// rewritten ones, under the mode the source file had.
type outFile struct {
	name    string
	data    []byte
	mode    fs.FileMode
	changed bool // the bytes differ from what the source held
}

// resume is one resume file's rewrite: the bytes to write, what changed, and
// the paths no rule covers. A file with gaps has no bytes: the run refuses
// rather than write half a rewrite.
type resume struct {
	data     []byte
	rewrites map[string]int
	gaps     []plan.Uncovered
}

// read is pass one: every file in src is read, and every resume file decoded
// and rewritten in memory. Gaps come back rather than an error, because the
// caller reports them all together once the directory has been read.
func read(src, app string, table Table, rep *Report, cfg config) ([]outFile, []plan.Uncovered, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", src, err)
	}
	files := make([]outFile, 0, len(entries))
	var gaps []plan.Uncovered
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			// qBittorrent writes this directory flat. A subdirectory is
			// something else's data, and copying it is not this package's job.
			return nil, nil, fmt.Errorf("%s: is a directory; a resume directory holds files only", filepath.Join(src, name))
		}
		info, err := e.Info()
		if err != nil {
			return nil, nil, fmt.Errorf("stat %s: %w", filepath.Join(src, name), err)
		}
		raw, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", filepath.Join(src, name), err)
		}
		rep.Files++
		out := outFile{name: name, data: raw, mode: info.Mode().Perm()}
		if strings.HasSuffix(name, resumeExt) {
			rep.Resume++
			data, g, err := rewriteFile(name, raw, app, table, rep)
			if err != nil {
				return nil, nil, err
			}
			gaps = append(gaps, g...)
			if len(g) == 0 {
				data, err = stoppedData(name, data, cfg, rep)
				if err != nil {
					return nil, nil, err
				}
				out.data = data
				out.changed = !bytes.Equal(data, raw)
				if out.changed {
					rep.Rewritten++
				} else {
					rep.Unchanged++
				}
			}
		} else {
			// The torrent's metadata, and anything else the client keeps
			// beside it, is copied byte for byte: its paths are relative to
			// save_path and stay right once save_path is.
			rep.Copied++
		}
		files = append(files, out)
	}
	return files, gaps, nil
}

// stoppedData marks one decoded resume file stopped when this run is a
// rehearsal, counting it for the report. A run that is not a rehearsal returns
// the bytes it was given: the stop is the snapshot's mode, not a setting.
func stoppedData(name string, data []byte, cfg config, rep *Report) ([]byte, error) {
	if !cfg.stop {
		return data, nil
	}
	out, stopped, err := stopBytes(name, data)
	if err != nil {
		return nil, err
	}
	if stopped {
		rep.Stopped++
	}
	return out, nil
}

// rewriteFile decodes one resume file and returns the bytes pass two writes.
// The gaps it finds are returned rather than an error: a file with two unmapped
// paths is worth reporting once, with the rest of the directory.
func rewriteFile(name string, raw []byte, app string, table Table, rep *Report) ([]byte, []plan.Uncovered, error) {
	res, err := rewriteResume(name, raw, app, table)
	if err != nil {
		return nil, nil, err
	}
	if len(res.gaps) > 0 {
		return nil, res.gaps, nil
	}
	for key, n := range res.rewrites {
		rep.Rewrites[key] += n
	}
	return res.data, nil, nil
}

// rewriteResume is one resume file's rewrite, decoded in memory: the bytes it
// would become, the keys it changed, and every path no rule covers. A file
// with gaps has no bytes, because the run refuses rather than write half a
// rewrite.
func rewriteResume(name string, raw []byte, app string, table Table) (resume, error) {
	v, err := bencode.Decode(raw)
	if err != nil {
		return resume{}, fmt.Errorf("%s: %w", name, err)
	}
	if v.Kind != bencode.Dict {
		return resume{}, fmt.Errorf("%s: is not a bencoded dictionary", name)
	}
	r := rewriter{app: app, file: name, table: table, rewrites: make(map[string]int)}
	r.paths(&v)
	res := resume{rewrites: r.rewrites, gaps: r.gaps}
	if len(r.gaps) > 0 {
		return res, nil
	}
	out, err := bencode.Encode(v)
	if err != nil {
		return resume{}, fmt.Errorf("%s: %w", name, err)
	}
	res.data = out
	return res, nil
}

// Scan reads a resume directory and reports what a rewrite would do, writing
// nothing: it is the check pass, on the staged copy the app will see.
//
// Gaps come back as values rather than as a *plan.UncoveredError, because a
// rewriter collects them with every other gap it finds before refusing once.
func Scan(dir, app string, table Table, opts ...Option) (Report, []plan.Uncovered, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	rep := Report{Rewrites: make(map[string]int)}
	_, gaps, err := read(dir, app, table, &rep, cfg)
	return rep, gaps, err
}

// Rewrite rewrites a resume directory in place: every file Scan decoded and
// changed is written through atomicfile, and nothing else is touched. Files
// that need no change keep their bytes and their mtime, so a second run writes
// nothing at all.
func Rewrite(dir, app string, table Table, opts ...Option) (Report, []plan.Uncovered, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	rep := Report{Rewrites: make(map[string]int)}
	files, gaps, err := read(dir, app, table, &rep, cfg)
	if err != nil || len(gaps) > 0 {
		return rep, gaps, err
	}
	for _, f := range files {
		if !f.changed {
			continue
		}
		if err := atomicfile.Write(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return rep, nil, err
		}
	}
	return rep, nil, nil
}

// Blob rewrites one bencoded resume blob in memory, as the SQLite resume store
// keeps it. name labels the blob where a gap is reported, as a resume file's
// name does; changed is false when the bytes came back the same.
func Blob(name string, raw []byte, app string, table Table, opts ...Option) (data []byte, rewrites map[string]int, gaps []plan.Uncovered, stopped bool, err error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	res, err := rewriteResume(name, raw, app, table)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if len(res.gaps) > 0 || !cfg.stop {
		return res.data, res.rewrites, res.gaps, false, nil
	}
	data, stopped, err = stopBytes(name, res.data)
	if err != nil {
		return nil, nil, nil, false, err
	}
	return data, res.rewrites, nil, stopped, nil
}

// stopKeys are the two resume keys qBittorrent 5.3 reads as a stopped torrent:
// paused=1 from libtorrent 1.2's resume format, and auto_managed=0 so the queue
// does not start it again. libtorrent 2 carries the same two keys for
// qBittorrent's benefit.
const (
	keyPaused      = "paused"
	keyAutoManaged = "auto_managed"
)

// stopBytes marks one resume file, or one bencoded resume blob, stopped.
// Everything else in it is left exactly as it was: the same round-trip promise
// as the path rewrite, made by the same encoder.
func stopBytes(name string, raw []byte) ([]byte, bool, error) {
	v, err := bencode.Decode(raw)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", name, err)
	}
	if v.Kind != bencode.Dict {
		return nil, false, fmt.Errorf("%s: is not a bencoded dictionary", name)
	}
	changed := false
	for _, want := range []struct {
		key   string
		value int64
	}{{keyPaused, 1}, {keyAutoManaged, 0}} {
		got, err := setInt(&v, want.key, want.value)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", name, err)
		}
		changed = changed || got
	}
	if !changed {
		return raw, false, nil
	}
	out, err := bencode.Encode(v)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", name, err)
	}
	return out, true, nil
}

// setInt writes one integer into a dictionary, inserting the key in its sorted
// position when the file does not carry it. libtorrent writes both keys, but a
// resume file another client wrote may not, and a missing key is not a reason
// to leave a torrent running.
func setInt(v *bencode.Value, key string, want int64) (bool, error) {
	if f := v.Lookup(key); f != nil {
		if f.Kind != bencode.Int {
			return false, fmt.Errorf("%s is not an integer", key)
		}
		if f.Int == want {
			return false, nil
		}
		f.Int = want
		return true, nil
	}
	k := []byte(key)
	at := len(v.Dict)
	for i := range v.Dict {
		if bytes.Compare(v.Dict[i].Key, k) > 0 {
			at = i
			break
		}
	}
	v.Dict = append(v.Dict, bencode.Field{})
	copy(v.Dict[at+1:], v.Dict[at:])
	v.Dict[at] = bencode.Field{Key: k, Value: bencode.Value{Kind: bencode.Int, Int: want}}
	return true, nil
}

// rewriter maps one resume file's stored paths, remembering the gaps so the
// run can report all of them at once.
type rewriter struct {
	app      string
	file     string
	table    Table
	rewrites map[string]int
	gaps     []plan.Uncovered
}

// paths rewrites every value in the file that holds a path. Everything else is
// left exactly as it was found.
func (r *rewriter) paths(v *bencode.Value) {
	for _, key := range pathKeys {
		if f := v.Lookup(key); f != nil && f.Kind == bencode.String {
			r.value(key, key, f)
		}
	}
	mapped := v.Lookup("mapped_files")
	if mapped == nil || mapped.Kind != bencode.List {
		return
	}
	for i := range mapped.List {
		if e := &mapped.List[i]; e.Kind == bencode.String {
			r.value("mapped_files", fmt.Sprintf("mapped_files[%d]", i), e)
		}
	}
}

// value maps one stored path in place.
//
// label names it for a gap: for a mapped file that is the entry, so the
// operator knows which of a torrent's many names has no rule, while the report
// counts under key so one file with ten unmapped names still reads as one line.
// A value that is not absolute — an empty qBt-savePath under automatic torrent
// management, or a mapped name relative to save_path — comes back unchanged and
// is neither counted nor reported.
func (r *rewriter) value(key, label string, v *bencode.Value) {
	out, ok := r.table.Rewrite(r.app, string(v.Bytes))
	if !ok {
		r.gaps = append(r.gaps, plan.Uncovered{App: r.app, File: r.file, Key: label, Path: string(v.Bytes)})
		return
	}
	if out == string(v.Bytes) {
		return
	}
	v.Bytes = []byte(out)
	r.rewrites[key]++
}

// checkDst refuses a destination this run must not write into, before anything
// is read: a destination holding a half copy looks exactly like a finished one,
// and the next run would refuse it as well.
func checkDst(src, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", src, err)
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dst, err)
	}
	if within(srcAbs, dstAbs) {
		return fmt.Errorf("write %s: it is inside the source %s, which this run only reads", dst, src)
	}
	info, err := os.Stat(dstAbs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat %s: %w", dst, err)
	case !info.IsDir():
		return fmt.Errorf("write %s: it exists and is not a directory", dst)
	}
	entries, err := os.ReadDir(dstAbs)
	if err != nil {
		return fmt.Errorf("read %s: %w", dst, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("write %s: it is not empty; a copy goes to a directory this run creates", dst)
	}
	return nil
}

// within reports whether p is dir or sits below it.
func within(dir, p string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// writeAll is pass two: the destination directory, then every file through
// atomicfile, so a crash leaves complete files rather than half-written resume
// data qBittorrent would reject.
func writeAll(dst string, files []outFile) (written []string, created bool, err error) {
	if _, statErr := os.Stat(dst); errors.Is(statErr, fs.ErrNotExist) {
		if err := os.MkdirAll(dst, dirPerm); err != nil {
			return nil, false, fmt.Errorf("create %s: %w", dst, err)
		}
		created = true
	}
	for _, f := range files {
		if err := atomicfile.Write(filepath.Join(dst, f.name), f.data, f.mode); err != nil {
			return written, created, err
		}
		written = append(written, f.name)
	}
	return written, created, nil
}

// undo removes what this run wrote and only that: the files it copied, and the
// destination directory when this run is what created it. A directory that was
// already there is left empty, which the next run accepts.
func undo(dst string, written []string, created bool) {
	for _, name := range written {
		os.Remove(filepath.Join(dst, name))
	}
	if created {
		os.Remove(dst)
	}
}
