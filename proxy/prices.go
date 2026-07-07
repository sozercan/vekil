package proxy

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

//go:embed prices.json
var defaultPricesFS embed.FS

type PriceEntry struct {
	InputPer1K  float64 `json:"input_per_1k"`
	OutputPer1K float64 `json:"output_per_1k"`
}

type priceCatalog struct {
	models map[string]PriceEntry
}

func defaultPriceCatalog() *priceCatalog {
	body, err := defaultPricesFS.ReadFile("prices.json")
	if err != nil {
		return &priceCatalog{models: map[string]PriceEntry{}}
	}
	catalog, err := decodePriceCatalog(body)
	if err != nil {
		return &priceCatalog{models: map[string]PriceEntry{}}
	}
	return catalog
}

func LoadPriceCatalogFile(path string) (*priceCatalog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return defaultPriceCatalog(), nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read prices %q: %w", path, err)
	}
	override, err := decodePriceCatalog(body)
	if err != nil {
		return nil, fmt.Errorf("decode prices %q: %w", path, err)
	}
	merged := defaultPriceCatalog()
	if merged.models == nil {
		merged.models = make(map[string]PriceEntry, len(override.models))
	}
	for model, price := range override.models {
		merged.models[model] = price
	}
	return merged, nil
}

func decodePriceCatalog(body []byte) (*priceCatalog, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	if rawModels, ok := top["models"]; ok {
		var models map[string]PriceEntry
		if err := json.Unmarshal(rawModels, &models); err != nil {
			return nil, fmt.Errorf("decode models: %w", err)
		}
		return newPriceCatalog(models), nil
	}
	var flat map[string]PriceEntry
	if err := json.Unmarshal(body, &flat); err != nil {
		return nil, err
	}
	return newPriceCatalog(flat), nil
}

func newPriceCatalog(models map[string]PriceEntry) *priceCatalog {
	out := make(map[string]PriceEntry, len(models))
	for model, price := range models {
		model = strings.TrimSpace(model)
		if model == "" || price.InputPer1K < 0 || price.OutputPer1K < 0 {
			continue
		}
		out[model] = price
	}
	return &priceCatalog{models: out}
}

func (c *priceCatalog) estimate(model string, promptTokens, completionTokens int) (float64, bool) {
	if c == nil || len(c.models) == 0 || (promptTokens == 0 && completionTokens == 0) {
		return 0, false
	}
	price, ok := c.lookup(model)
	if !ok {
		return 0, false
	}
	cost := float64(promptTokens)/1000*price.InputPer1K + float64(completionTokens)/1000*price.OutputPer1K
	return cost, true
}

func (c *priceCatalog) lookup(model string) (PriceEntry, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return PriceEntry{}, false
	}
	if price, ok := c.models[model]; ok {
		return price, true
	}
	if normalized := NormalizeModelName(model); normalized != model {
		if price, ok := c.models[normalized]; ok {
			return price, true
		}
	}
	return PriceEntry{}, false
}
