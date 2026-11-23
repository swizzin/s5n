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

// LoginUnavailableError is one app refusing its own password login outright,
// because of the auth mode it is running in. It is not a credential that
// changed, so it must not be reported as one: an operator told to restart the
// pod would restart it and find the same refusal.
//
// autobrr is the app that answers this way: v1.91.0 resolves its auth mode and
// capabilities in one place (internal/domain/config.go's AuthCapabilities at
// v1.91.1) and POST /api/auth/login answers 403 "password login is unavailable
// in oidc authentication mode" when OIDC is on with the built-in login switched
// off (internal/http/auth.go:122-126), which is a chart decision.
type LoginUnavailableError struct {
	App  string
	Mode string
}

func (e *LoginUnavailableError) Error() string {
	return fmt.Sprintf("%s does not offer its own password login (%s mode): its configuration has to keep the built-in login reachable for the account s5n wires with", e.App, e.Mode)
}
