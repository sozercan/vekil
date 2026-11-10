package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Keep the native Swift analytics fixtures grounded in the Go-owned public
// /stats.json contract. Swift still owns stale/prior-generation presentation,
// but every valid fixture must be a strict Go snapshot shape.
func TestNativeStatsFixturesMatchGoContract(t *testing.T) {
	fixtureDir := filepath.Join("..", "mac", "VekilApp", "Tests", "VekilCoreTests", "Fixtures")
	fixtures, err := filepath.Glob(filepath.Join(fixtureDir, "stats-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("native stats fixtures are missing")
	}
	for _, path := range fixtures {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if name == "stats-malformed.json" {
				var malformed any
				err = json.Unmarshal(body, &malformed)
				if err == nil {
					t.Fatal("malformed fixture decoded successfully")
				}
				return
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(body, &object); err != nil {
				t.Fatal(err)
			}
			// One fixture deliberately proves Swift ignores an additive future
			// top-level field. Remove only that sentinel before strict Go decoding.
			delete(object, "future_additive_field")
			strictBody, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(strictBody))
			decoder.DisallowUnknownFields()
			var snapshot statsSnapshot
			if err := decoder.Decode(&snapshot); err != nil {
				t.Fatalf("fixture does not match statsSnapshot: %v", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				t.Fatalf("fixture has trailing JSON: %v", err)
			}
		})
	}
}
