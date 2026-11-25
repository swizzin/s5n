package main

// Every app's rewriter registers itself from its package's init, and nothing
// else in box refers to those packages, so without these imports the binary
// links none of them: `box migrate apply` would then find no rewriter for any
// app and refuse every plan. The test beside this file pins the set.
import (
	_ "github.com/swizzin/s5n/internal/migrate/arr"
	_ "github.com/swizzin/s5n/internal/migrate/autobrr"
	_ "github.com/swizzin/s5n/internal/migrate/bazarr"
	_ "github.com/swizzin/s5n/internal/migrate/maintainerr"
	_ "github.com/swizzin/s5n/internal/migrate/peerbanhelper"
	_ "github.com/swizzin/s5n/internal/migrate/plex"
	_ "github.com/swizzin/s5n/internal/migrate/qbit"
	_ "github.com/swizzin/s5n/internal/migrate/qui"
	_ "github.com/swizzin/s5n/internal/migrate/sabnzbd"
	_ "github.com/swizzin/s5n/internal/migrate/seerr"
	_ "github.com/swizzin/s5n/internal/migrate/tautulli"
)
