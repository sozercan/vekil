package main

import (
	"fmt"
	"io"
	"strings"
)

var (
	buildVersion = "dev"
	buildCommit  = "unknown"
)

func versionString() string {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = "dev"
	}
	commit := strings.TrimSpace(buildCommit)
	if commit == "" || commit == "unknown" {
		return version
	}
	return fmt.Sprintf("%s (%s)", version, commit)
}

func runVersion(w io.Writer) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintln(w, versionString())
}
