package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

func TestParseNoArgs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		check bool
		err   string
	}{
		{name: "flags only", args: []string{"--check"}, check: true},
		{name: "nothing", args: nil},
		// The case that matters: a stray word ends the flags, so --check
		// after it is never read and the run would be a real one.
		{name: "a word before a flag", args: []string{"sonarr", "--check"}, err: `update takes no arguments, and "sonarr" is not a flag`},
		{name: "a word after a flag", args: []string{"--check", "sonarr"}, check: true, err: `"sonarr" is not a flag`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("update", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			check := fs.Bool("check", false, "")
			err := parseNoArgs(fs, "update", tc.args)
			switch {
			case tc.err == "" && err != nil:
				t.Fatalf("parseNoArgs(%q): %v", tc.args, err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Fatalf("parseNoArgs(%q) = %v, want an error containing %q", tc.args, err, tc.err)
			}
			if *check != tc.check {
				t.Fatalf("parseNoArgs(%q): check = %v, want %v", tc.args, *check, tc.check)
			}
		})
	}
}
