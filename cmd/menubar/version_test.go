package main

import (
	"testing"

	version "github.com/sozercan/vekil/version"
)

func TestVersionMenuTitle(t *testing.T) {
	original := version.Build
	t.Cleanup(func() {
		version.Build = original
	})

	tests := []struct {
		name    string
		version string
		want    string
	}{
		{
			name:    "uses injected version",
			version: "1.2.3",
			want:    "Version 1.2.3",
		},
		{
			name:    "falls back for blank version",
			version: " ",
			want:    "Version dev",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version.Build = tc.version
			if got := versionMenuTitle(); got != tc.want {
				t.Fatalf("versionMenuTitle() = %q, want %q", got, tc.want)
			}
		})
	}
}
