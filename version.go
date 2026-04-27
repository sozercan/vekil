package main

import "strings"

// buildVersion is injected by release/build ldflags. Local builds fall back to
// "dev".
var buildVersion = "dev"

func effectiveBuildVersion() string {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		return "dev"
	}
	return version
}
