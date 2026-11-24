// Package report is the migration report: the page a person reads in full.
//
// A migration moves, renames and leaves behind more than anyone reads on the
// screen while it runs (docs/output.md, rule 7). So the full account goes into
// one sectioned plain-text file beside the snapshot, readable with `less`, and
// the screen keeps the counts and one line naming it. The file opens with a
// short summary -- the numbers a person takes in at a glance -- and then the
// sections: the apps, the data, every rewritten address, what box decided, its
// notes, and what stays behind.
//
// Nothing here holds a secret: the counts, the paths, the addresses and the
// reasons are what a plan and a discovery report already carry, and the release
// Secret's values are never in either. A report is written 0600 beside a
// snapshot that is 0600, in a directory box makes 0700.
package report

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// Width is the column the report fits: the same 80 every screen and log fits,
// because the file is read in the terminal it was written for.
const Width = 80

// suffix is what a report beside a snapshot is named with: the snapshot's own
// name, less its archive suffix, and this.
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
	// Snapshot is the file this report is about. It is named in the first line
	// and in the file name beside it.
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

// Name is where a snapshot's report is written: beside it, named for it.
func Name(snapshot string) string {
	base := filepath.Base(snapshot)
	if cut, ok := strings.CutSuffix(base, ".tar.gz"); ok {
		base = cut
	}
	return filepath.Join(filepath.Dir(snapshot), base+suffix)
}

// Write puts the report beside the snapshot it is about, 0600: it names the
// source's own paths and addresses, and it is a sibling of a file that holds the
// source's API keys.
func Write(path string, d Doc) error {
	return atomicfile.Write(path, []byte(d.Text()), 0o600)
}

// writeText writes text at indent, wrapping at Width with the continuation
// hanging under the text rather than under the rows' names (rule 7).
func writeText(b *strings.Builder, indent int, text string) {
	for _, line := range Wrap(text, Width-indent) {
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(line)
		b.WriteString("\n")
	}
}

// writeField writes one row: the name padded into the column it lines up in, two
// columns, and then the facts, wrapped so a reason that does not fit hangs under
// the facts and not under the name.
func writeField(b *strings.Builder, indent int, name string, column int, facts string) {
	head := strings.Repeat(" ", indent)
	if name != "" {
		width := column
		if len(name) > width {
			width = len(name)
		}
		head += pad(name, width)
		if facts == "" {
			b.WriteString(strings.TrimRight(head, " ") + "\n")
			return
		}
		// A name that takes the whole column leaves no room for its facts on the
		// same line: they go under it, at the row's own indentation, whole.
		if len(head)+2+len(facts) > Width && width > column {
			b.WriteString(strings.TrimRight(head, " ") + "\n")
			head = strings.Repeat(" ", indent+2)
		} else {
			head += "  "
		}
	}
	if facts == "" {
		return
	}
	for i, line := range Wrap(facts, Width-len(head)) {
		if i == 0 {
			b.WriteString(head + line + "\n")
			continue
		}
		b.WriteString(strings.Repeat(" ", len(head)) + line + "\n")
	}
}

// Wrap breaks text into lines of at most width columns, never inside a word: a
// path or an address that does not fit goes on a line of its own, whole, because
// half a path is a different path.
func Wrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) <= width {
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
		if n := len(c.Label); n > w {
			w = n
		}
	}
	return w
}

// widestRow is the width of a section's name column, capped so one long name
// does not push every fact off the column it lines up in.
func widestRow(rows []Row) int {
	const cap = 24
	w := 0
	for _, r := range rows {
		if n := len(r.Name); n > w && n <= cap {
			w = n
		}
	}
	return w
}

// pad right-pads s to width.
func pad(s string, width int) string {
	if n := width - len(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
