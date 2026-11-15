package rewrite

import (
	"context"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

type nop struct{}

func (nop) Check(context.Context, Input) (Report, error) { return Report{}, nil }
func (nop) Write(context.Context, Input) (Report, error) { return Report{}, nil }

func TestRegistry(t *testing.T) {
	t.Cleanup(func() { delete(registry, "testapp") })
	Register("testapp", nop{})
	if _, ok := For("testapp"); !ok {
		t.Fatal("For(testapp) after Register: not found")
	}
	if _, ok := For("sonarr-4k"); ok {
		t.Error("For finds an instance name; rewriters are filed by catalog app")
	}
	defer func() {
		if recover() == nil {
			t.Error("a second Register for one app did not panic")
		}
	}()
	Register("testapp", nop{})
}

func TestBlocked(t *testing.T) {
	if (Report{Notes: []string{"x"}, Changes: []Change{{Count: 1}}}).Blocked() {
		t.Error("notes and changes blocked apply")
	}
	if !(Report{Uncovered: []plan.Uncovered{{Path: "/x"}}}).Blocked() {
		t.Error("an uncovered path did not block apply")
	}
	if !(Report{Unclassified: []Address{{Host: "nas"}}}).Blocked() {
		t.Error("an unclassified address did not block apply")
	}
}
