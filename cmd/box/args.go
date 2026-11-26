package main

import (
	"flag"
	"fmt"
)

// parseNoArgs refuses a leftover argument: Go's parser stops at a stray word, so
// `box update sonarr --check` would silently run a whole update.
func parseNoArgs(fs *flag.FlagSet, verb string, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments, and %q is not a flag; nothing after it was read", verb, fs.Arg(0))
	}
	return nil
}
