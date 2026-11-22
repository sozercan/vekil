package main

import "strings"

var buildVersion = "dev"

func versionMenuTitle() string {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = "dev"
	}

	return "Version " + version
}
