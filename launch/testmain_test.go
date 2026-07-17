package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		if capturePath := os.Getenv("LAUNCH_VERSION_ENV_CAPTURE"); capturePath != "" {
			body, err := json.Marshal(map[string]string{
				"api_key":        os.Getenv("ANTHROPIC_API_KEY"),
				"auth_token":     os.Getenv("ANTHROPIC_AUTH_TOKEN"),
				"base_url":       os.Getenv("ANTHROPIC_BASE_URL"),
				"custom_headers": os.Getenv("ANTHROPIC_CUSTOM_HEADERS"),
				"provider_token": os.Getenv("MY_PROVIDER_TOKEN"),
			})
			if err != nil || os.WriteFile(capturePath, body, 0o600) != nil {
				os.Exit(98)
			}
		}
		_, _ = fmt.Fprintln(os.Stdout, "2.1.211 (Claude Code)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}
