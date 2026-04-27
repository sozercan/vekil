package version

import "strings"

// Build is injected by build ldflags. Local builds fall back to "dev".
var Build = "dev"

func String() string {
	build := strings.TrimSpace(Build)
	if build == "" {
		return "dev"
	}
	return build
}
