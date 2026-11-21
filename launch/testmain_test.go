package launch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "app-server" {
		if response := os.Getenv("LAUNCH_TEST_CODEX_CONFIG_RESPONSE"); response != "" {
			if expected := os.Getenv("LAUNCH_TEST_CODEX_EXPECT_HOME"); expected != "" && os.Getenv("CODEX_HOME") != expected {
				os.Exit(98)
			}
			input := bufio.NewScanner(os.Stdin)
			for range 3 {
				if !input.Scan() {
					os.Exit(1)
				}
			}
			_, _ = fmt.Fprintln(os.Stdout, response)
			os.Exit(0)
		}
	}
	if len(os.Args) == 2 && os.Args[1] == "--vekil-test-hang" {
		_, _ = fmt.Fprintln(os.Stdout, "partial output")
		time.Sleep(time.Hour)
		os.Exit(0)
	}
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
