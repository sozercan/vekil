package main

import "strings"

// buildVersion is injected at build time. Local builds fall back to "dev".
var buildVersion = "dev"

func normalizedBuildVersion() string {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		return "dev"
	}
	return version
}
