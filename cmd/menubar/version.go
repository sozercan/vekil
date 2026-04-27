package main

import buildversion "github.com/sozercan/vekil/version"

func versionMenuTitle() string {
	return "Version " + buildversion.String()
}
