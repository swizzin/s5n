package apps

import (
	"fmt"
)

// LoginRejectedError is one app's own refusal of a session: the account exists
// but the password it holds is not the one that was sent. Both apps s5n wires
// take their password from the release Secret only when their pod starts (the
// init container writes it on every start), so a password changed inside a
// running app lands here until that pod is restarted. It is a state an operator
// can fix, and the caller's sentence says how; the app's own HTTP status is
// not useful on its own (qui answers 401, autobrr 403).
type LoginRejectedError struct {
	App      string
	Username string
}

func (e *LoginRejectedError) Error() string {
	return fmt.Sprintf("%s rejected the account %q", e.App, e.Username)
}
