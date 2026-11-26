package release

import (
	"fmt"
	"time"

	"github.com/swizzin/s5n/internal/say"
)

// wiringName is what box calls the wiring Job in rows, waits and its container.
const wiringName = "wiring"

// The receipt's two not-ok results, as the Job writes them in status.json.
const (
	statePending = "pending"
	stateFailed  = "failed"
)

// ReportReceipt folds a receipt into rows: all passing is one row with a count;
// a failed or pending check gets its own row and reason. The whole receipt still
// goes to the run log, because the folded row throws the evidence away. A
// rehearsal box's pending checks clear on the real run.
func ReportReceipt(r say.Reporter, rec Receipt, rehearsal bool) {
	if rec.Status == "" && len(rec.Steps) == 0 {
		return
	}
	logReceipt(r, rec)
	rows, pending, hasFailed := receiptRows(rec)
	if rec.Status == "ok" && !hasFailed {
		// A pending check still needs the person even when the Job is healthy.
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

// receiptRows is the non-ok checks in the Job's order; pending wait on a person.
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

// rehearsalRow says pending rows clear on the real run.
func rehearsalRow() say.Row {
	return say.Row{Mark: say.MarkNote, Facts: []string{"rehearsal", "pending rows clear on the real run"}}
}

// receiptFacts is the check count and duration; a sub-second Job says one
// second, an older Job without a duration says the count alone.
func receiptFacts(rec Receipt) []string {
	facts := []string{fmt.Sprintf("%d %s", len(rec.Steps), plural(len(rec.Steps), "check", "checks"))}
	if rec.Duration > 0 {
		d := rec.Duration.Round(time.Second)
		if d < time.Second {
			d = time.Second
		}
		facts = append(facts, say.Duration(d))
	}
	return facts
}

// logReceipt writes every check into the run log, nothing folded.
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
