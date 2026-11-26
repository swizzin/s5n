// Package fastresume rewrites the paths qBittorrent stored in a resume
// directory, ahead of migrating it onto box: a client that kept the source's paths
// finds none of its files and downloads everything again from zero, with the
// copies sitting right there. The path keys are libtorrent's save_path
// (write_resume_data.cpp:116, read_resume_data.cpp:171), qBittorrent's qBt-savePath
// and qBt-downloadPath overrides (bencoderesumedatastorage.cpp:260-266), and
// absolute mapped_files entries (write_resume_data.cpp:217); an empty qBt-savePath
// means automatic torrent management and is left alone. Nothing else is a path.
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

// resumeExt is qBittorrent's resume suffix (bencoderesumedatastorage.cpp:483).
const resumeExt = ".fastresume"

// dirPerm is what a directory this package creates gets, the mode `box init` gives
// the config root; handing the tree to puid:pgid is the caller's job.
const dirPerm fs.FileMode = 0o775

// pathKeys are the resume keys libtorrent and qBittorrent read as an absolute
// path; part_file_dir is libtorrent 2's, empty in qBittorrent's own files
// (libtorrent@v2.1.2:src/write_resume_data.cpp:109, read_resume_data.cpp:288).
var pathKeys = []string{"save_path", "qBt-savePath", "qBt-downloadPath", "part_file_dir"}

// Table is the plan's per-app path rules; a table is never applied to another
// app's files, because each app saw its own paths.
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
	// stop marks every torrent stopped as well as rewriting its paths: a
	// rehearsal's copy must not seed, recheck or fetch metadata.
	stop bool
}

// DryRun computes the report and writes nothing, not even the destination
// directory, which is ignored.
func DryRun() Option {
	return func(c *config) { c.dryRun = true }
}

// Stop marks every torrent stopped: paused=1 and auto_managed=0, which qBittorrent
// 5.3 reads as stopped (bencoderesumedatastorage.cpp:360). With the data absent or
// shared, a running torrent would recheck to 0% and churn the disk, and a seeding
// one would announce. It is the same pass as the path rewrite, so a file is
// decoded once and every key this package does not name comes back byte for byte.
func Stop() Option {
	return func(c *config) { c.stop = true }
}

// RewriteDir copies src into dst, rewriting every resume file's paths. src is
// only ever read: it belongs to the source install, which stays the rollback. dst
// must not exist or must be empty, or it could not be told from a finished copy.
// Every file is read and decoded before the first write, so a gap refuses the run
// and leaves dst as it found it, with every gap in one *plan.UncoveredError: a
// resume directory is thousands of files.
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

// outFile is one file pass two writes, under the source's mode.
type outFile struct {
	name    string
	data    []byte
	mode    fs.FileMode
	changed bool // the bytes differ from what the source held
}

// resume is one file's rewrite; a file with gaps has no bytes, because the run
// refuses rather than write half a rewrite.
type resume struct {
	data     []byte
	rewrites map[string]int
	gaps     []plan.Uncovered
}

// read is pass one: every file read, every resume file rewritten in memory. Gaps
// come back as values, because the caller reports them all at once.
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
			// qBittorrent writes this directory flat; a subdirectory is someone
			// else's data.
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
			// The metadata is copied byte for byte: its paths are relative to
			// save_path and stay right.
			rep.Copied++
		}
		files = append(files, out)
	}
	return files, gaps, nil
}

// stoppedData marks one file stopped in a rehearsal, counting it; any other run
// gets the bytes back unchanged, because the stop is the snapshot's mode.
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

// rewriteFile returns the bytes pass two writes, with gaps as values so they are
// reported with the rest of the directory.
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

// rewriteResume is one file's rewrite in memory; with gaps there are no bytes.
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

// Scan reports what a rewrite would do, writing nothing. Gaps come back as values
// so a rewriter collects them with its other gaps before refusing once.
func Scan(dir, app string, table Table, opts ...Option) (Report, []plan.Uncovered, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	rep := Report{Rewrites: make(map[string]int)}
	_, gaps, err := read(dir, app, table, &rep, cfg)
	return rep, gaps, err
}

// Rewrite rewrites a resume directory in place: only changed files are written,
// through atomicfile, so a second run writes nothing.
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

// Blob rewrites one bencoded blob in memory, as the SQLite store keeps it; name
// labels it in a gap.
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

// The two keys qBittorrent 5.3 reads as stopped: paused=1 and auto_managed=0, so
// the queue does not start it again.
const (
	keyPaused      = "paused"
	keyAutoManaged = "auto_managed"
)

// stopBytes marks one file or blob stopped, leaving everything else byte for
// byte.
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

// setInt writes one integer, inserting the key in its sorted position when
// absent: a missing key is not a reason to leave a torrent running.
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

// rewriter maps one file's stored paths, keeping the gaps for one report.
type rewriter struct {
	app      string
	file     string
	table    Table
	rewrites map[string]int
	gaps     []plan.Uncovered
}

// paths rewrites every value that holds a path, leaving the rest as found.
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

// value maps one stored path in place. label names the entry in a gap, while the
// report counts under key so ten unmapped names read as one line. A relative value
// (automatic torrent management's empty qBt-savePath, a mapped name) is unchanged
// and neither counted nor reported.
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

// checkDst refuses a destination this run must not write into, before anything is
// read: a half copy looks exactly like a finished one.
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

// writeAll is pass two: the directory, then every file through atomicfile, so a
// crash leaves complete files.
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

// undo removes what this run wrote: its files, and a directory it created; an
// already-present directory is left empty, which the next run accepts.
func undo(dst string, written []string, created bool) {
	for _, name := range written {
		os.Remove(filepath.Join(dst, name))
	}
	if created {
		os.Remove(dst)
	}
}
