package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sozercan/vekil/proxy"
)

func runState(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "prune" {
		_, _ = fmt.Fprintln(stderr, "Usage: vekil state prune --file PATH --before RFC3339 --confirm")
		return 2
	}
	fs := flag.NewFlagSet("state prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("file", "", "Existing private durable state file; serving must be stopped")
	cutoff := fs.String("before", "", "Retire records issued before this whole-second RFC3339 timestamp")
	confirm := fs.Bool("confirm", false, "Acknowledge that affected continuations will fail after pruning")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	before, err := time.Parse(time.RFC3339, *cutoff)
	// Reject fractional syntax before Go's parser can silently truncate digits
	// beyond nanosecond precision. The store records issuance in whole seconds.
	if err != nil || *path == "" || !*confirm || fs.NArg() != 0 || strings.ContainsAny(*cutoff, ".,") || before.Unix() <= 0 || before.After(time.Now()) {
		_, _ = fmt.Fprintln(stderr, "pruning requires --file, a past whole-second --before RFC3339 timestamp, and --confirm; affected continuation state will become unknown")
		return 2
	}
	removed, err := proxy.PruneDurableStateBindings(*path, before)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Pruned %d ownership records; affected continuations can no longer be verified. Database pages are retained for reuse.\n", removed)
	return 0
}
