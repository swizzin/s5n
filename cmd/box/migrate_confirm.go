package main

// migrate_confirm.go renders the one confirm `box migrate apply` asks, as
// docs/output.md's s-confirm: app columns, the library, rows needing a person,
// one footer, one question. The full account is the report; --details prints it.

// confirmScreen is the heading and report path; an empty Report writes none.
type confirmScreen struct {
	Head string
	// Snapshot is the file applied, which names the report.
	Snapshot string
	// Report is the page's file beside the snapshot.
	Report string
	// Taken and Kind name the snapshot in that report, when it has a path.
	Taken string
	Kind  string
}
