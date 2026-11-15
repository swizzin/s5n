package peerbanhelper

import (
	"context"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

func TestRegistered(t *testing.T) {
	if _, ok := rewrite.For("pbh"); !ok {
		t.Fatal("pbh has no rewriter registered")
	}
}

func TestChangesNothingAndSaysWhy(t *testing.T) {
	in := rewrite.Input{App: "pbh", Dir: t.TempDir()}
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("Blocked() with %v, %v", rep.Uncovered, rep.Unclassified)
	}
	if len(rep.Changes) != 0 {
		t.Errorf("Changes = %v, want none", rep.Changes)
	}
	if len(rep.Notes) == 0 {
		t.Error("no note saying why nothing changes")
	}
	written, err := rewriter{}.Write(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Changes) != 0 || written.Blocked() {
		t.Errorf("Write = %+v, want the same empty report", written)
	}
}
