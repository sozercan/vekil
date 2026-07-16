package main

import "testing"

func TestVersionMenuTitle(t *testing.T) {
	original := buildVersion
	t.Cleanup(func() {
		buildVersion = original
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
			buildVersion = tc.version
			if got := versionMenuTitle(); got != tc.want {
				t.Fatalf("versionMenuTitle() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMetricsDisabledFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset defaults enabled", value: "", want: false},
		{name: "true disables", value: "true", want: true},
		{name: "one disables", value: "1", want: true},
		{name: "false keeps enabled", value: "false", want: false},
		{name: "invalid keeps enabled", value: "not-a-bool", want: false},
		{name: "whitespace wrapped value stays invalid like CLI", value: " true ", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NO_METRICS", tt.value)
			if got := metricsDisabledFromEnv(); got != tt.want {
				t.Fatalf("metricsDisabledFromEnv() = %t, want %t", got, tt.want)
			}
		})
	}
}
