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
	if len(args) == 0 || (args[0] != "prune" && args[0] != "prune-history") {
		_, _ = fmt.Fprintln(stderr, "Usage: vekil state {prune|prune-history} --file PATH --before RFC3339 --confirm")
		return 2
	}
	history := args[0] == "prune-history"
	fs := flag.NewFlagSet("state "+args[0], flag.ContinueOnError)
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
	prune := proxy.PruneDurableStateBindings
	if history {
		prune = proxy.PruneConversationHistory
	}
	removed, err := prune(*path, before)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if history {
		_, _ = fmt.Fprintf(stdout, "Pruned %d conversation snapshots and retired eligible unresolved attempts; affected history cannot migrate. Ownership records remain. Freed pages are reusable, not securely erased.\n", removed)
	} else {
		_, _ = fmt.Fprintf(stdout, "Pruned %d ownership records; affected continuations can no longer be verified. Database pages are retained for reuse.\n", removed)
	}
	return 0
}
