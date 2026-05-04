package buildinfo

import "strings"

// Version and Commit are populated via -ldflags for release builds.
var (
	Version = "dev"
	Commit  = "unknown"
)

func NormalizedVersion() string {
	version := strings.TrimSpace(Version)
	if version == "" {
		return "dev"
	}
	return version
}

func NormalizedCommit() string {
	commit := strings.TrimSpace(Commit)
	if commit == "" {
		return "unknown"
	}
	return commit
}
