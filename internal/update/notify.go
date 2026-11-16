package update

import "context"

// The three things a step tells someone about. They are words rather than an
// enum because they travel: a notice carries one, and the receiver's rules are
// written against it.
const (
	// EventUpdated is a step that passed.
	EventUpdated = "updated"
	// EventRolledBack is one app box put back on the image it ran.
	EventRolledBack = "rolled-back"
	// EventStopped is a step that did not pass.
	EventStopped = "stopped"
)

// Notice is one thing this run did, as update tells it. update builds no
// sentence and opens no connection: a box with no address to post to must not
// pay for the interface, and the words a person reads are cmd/box's.
type Notice struct {
	Event string
	// Name is the chart's name, so the receiver reads "s5n 0.5.1 -> 0.5.2"
	// rather than a bare version pair.
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

// AppChange is one app's image on either side of a step, as the plan had it: the
// short handle the plan prints (a tag when there was one), not a digest.
type AppChange struct {
	App  string
	From string
	To   string
}

// notify tells whoever is listening about one event, if anyone is. It is one
// function on the Updater rather than a report line because the report is for
// the person running box, and this is for a person who is not.
func (u *Updater) notify(ctx context.Context, n Notice) {
	if u.Notify == nil {
		return
	}
	u.Notify(ctx, n)
}

// movedApps is what a passing step moved, in plan order, without the apps it put
// back: an app rolled back did not stay on the new image, and a notice that said
// it moved would be wrong about the one thing it is for.
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
