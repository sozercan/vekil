package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func conversationMigrationConfigFixture() ProvidersConfig {
	cfg := validAzureRouteConfig()
	secondary := cfg.Providers[0]
	secondary.ID = "azure-secondary"
	secondary.BaseURL = "https://secondary.openai.azure.com/openai/v1"
	cfg.Providers = append(cfg.Providers, secondary)
	cfg.ModelRoutes[0].Targets = append(cfg.ModelRoutes[0].Targets, ModelRouteTargetConfig{
		ID: "secondary", Provider: secondary.ID, UpstreamModel: "deployment-secondary",
	})
	cfg.ModelRoutes[0].Routing = ModelRouteRoutingConfig{
		Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2,
	}
	cfg.ConversationMigration = &ConversationMigrationConfig{Routes: []string{cfg.ModelRoutes[0].ID}}
	return cfg
}

func TestLoadProvidersConfigFileConversationMigration(t *testing.T) {
	for _, ext := range []string{".json", ".yaml"} {
		for _, tc := range []struct {
			name, migration, wantError string
			wantHistory, wantSnapshots int
			wantTotal                  int64
		}{
			{name: "omitted"},
			{name: "defaults", migration: `{"routes":["gpt-route"]}`, wantHistory: 8 << 20, wantTotal: 256 << 20, wantSnapshots: 4096},
			{name: "explicit limits", migration: `{"routes":["gpt-route"],"max_history_bytes":1024,"max_total_bytes":4096,"max_snapshots":2}`, wantHistory: 1024, wantTotal: 4096, wantSnapshots: 2},
			{name: "upper bounds", migration: fmt.Sprintf(`{"routes":["gpt-route"],"max_history_bytes":%d,"max_total_bytes":%d,"max_snapshots":%d}`, maxConversationMigrationHistoryBytes, maxConversationMigrationTotalBytes, maxConversationMigrationSnapshots), wantHistory: maxConversationMigrationHistoryBytes, wantTotal: maxConversationMigrationTotalBytes, wantSnapshots: maxConversationMigrationSnapshots},
			{name: "null block", migration: `null`, wantError: "conversation_migration: must be an object"},
			{name: "empty block", migration: `{}`, wantError: "conversation_migration.routes"},
			{name: "empty routes", migration: `{"routes":[]}`, wantError: "conversation_migration.routes"},
			{name: "null routes", migration: `{"routes":null}`, wantError: "conversation_migration.routes"},
			{name: "unknown route", migration: `{"routes":["unknown"]}`, wantError: "conversation_migration.routes[0]: references unknown operational route"},
			{name: "public ID is not a route ID", migration: `{"routes":["gpt-public"]}`, wantError: "conversation_migration.routes[0]: references unknown operational route"},
			{name: "duplicate route", migration: `{"routes":["gpt-route","gpt-route"]}`, wantError: "conversation_migration.routes[1]: duplicates conversation_migration.routes[0]"},
			{name: "empty route ID", migration: `{"routes":[""]}`, wantError: "conversation_migration.routes[0]"},
			{name: "whitespace route ID", migration: `{"routes":[" gpt-route"]}`, wantError: "conversation_migration.routes[0]"},
			{name: "unknown field", migration: `{"routes":["gpt-route"],"max_snapshot":1}`, wantError: "conversation_migration.max_snapshot: unknown field"},
			{name: "case variant field", migration: `{"routes":["gpt-route"],"Max_snapshots":1}`, wantError: "conversation_migration.Max_snapshots: unknown field"},
			{name: "zero history", migration: `{"routes":["gpt-route"],"max_history_bytes":0}`, wantError: "conversation_migration.max_history_bytes"},
			{name: "null history", migration: `{"routes":["gpt-route"],"max_history_bytes":null}`, wantError: "conversation_migration.max_history_bytes"},
			{name: "negative history", migration: `{"routes":["gpt-route"],"max_history_bytes":-1}`, wantError: "conversation_migration.max_history_bytes"},
			{name: "excessive history", migration: fmt.Sprintf(`{"routes":["gpt-route"],"max_history_bytes":%d}`, maxConversationMigrationHistoryBytes+1), wantError: "conversation_migration.max_history_bytes"},
			{name: "zero total", migration: `{"routes":["gpt-route"],"max_total_bytes":0}`, wantError: "conversation_migration.max_total_bytes"},
			{name: "null total", migration: `{"routes":["gpt-route"],"max_total_bytes":null}`, wantError: "conversation_migration.max_total_bytes"},
			{name: "negative total", migration: `{"routes":["gpt-route"],"max_total_bytes":-1}`, wantError: "conversation_migration.max_total_bytes"},
			{name: "excessive total", migration: fmt.Sprintf(`{"routes":["gpt-route"],"max_total_bytes":%d}`, maxConversationMigrationTotalBytes+1), wantError: "conversation_migration.max_total_bytes"},
			{name: "total smaller than history", migration: `{"routes":["gpt-route"],"max_history_bytes":1024,"max_total_bytes":512}`, wantError: "conversation_migration.max_total_bytes: must be at least max_history_bytes"},
			{name: "zero snapshots", migration: `{"routes":["gpt-route"],"max_snapshots":0}`, wantError: "conversation_migration.max_snapshots"},
			{name: "null snapshots", migration: `{"routes":["gpt-route"],"max_snapshots":null}`, wantError: "conversation_migration.max_snapshots"},
			{name: "negative snapshots", migration: `{"routes":["gpt-route"],"max_snapshots":-1}`, wantError: "conversation_migration.max_snapshots"},
			{name: "excessive snapshots", migration: fmt.Sprintf(`{"routes":["gpt-route"],"max_snapshots":%d}`, maxConversationMigrationSnapshots+1), wantError: "conversation_migration.max_snapshots"},
			{name: "fractional snapshots", migration: `{"routes":["gpt-route"],"max_snapshots":1.5}`, wantError: "conversation_migration.max_snapshots"},
		} {
			t.Run(ext+"/"+tc.name, func(t *testing.T) {
				cfg := conversationMigrationConfigFixture()
				cfg.ConversationMigration = nil
				body, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if err := json.Unmarshal(body, &document); err != nil {
					t.Fatal(err)
				}
				if tc.migration != "" {
					var migration any
					if err := json.Unmarshal([]byte(tc.migration), &migration); err != nil {
						t.Fatal(err)
					}
					if fields, ok := migration.(map[string]any); ok {
						for _, field := range []string{"max_history_bytes", "max_total_bytes", "max_snapshots"} {
							if value, ok := fields[field].(float64); ok && value == float64(int64(value)) {
								fields[field] = int64(value)
							}
						}
					}
					document["conversation_migration"] = migration
				}
				path := writeConversationMigrationConfig(t, ext, document)
				loaded, err := LoadProvidersConfigFile(path)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("load error = %v, want %s", err, tc.wantError)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc.migration == "" {
					if loaded.ConversationMigration != nil {
						t.Fatal("omitted migration block enabled migration")
					}
					return
				}
				migration := loaded.ConversationMigration
				if migration == nil || migration.MaxHistoryBytes != tc.wantHistory || migration.MaxTotalBytes != tc.wantTotal || migration.MaxSnapshots != tc.wantSnapshots {
					t.Fatalf("migration config = %+v", migration)
				}
				if len(migration.Routes) != 1 || migration.Routes[0] != "gpt-route" {
					t.Fatalf("migration routes = %v", migration.Routes)
				}
				if err := ValidateProvidersConfig(loaded); err != nil {
					t.Fatalf("validate loaded config: %v", err)
				}
			})
		}
	}
}

func TestConversationMigrationConfigRouteValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*ProvidersConfig)
		wantError string
	}{
		{name: "different Azure deployments", change: func(*ProvidersConfig) {}},
		{name: "Copilot backup", change: func(cfg *ProvidersConfig) {
			cfg.Providers[1] = ProviderConfig{ID: "copilot", Type: "copilot"}
			cfg.ModelRoutes[0].Targets[1].Provider = "copilot"
		}},
		{name: "Copilot source", change: func(cfg *ProvidersConfig) {
			cfg.Providers[0] = ProviderConfig{ID: "copilot", Type: "copilot"}
			cfg.ModelRoutes[0].Targets[0].Provider = "copilot"
		}},
		{name: "same provider different deployments", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Targets[1].Provider = cfg.Providers[0].ID
			cfg.Providers = cfg.Providers[:1]
		}},
		{name: "explicit durable mode", change: func(cfg *ProvidersConfig) {
			cfg.StateBindings = &StateBindingsConfig{Mode: "durable"}
		}},
		{name: "empty state binding block uses durable default", change: func(cfg *ProvidersConfig) {
			cfg.StateBindings = &StateBindingsConfig{}
		}},
		{name: "schema v1", change: func(cfg *ProvidersConfig) {
			cfg.SchemaVersion = 1
			cfg.ModelRoutes = nil
		}, wantError: "conversation_migration: requires schema_version: 2"},
		{name: "memory mode", change: func(cfg *ProvidersConfig) {
			cfg.StateBindings = &StateBindingsConfig{Mode: " memory "}
		}, wantError: "conversation_migration: requires durable state_bindings"},
		{name: "internal route", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Exposure = modelRouteExposureInternal
			cfg.ModelRoutes[0].PublicID = ""
		}, wantError: "conversation_migration.routes[0]: route \"gpt-route\" must have public exposure"},
		{name: "Chat only route", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Endpoints = []string{providerEndpointChatCompletions}
		}, wantError: "conversation_migration.routes[0]: route \"gpt-route\" must support \"/responses\""},
		{name: "primary only route", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Routing = ModelRouteRoutingConfig{Mode: string(routeModePrimaryOnly)}
		}, wantError: "conversation_migration.routes[0]: route \"gpt-route\" must use routing.mode \"priority_failover\""},
		{name: "single target", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Targets = cfg.ModelRoutes[0].Targets[:1]
			cfg.ModelRoutes[0].Routing.MaxTargetAttempts = 1
			cfg.Providers = cfg.Providers[:1]
		}, wantError: "conversation_migration.routes[0]: route \"gpt-route\" must have at least two targets"},
		{name: "insufficient target budget", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Routing.MaxTargetAttempts = 1
		}, wantError: "conversation_migration.routes[0]: route \"gpt-route\" must allow at least two target attempts and upstream sends"},
		{name: "insufficient send budget", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes[0].Routing.MaxUpstreamSends = 1
		}, wantError: "model_routes[0].routing.max_upstream_sends"},
		{name: "unsupported target provider", change: func(cfg *ProvidersConfig) {
			cfg.Providers[1].Type = string(providerTypeOpenAICompatible)
		}, wantError: "conversation_migration.routes[0]: route \"gpt-route\" target \"secondary\" must use an azure-openai or copilot provider"},
		{name: "excessive route list", change: func(cfg *ProvidersConfig) {
			cfg.ConversationMigration.Routes = make([]string, maxExplicitModelRoutes+1)
		}, wantError: "conversation_migration.routes: contains 257 routes; maximum is 256"},
		{name: "unselected route is unrestricted", change: func(cfg *ProvidersConfig) {
			cfg.ModelRoutes = append(cfg.ModelRoutes, ModelRouteConfig{
				ID: "other-route", PublicID: "other-public", Endpoints: []string{providerEndpointChatCompletions},
				Targets: []ModelRouteTargetConfig{{ID: "other-target", Provider: "azure", UpstreamModel: "other-deployment"}},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := conversationMigrationConfigFixture()
			tc.change(&cfg)
			err := ValidateProvidersConfig(cfg)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("validation error = %v, want %s", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadProvidersConfigFileConversationMigrationSchemaAndDuplicates(t *testing.T) {
	for _, ext := range []string{".json", ".yaml"} {
		for _, version := range []int{0, 1} {
			for _, migration := range []any{nil, map[string]any{"routes": []string{"route"}}} {
				document := map[string]any{"providers": []any{}, "conversation_migration": migration}
				if version != 0 {
					document["schema_version"] = version
				}
				_, err := LoadProvidersConfigFile(writeConversationMigrationConfig(t, ext, document))
				if err == nil || !strings.Contains(err.Error(), "conversation_migration: requires schema_version: 2") {
					t.Fatalf("%s schema %d migration %v: %v", ext, version, migration, err)
				}
			}
		}
		body := `{"schema_version":2,"providers":[],"conversation_migration":{"max_snapshots":1,"max_snapshots":2}}`
		if ext == ".yaml" {
			body = "schema_version: 2\nproviders: []\nconversation_migration:\n  max_snapshots: 1\n  max_snapshots: 2\n"
		}
		path := filepath.Join(t.TempDir(), "providers"+ext)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadProvidersConfigFile(path)
		if err == nil || !strings.Contains(err.Error(), "conversation_migration.max_snapshots: duplicate mapping key") {
			t.Fatalf("duplicate field error = %v", err)
		}
	}
}

func TestConversationMigrationConfigValidationIsOfflineAndCloned(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer upstream.Close()
	cfg := conversationMigrationConfigFixture()
	for index := range cfg.Providers {
		cfg.Providers[index].BaseURL = upstream.URL + "/openai/v1"
	}
	stateDir := filepath.Join(t.TempDir(), "unopened")
	cfg.StateBindings = &StateBindingsConfig{File: filepath.Join(stateDir, "bindings.db")}
	if err := ValidateProvidersConfigFile(writeConversationMigrationConfig(t, ".json", cfg)); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("offline validation made %d upstream requests", got)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("offline validation created state directory: %v", err)
	}
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConversationMigration.MaxHistoryBytes != 0 || cfg.ConversationMigration.MaxTotalBytes != 0 || cfg.ConversationMigration.MaxSnapshots != 0 {
		t.Fatal("validation changed the caller's migration limits")
	}
	validated.config.ConversationMigration.Routes[0] = "changed"
	if cfg.ConversationMigration.Routes[0] != "gpt-route" {
		t.Fatal("validation retained the caller's migration route slice")
	}
}

func writeConversationMigrationConfig(t *testing.T, ext string, config any) string {
	t.Helper()
	var body []byte
	var err error
	if ext == ".yaml" {
		body, err = yaml.Marshal(config)
	} else {
		body, err = json.Marshal(config)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "providers"+ext)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
