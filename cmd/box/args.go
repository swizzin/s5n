package main

import (
	"flag"
	"fmt"
)

// parseNoArgs parses a verb's flags and refuses anything left over. Go's flag
// package stops at the first argument that is not a flag and leaves the rest
// unread, so `box update sonarr --check` would otherwise drop --check with the
// app and run a whole update, and a --kubeconfig after a stray word would fall
// back to the default cluster. A verb that takes no arguments says so instead.
func parseNoArgs(fs *flag.FlagSet, verb string, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments, and %q is not a flag; nothing after it was read", verb, fs.Arg(0))
	}
	return nil
}
