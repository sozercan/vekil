package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Version and Commit can be injected at build time with -ldflags. When they
// are left blank, Vekil falls back to Go's embedded build metadata.
var (
	Version string
	Commit  string
)

// Info describes build metadata exposed both via --version and Prometheus.
type Info struct {
	Version   string
	Commit    string
	GoVersion string
}

// Current returns the best available build metadata.
func Current() Info {
	info := Info{
		Version:   strings.TrimSpace(Version),
		Commit:    strings.TrimSpace(Commit),
		GoVersion: strings.TrimSpace(runtime.Version()),
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok && buildInfo != nil {
		if info.Version == "" {
			version := strings.TrimSpace(buildInfo.Main.Version)
			if version != "" && version != "(devel)" {
				info.Version = version
			}
		}
		if info.Commit == "" {
			info.Commit = vcsRevision(buildInfo.Settings)
		}
		if info.GoVersion == "" {
			info.GoVersion = strings.TrimSpace(buildInfo.GoVersion)
		}
	}

	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.GoVersion == "" {
		info.GoVersion = "unknown"
	}

	return info
}

// String returns a human-readable version string.
func String() string {
	info := Current()
	return fmt.Sprintf("vekil %s (%s)", info.Version, info.Commit)
}

func vcsRevision(settings []debug.BuildSetting) string {
	if len(settings) == 0 {
		return ""
	}

	revision := ""
	modified := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = strings.TrimSpace(setting.Value) == "true"
		}
	}

	if revision == "" {
		return ""
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
}
