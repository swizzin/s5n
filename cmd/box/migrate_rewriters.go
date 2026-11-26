package main

// Rewriters register from their packages' init and nothing else imports them:
// without these blank imports no rewriter is linked and every plan is refused.
// The test beside this file pins the set.
import (
	_ "github.com/swizzin/s5n/internal/migrate/arr"
	_ "github.com/swizzin/s5n/internal/migrate/autobrr"
	_ "github.com/swizzin/s5n/internal/migrate/bazarr"
	_ "github.com/swizzin/s5n/internal/migrate/filebrowser"
	_ "github.com/swizzin/s5n/internal/migrate/jellyfin"
	_ "github.com/swizzin/s5n/internal/migrate/maintainerr"
	_ "github.com/swizzin/s5n/internal/migrate/peerbanhelper"
	_ "github.com/swizzin/s5n/internal/migrate/plex"
	_ "github.com/swizzin/s5n/internal/migrate/qbit"
	_ "github.com/swizzin/s5n/internal/migrate/qui"
	_ "github.com/swizzin/s5n/internal/migrate/sabnzbd"
	_ "github.com/swizzin/s5n/internal/migrate/seerr"
	_ "github.com/swizzin/s5n/internal/migrate/tautulli"
)
