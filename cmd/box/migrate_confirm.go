package main

// migrate_confirm.go renders the one confirm `box migrate apply` asks: the
// heading line, the blocks a person decides on, and the question.
//
// It is the screen docs/output.md's s-confirm holds: the apps in columns (what
// each carries, what changes), the library the run lays out, the rows that need a
// person, one footer line counting what folded away, and the one question. Every
// block wraps at 80 with the facts hanging under the facts column, so a row that
// says a lot still reads as a table (rule 7). The whole account -- every rewrite
// by column, every note said once -- is the report beside the snapshot, and
// --details prints the per-rewrite list.

// confirmScreen is what a migrate confirm says above its blocks: the heading
// naming the run, and where its report is written. Report empty writes none,
// which is what a rehearsal of nothing and a test's synthetic summary want.
type confirmScreen struct {
	Head string
	// Snapshot is the file this run applies, which its report is named for.
	Snapshot string
	// Report is the file this run's page goes to, beside the snapshot.
	Report string
	// Taken and Kind name the snapshot in that report, when it has a path.
	Taken string
	Kind  string
}
