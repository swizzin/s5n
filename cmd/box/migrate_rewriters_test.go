package main

import (
	"path/filepath"
	"testing"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// TestEveryCatalogAppHasARewriterInTheBinary: box rewrites every migrated
// app's stored paths with no exceptions (docs/box.md, "### migrate"), and a
// rewriter exists only if this binary links its package. No test file in this
// package imports a rewriter, so what registers here is exactly what ships.
func TestEveryCatalogAppHasARewriterInTheBinary(t *testing.T) {
	cat, err := catalog.Load(filepath.Join("..", "..", "charts", "s5n"))
	if err != nil {
		t.Fatalf("load the chart's catalog: %v", err)
	}
	for _, app := range cat.Names() {
		if _, ok := rewrite.For(app); !ok {
			t.Errorf("catalog app %s has no rewriter linked into box (linked: %v)", app, rewrite.Apps())
		}
	}
	if len(rewrite.Apps()) == 0 {
		t.Fatal("box links no rewriter at all")
	}
}
