package main

import "strings"

// buildVersion is injected by release/build ldflags. Local builds fall back to
// "dev" so build info metrics still expose a stable version label.
var buildVersion = "dev"

func normalizedBuildVersion() string {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		return "dev"
	}
	return version
}
