package launch

import (
	"os"
	"strings"
	"testing"
)

func TestCodexLaunchProviderConfigCheck(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, config, wantError string
	}{
		{"unrelated provider", `{"id":2,"result":{"config":{"model_providers":{"other":{}}}}}`, ""},
		{"reserved provider", `{"id":2,"result":{"config":{"model_providers":{"vekil_launch":{"http_headers":{"Example":"not-for-logs"}}}}}}`, "reserved by Vekil"},
		{"invalid response", `{"id":2,"error":{"message":"not-for-logs"}}`, "missing effective configuration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := append(os.Environ(), "LAUNCH_TEST_CODEX_CONFIG_RESPONSE="+test.config)
			err := checkCodexLaunchProvider(resolvedExecutable{path: executable}, env, t.TempDir())
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) || strings.Contains(err.Error(), "not-for-logs") {
				t.Fatalf("expected a redacted %q error, got %v", test.wantError, err)
			}
		})
	}
}

func TestCodexConfigDirectory(t *testing.T) {
	for _, args := range [][]string{
		{"-C", "workspace", "exec", "test"},
		{"exec", "--cd=workspace", "test"},
		{"exec", "-Cworkspace", "--", "-Cignored"},
		{"--image", "-Cignored", "--cd", "workspace"},
	} {
		if directory := codexConfigDirectory(args); directory != "workspace" {
			t.Fatalf("directory for %v = %q", args, directory)
		}
	}
}
