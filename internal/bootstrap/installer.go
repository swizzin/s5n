package bootstrap

import (
	"context"
	"fmt"
	"io"
)

// PrintInstaller is the real Installer for this phase: it only prints what it
// would do and installs nothing. The decision logic is unchanged whether the
// installer is this, or the real k0s installer that lands in a later Phase 2
// step.
type PrintInstaller struct {
	W io.Writer
}

// Install prints the planned k0s install.
func (p PrintInstaller) Install(_ context.Context, plan InstallPlan) error {
	w := p.W
	if w == nil {
		w = io.Discard
	}
	if plan.SHA256 != "" {
		fmt.Fprintf(w, "box would install k0s %s (sha256 %s) on this clean host: k0s install controller --single\n", plan.Version, plan.SHA256)
	} else {
		fmt.Fprintf(w, "box would install k0s %s on this clean host: k0s install controller --single\n", plan.Version)
	}
	return nil
}
