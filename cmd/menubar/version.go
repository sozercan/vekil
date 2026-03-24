package main

import "strings"

// buildVersion is injected by make build-app so the menu shows the same
// version as the packaged app bundle. Local builds fall back to "dev".
var buildVersion = "dev"

func versionMenuTitle() string {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = "dev"
	}

	return "Version " + version
}
