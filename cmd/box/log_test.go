package main

import (
	"testing"

	"github.com/swizzin/s5n/internal/runlog"
)

// A log is named for the command that wrote it, and an update timer's wake is
// named apart from an update a person ran: the prune keeps only a few wakes.
func TestALogIsNamedForItsCommand(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"box"}, boxName},
		{[]string{"box", "apply", "--values", "v.yaml"}, "apply"},
		{[]string{"box", "migrate", "discover"}, "migrate-discover"},
		{[]string{"box", "migrate", "--help"}, "migrate"},
		{[]string{"box", "update"}, "update"},
		{[]string{"box", "update", "--values", "v.yaml", "--timer"}, runlog.TimerCommand},
		{[]string{"box", "update", "-timer"}, runlog.TimerCommand},
		{[]string{"box", "update", "--timer=false"}, "update"},
	}
	for _, c := range cases {
		if got := logNameFor(c.args); got != c.want {
			t.Errorf("logNameFor(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
