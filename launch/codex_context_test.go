package launch

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCodexAdapterPromptContextBudget(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		metadata      string
		prompt, total int64
	}{
		{"prompt limit", `{"capabilities":{"limits":{"max_prompt_tokens":128000,"max_context_window_tokens":400000}}}`, 128000, 400000},
		{"prompt alias", `{"capabilities":{"limits":{"max_prompt":96000,"context_window":200000}}}`, 96000, 200000},
		{"input alias", `{"capabilities":{"limits":{"max_input_tokens":64000,"context_window_tokens":128000}}}`, 64000, 128000},
		{"explicit public contract", `{"context_window":100000,"max_context_window":300000,"capabilities":{"limits":{"max_prompt_tokens":128000,"max_context_window_tokens":400000}}}`, 100000, 300000},
		{"total fallback", `{"capabilities":{"limits":{"max_context_window_tokens":400000}}}`, 400000, 400000},
		{"ignore invalid limits", `{"capabilities":{"limits":{"max_prompt_tokens":-1,"max_prompt":0,"max_input_tokens":32000,"max_context_window_tokens":64000}}}`, 32000, 64000},
	} {
		t.Run(test.name, func(t *testing.T) {
			var model ModelInfo
			if err := json.Unmarshal([]byte(test.metadata), &model); err != nil {
				t.Fatal(err)
			}
			model.ID = "context-model"
			model.SupportedEndpoints = []string{"/responses"}
			prepared, err := (CodexAdapter{}).Prepare(PrepareInput{BaseURL: "http://127.0.0.1:43210", Model: model, Binary: binary, LocalToken: "test", DryRun: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := prepared.Cleanup(); err != nil {
					t.Error(err)
				}
			})
			body, err := os.ReadFile(codexCatalogPathFromArgs(t, prepared.Args))
			if err != nil {
				t.Fatal(err)
			}
			var catalog codexCatalog
			if err := json.Unmarshal(body, &catalog); err != nil {
				t.Fatal(err)
			}
			got := catalog.Models[0]
			if got["context_window"] != float64(test.prompt) || got["max_context_window"] != float64(test.total) {
				t.Fatalf("catalog context = %v / %v, want %d / %d", got["context_window"], got["max_context_window"], test.prompt, test.total)
			}
		})
	}
}
