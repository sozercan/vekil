package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPriceCatalogEstimate(t *testing.T) {
	catalog := newPriceCatalog(map[string]PriceEntry{
		"model-a": {InputPer1K: 0.01, OutputPer1K: 0.03},
	})
	got, ok := catalog.estimate("model-a", 2000, 500)
	if !ok {
		t.Fatal("estimate ok = false, want true")
	}
	want := 0.035
	if got != want {
		t.Fatalf("estimate = %v, want %v", got, want)
	}
	if _, ok := catalog.estimate("missing", 2000, 500); ok {
		t.Fatal("missing model should not produce a cost")
	}
}

func TestLoadPriceCatalogFileMergesOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(`{"models":{"gpt-5.4":{"input_per_1k":1,"output_per_1k":2},"custom":{"input_per_1k":3,"output_per_1k":4}}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	catalog, err := LoadPriceCatalogFile(path)
	if err != nil {
		t.Fatalf("LoadPriceCatalogFile() error = %v", err)
	}
	if got, ok := catalog.lookup("gpt-5.4"); !ok || got.InputPer1K != 1 || got.OutputPer1K != 2 {
		t.Fatalf("override gpt-5.4 = %+v, ok=%v", got, ok)
	}
	if got, ok := catalog.lookup("custom"); !ok || got.InputPer1K != 3 || got.OutputPer1K != 4 {
		t.Fatalf("custom = %+v, ok=%v", got, ok)
	}
	if _, ok := catalog.lookup("claude-sonnet-4.6"); !ok {
		t.Fatal("default catalog entry should remain after merge")
	}
}

func TestLoadPriceCatalogFileRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := LoadPriceCatalogFile(path)
	if err == nil || !strings.Contains(err.Error(), "decode prices") {
		t.Fatalf("LoadPriceCatalogFile() error = %v, want decode error", err)
	}
}

func TestLoadPriceCatalogFileRejectsMalformedWrappedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(`{"models":{"bad":{"input_per_1k":"oops","output_per_1k":2}}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := LoadPriceCatalogFile(path)
	if err == nil || !strings.Contains(err.Error(), "decode models") {
		t.Fatalf("LoadPriceCatalogFile() error = %v, want decode models error", err)
	}
}
