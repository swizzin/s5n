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
// filesystem path.
var pathKeys = []string{"save_path", "qBt-savePath", "qBt-downloadPath"}

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
	Rewrites  map[string]int // path rewrites per resume key
}

// Option varies a run.
type Option func(*config)

type config struct {
	dryRun bool
}

// DryRun computes the report and writes nothing, not even the destination
// directory, which is ignored.
func DryRun() Option {
	return func(c *config) { c.dryRun = true }
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
	files, gaps, err := read(src, app, table, &rep)
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
	name string
	data []byte
	mode fs.FileMode
}

// read is pass one: every file in src is read, and every resume file decoded
// and rewritten in memory. Gaps come back rather than an error, because the
// caller reports them all together once the directory has been read.
func read(src, app string, table Table, rep *Report) ([]outFile, []plan.Uncovered, error) {
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
				out.data = data
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

// rewriteFile decodes one resume file and returns the bytes pass two writes.
// The gaps it finds are returned rather than an error: a file with two unmapped
// paths is worth reporting once, with the rest of the directory.
func rewriteFile(name string, raw []byte, app string, table Table, rep *Report) ([]byte, []plan.Uncovered, error) {
	v, err := bencode.Decode(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if v.Kind != bencode.Dict {
		return nil, nil, fmt.Errorf("%s: is not a bencoded dictionary", name)
	}
	r := rewriter{app: app, file: name, table: table, rep: rep}
	r.paths(&v)
	if len(r.gaps) > 0 {
		return nil, r.gaps, nil
	}
	out, err := bencode.Encode(v)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if bytes.Equal(out, raw) {
		rep.Unchanged++
		return raw, nil, nil
	}
	rep.Rewritten++
	return out, nil, nil
}

// rewriter maps one resume file's stored paths, remembering the gaps so the
// run can report all of them at once.
type rewriter struct {
	app   string
	file  string
	table Table
	rep   *Report
	gaps  []plan.Uncovered
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
	r.rep.Rewrites[key]++
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
