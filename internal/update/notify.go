package update

import "context"

// The three events a step reports; they travel in a notice, so they are words.
const (
	// EventUpdated is a step that passed.
	EventUpdated = "updated"
	// EventRolledBack is one app box put back on the image it ran.
	EventRolledBack = "rolled-back"
	// EventStopped is a step that did not pass.
	EventStopped = "stopped"
)

// Notice is one thing this run did. update builds no sentence and opens no
// connection; the words a person reads are cmd/box's.
type Notice struct {
	Event string
	// Name is the chart's name, so a receiver reads "s5n 0.5.1 -> 0.5.2".
	Name string
	// From and To are the releases the step moved between.
	From string
	To   string
	// Apps are the apps the event is about, with their images where it is
	// about a move.
	Apps []AppChange
	// Gate, Error and On are a stopped step's: which gate failed, what it
	// said, and the release the box is on now.
	Gate  string
	Error string
	On    string
}

// AppChange is one app's image on either side, as the plan prints it.
type AppChange struct {
	App  string
	From string
	To   string
}

// notify tells whoever is listening about one event, if anyone is.
func (u *Updater) notify(ctx context.Context, n Notice) {
	if u.Notify == nil {
		return
	}
	u.Notify(ctx, n)
}

// movedApps is what a passing step moved, without the apps it put back: a
// rolled-back app did not stay on the new image.
func movedApps(step StepPlan, rolledBack []string) []AppChange {
	back := make(map[string]bool, len(rolledBack))
	for _, app := range rolledBack {
		back[app] = true
	}
	out := make([]AppChange, 0, len(step.Moving))
	for _, app := range step.Moving {
		if back[app] {
			continue
		}
		change := step.Images[app]
		out = append(out, AppChange{App: app, From: change.Ran.Label(), To: change.MovedTo.Label()})
	}
	return out
}
