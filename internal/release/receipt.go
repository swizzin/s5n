package release

import (
	"fmt"
	"time"

	"github.com/swizzin/s5n/internal/say"
)

// wiringName is what box calls the wiring Job: the row its receipt folds to, the
// row a wait that ran out of time leaves, the line the live wait shows, and the
// container the chart gives the Job's pod.
const wiringName = "wiring"

// The receipt's two not-ok results, as the Job writes them in status.json.
const (
	statePending = "pending"
	stateFailed  = "failed"
)

// ReportReceipt folds a wiring receipt into the lines a person reads. A receipt
// whose every check passed is one row: what the Job checked, and how long it
// took. A check that failed, and one that waits on a person, are one row each,
// with the reason under it; everything else stays in the run log.
//
// The whole receipt -- every check, its result and its detail -- goes to the
// run log either way, because the row that folded thirty-four checks into a
// count is also the row that threw the evidence away.
//
// rehearsal is a box built from a rehearsal snapshot. It has no internet, so a
// check that waits on a claim or a sign-in is not one to clear here: the rows
// say that the real run clears them.
func ReportReceipt(r say.Reporter, rec Receipt, rehearsal bool) {
	if rec.Status == "" && len(rec.Steps) == 0 {
		return
	}
	logReceipt(r, rec)
	rows, pending, hasFailed := receiptRows(rec)
	if rec.Status == "ok" && !hasFailed {
		// What went to plan is a count. What waits on a person is still a row:
		// a healthy Job with an unclaimed Plex has done its work and needs the
		// person for the next part.
		r.Rows([]say.Row{{Mark: say.MarkDone, Name: wiringName, Facts: receiptFacts(rec)}})
		r.Rows(rows)
		if rehearsal && pending > 0 {
			r.Rows([]say.Row{rehearsalRow()})
		}
		return
	}
	if len(rows) == 0 {
		rows = []say.Row{{Mark: say.MarkFailed, Name: wiringName, State: stateFailed, Why: rec.Error}}
	}
	r.Rows(rows)
	if rehearsal && pending > 0 {
		r.Rows([]say.Row{rehearsalRow()})
	}
}

// receiptRows are the checks that are not ok: one row each, in the Job's own
// order, because that is the order the run reached them in. pending counts the
// ones that wait on a person.
func receiptRows(rec Receipt) (rows []say.Row, pending int, hasFailed bool) {
	for _, step := range rec.Steps {
		switch step.Result {
		case stateFailed:
			hasFailed = true
			rows = append(rows, say.Row{Mark: say.MarkFailed, Name: step.Name, State: stateFailed, Why: step.Detail})
		case statePending:
			pending++
			rows = append(rows, say.Row{Mark: say.MarkNeeds, Name: step.Name, State: statePending, Why: step.Detail})
		}
	}
	return rows, pending, hasFailed
}

// rehearsalRow is the line a rehearsal box's pending rows end with: they are
// the real run's to clear, and this box is going away.
func rehearsalRow() say.Row {
	return say.Row{Mark: say.MarkNote, Facts: []string{"rehearsal", "pending rows clear on the real run"}}
}

// receiptFacts are the one row a healthy receipt gets: how many checks it ran,
// and the Job's own duration. A receipt that carries no duration (a fake, or an
// older Job) says the count alone rather than "0s".
func receiptFacts(rec Receipt) []string {
	facts := []string{fmt.Sprintf("%d %s", len(rec.Steps), plural(len(rec.Steps), "check", "checks"))}
	if d := rec.Duration.Round(time.Second); d > 0 {
		facts = append(facts, say.Duration(d))
	}
	return facts
}

// logReceipt is the receipt's detail, one line per check, in the run log's own
// shape: the same words a row would have said, with nothing folded away.
func logReceipt(r say.Reporter, rec Receipt) {
	for _, step := range rec.Steps {
		detail := step.Detail
		if detail != "" {
			detail = " (" + detail + ")"
		}
		r.Detail(fmt.Sprintf("wiring %s: %s%s", step.Name, step.Result, detail))
	}
	if rec.Status != "ok" && rec.Error != "" {
		r.Detail(fmt.Sprintf("wiring %s: %s", rec.Status, rec.Error))
	}
}
