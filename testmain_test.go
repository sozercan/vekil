package main

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		switch os.Getenv("MAIN_LAUNCH_HELPER_TARGET") {
		case "codex":
			_, _ = fmt.Fprintln(os.Stdout, "codex-cli 0.144.5")
		case "copilot":
			_, _ = fmt.Fprintln(os.Stdout, "GitHub Copilot CLI 1.0.71")
		default:
			_, _ = fmt.Fprintln(os.Stdout, "2.1.211 (Claude Code)")
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "debug" && os.Args[2] == "models" && os.Args[3] == "--bundled" {
		_, _ = fmt.Fprintln(os.Stdout, `{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5","description":"test template","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium","description":"Balanced"}],"shell_type":"shell_command","visibility":"list","supported_in_api":true,"priority":0,"base_instructions":"You are Codex.","context_window":128000,"max_context_window":128000,"effective_context_window_percent":95,"input_modalities":["text"],"supports_parallel_tool_calls":true}]}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
