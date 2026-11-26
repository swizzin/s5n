// Package report is the migration report: a migration leaves behind more than
// the screen shows (docs/output.md, rule 7), so the full account goes into one
// sectioned plain-text file beside the snapshot, opened by the summary. Nothing
// here holds a secret; the file is 0600 beside a 0600 snapshot in a 0700
// directory.
package report

import (
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// Width is the column the report fits, the same 80 every screen and log fits.
const Width = 80

// suffix names a report beside a snapshot: the snapshot's name, less its
// archive suffix.
const suffix = "-report.txt"

// Row is one line of a section: what the line is about, and the facts that
// belong to it.
type Row struct {
	Name  string
	Facts []string
}

// Section is one heading and its rows. Titles are lowercase nouns, one level
// deep (rule 9).
type Section struct {
	Title string
	Rows  []Row
}

// Count is one line of the summary: a label and the number a person reads.
type Count struct {
	Label string
	Value string
}

// Doc is one migration report: what the run read, did or will do.
type Doc struct {
	// Kind is the snapshot's mode: survey, rehearsal or final.
	Kind string
	// Snapshot is the file this report is about, named in the first line and in
	// the file name beside it.
	Snapshot string
	// Taken is when the source was read.
	Taken time.Time
	// Box is the version that wrote it.
	Box string
	// Counts are the summary at the top: apps, addresses, notes, what stays.
	Counts []Count
	// Sections are the rest, in the order a person reads them.
	Sections []Section
}

// Text is the report as the file holds it.
func (d Doc) Text() string {
	var b strings.Builder
	head := "s5n migration report"
	if d.Kind != "" {
		head += " · " + d.Kind
	}
	if d.Snapshot != "" {
		head += " · " + filepath.Base(d.Snapshot)
	}
	writeText(&b, 0, head)
	read := "read " + d.Taken.UTC().Format("2006-01-02 15:04") + " UTC"
	if d.Box != "" {
		read += " by box " + d.Box
	}
	writeText(&b, 0, read)
	if len(d.Counts) > 0 {
		b.WriteString("\nsummary\n")
		label := widestLabel(d.Counts)
		for _, c := range d.Counts {
			writeField(&b, 2, c.Label, label, c.Value)
		}
	}
	for _, s := range d.Sections {
		if len(s.Rows) == 0 {
			continue
		}
		b.WriteString("\n" + s.Title + "\n")
		label := widestRow(s.Rows)
		for _, r := range s.Rows {
			writeField(&b, 2, r.Name, label, strings.Join(r.Facts, " · "))
		}
	}
	return b.String()
}

// Name is the read's page, beside the snapshot and named for it: the same text
// the snapshot carries as its report.txt.
func Name(snapshot string) string {
	return beside(snapshot, suffix)
}

// applySuffix is a run's own page beside the same snapshot, so it never replaces
// the read's.
const applySuffix = "-apply-report.txt"

// ApplyName is where a run's own report is written: beside the snapshot, named
// for it and for the run.
func ApplyName(snapshot string) string {
	return beside(snapshot, applySuffix)
}

// beside is a snapshot's own name with one suffix, in the snapshot's directory.
func beside(snapshot, suffix string) string {
	base := filepath.Base(snapshot)
	if cut, ok := strings.CutSuffix(base, ".tar.gz"); ok {
		base = cut
	}
	return filepath.Join(filepath.Dir(snapshot), base+suffix)
}

// Write puts the report beside the snapshot, 0600: it names the source's paths
// and addresses, beside a file that holds its API keys.
func Write(path string, d Doc) error {
	return atomicfile.Write(path, []byte(d.Text()), 0o600)
}

// writeText writes text at indent, wrapped at Width with the continuation hanging
// under the text, not under a row's name (rule 7).
func writeText(b *strings.Builder, indent int, text string) {
	for _, line := range Wrap(text, Width-indent) {
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// writeField writes one row: the name padded into its column, two columns, then
// the facts, wrapped so a long reason hangs under the facts.
func writeField(b *strings.Builder, indent int, name string, column int, facts string) {
	head := strings.Repeat(" ", indent)
	if name != "" {
		width := column
		if Cols(name) > width {
			width = Cols(name)
		}
		head += pad(name, width)
		if facts == "" {
			b.WriteString(strings.TrimRight(head, " ") + "\n")
			return
		}
		// A name as wide as the column leaves no room for facts: they go under
		// it.
		if Cols(head)+2+Cols(facts) > Width && width > column {
			b.WriteString(strings.TrimRight(head, " ") + "\n")
			head = strings.Repeat(" ", indent+2)
		} else {
			head += "  "
		}
	}
	if facts == "" {
		return
	}
	// A row with no name is its facts, its continuation two columns further in:
	// one starting at the row's indent would read as another row.
	hang := Cols(head)
	if name == "" {
		hang += 2
	}
	for i, line := range Wrap(facts, Width-Cols(head)) {
		if i == 0 {
			b.WriteString(head + line + "\n")
			continue
		}
		b.WriteString(strings.Repeat(" ", hang) + line + "\n")
	}
}

// Cols is how many columns s takes on a terminal: one per character, so a
// multi-byte arrow is one column.
func Cols(s string) int { return utf8.RuneCountInString(s) }

// Wrap breaks text into lines of at most width columns, never inside a word: half
// a path is a different path.
func Wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := glueSeparators(strings.Fields(text))
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if Cols(line)+1+Cols(w) <= width {
			line += " " + w
			continue
		}
		out = append(out, line)
		line = w
	}
	return append(out, line)
}

// widestLabel is the width of the summary's label column.
func widestLabel(counts []Count) int {
	w := 0
	for _, c := range counts {
		if n := Cols(c.Label); n > w {
			w = n
		}
	}
	return w
}

// widestRow is the width of a section's name column, capped so one long name
// does not push every fact out of line.
func widestRow(rows []Row) int {
	const cap = 24
	w := 0
	for _, r := range rows {
		if n := Cols(r.Name); n > w && n <= cap {
			w = n
		}
	}
	return w
}

// pad right-pads s to width.
func pad(s string, width int) string {
	if n := width - Cols(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// glueSeparators keeps a middle dot on the line of the fact before it, so a
// wrapped line never starts with one.
func glueSeparators(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w == "·" && len(out) > 0 {
			out[len(out)-1] += " ·"
			continue
		}
		out = append(out, w)
	}
	return out
}
