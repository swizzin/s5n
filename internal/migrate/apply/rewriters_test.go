package apply

// rewriters_test.go links every rewriter into the test binary.
//
// A rewriter files itself under its catalog app from its own init()
// (internal/migrate/rewrite, Register), so a package that is not linked has no
// rewriter at all and every plan naming its app is refused. That is the same

import (
	_ "github.com/swizzin/s5n/internal/migrate/arr"
	_ "github.com/swizzin/s5n/internal/migrate/autobrr"
	_ "github.com/swizzin/s5n/internal/migrate/maintainerr"
	_ "github.com/swizzin/s5n/internal/migrate/peerbanhelper"
	_ "github.com/swizzin/s5n/internal/migrate/plex"
	_ "github.com/swizzin/s5n/internal/migrate/qbit"
	_ "github.com/swizzin/s5n/internal/migrate/qui"
	_ "github.com/swizzin/s5n/internal/migrate/seerr"
	_ "github.com/swizzin/s5n/internal/migrate/tautulli"
)
