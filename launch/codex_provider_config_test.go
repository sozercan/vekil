package launch

import (
	"os"
	"path/filepath"
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

func TestCodexProbeKeepsRelativeHomeWhenWorkspaceDiffers(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	launcher, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	environment := applyEnvironment(os.Environ(), nil, map[string]string{
		"CODEX_HOME":                        "codex-home",
		"LAUNCH_TEST_CODEX_EXPECT_HOME":     filepath.Join(launcher, "codex-home"),
		"LAUNCH_TEST_CODEX_CONFIG_RESPONSE": `{"id":2,"result":{"config":{"model_providers":{"vekil_launch":{}}}}}`,
	})
	err = checkCodexLaunchProvider(resolvedExecutable{path: executable}, environment, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "reserved by Vekil") {
		t.Fatalf("expected reserved provider in launcher's Codex home, got %v", err)
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
