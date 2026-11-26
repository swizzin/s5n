package apps

import (
	"fmt"
)

// LoginRejectedError is one app's refusal of a session: the account exists but the
// password sent is not the one it holds. qui and autobrr take their password from the
// Secret only at pod start, so a password changed inside a running app lands here
// until it restarts; the caller says how (qui answers 401, autobrr 403).
//
// Jellyfin's is a different state with the same shape: a migrated server's
// administrator password is one box never had, so it is reported pending.
type LoginRejectedError struct {
	App      string
	Username string
}

func (e *LoginRejectedError) Error() string {
	return fmt.Sprintf("%s rejected the account %q", e.App, e.Username)
}

// LoginUnavailableError is an app refusing its own password login because of the auth
// mode it runs in. It is not a credential that changed, so it must not be reported as
// one.
//
// autobrr answers this way: v1.91.1 resolves its auth mode in
// internal/domain/config.go's AuthCapabilities, and POST /api/auth/login answers 403
// "password login is unavailable in oidc authentication mode" when OIDC is on with the
// built-in login off (internal/http/auth.go:122-126), which is a chart decision.
type LoginUnavailableError struct {
	App  string
	Mode string
}

func (e *LoginUnavailableError) Error() string {
	return fmt.Sprintf("%s does not offer its own password login (%s mode): its configuration has to keep the built-in login reachable for the account s5n wires with", e.App, e.Mode)
}
