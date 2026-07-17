package launch

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		_, _ = fmt.Fprintln(os.Stdout, "2.1.211 (Claude Code)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
