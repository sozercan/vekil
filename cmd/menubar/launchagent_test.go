package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestLaunchAgentProgramArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		executable string
		want       []string
	}{
		{
			name:       "app bundle executable uses open by bundle id",
			executable: "/Applications/Copilot Proxy.app/Contents/MacOS/copilot-proxy-menubar",
			want:       []string{"/usr/bin/open", "-b", appBundleID},
		},
		{
			name:       "standalone executable uses direct path",
			executable: "/usr/local/bin/copilot-proxy-menubar",
			want:       []string{"/usr/local/bin/copilot-proxy-menubar"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := launchAgentProgramArguments(tt.executable)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("launchAgentProgramArguments(%q) = %v, want %v", tt.executable, got, tt.want)
			}
		})
	}
}

func TestLaunchAgentPlistEscapesArguments(t *testing.T) {
	t.Parallel()

	plist, err := launchAgentPlist([]string{"/tmp/Copilot & Proxy", "--model=claude<sonnet>"})
	if err != nil {
		t.Fatalf("launchAgentPlist() error = %v", err)
	}

	wantFragments := []string{
		"<string>/tmp/Copilot &amp; Proxy</string>",
		"<string>--model=claude&lt;sonnet&gt;</string>",
		"<string>" + launchAgentLabel + "</string>",
	}

	for _, fragment := range wantFragments {
		if !strings.Contains(plist, fragment) {
			t.Fatalf("launchAgentPlist() missing fragment %q in %q", fragment, plist)
		}
	}
}
