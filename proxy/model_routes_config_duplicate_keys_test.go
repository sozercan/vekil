package proxy

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestRejectDuplicateJSONMappingKeysReportsConfigPaths(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "top level", body: `{"providers":[],"providers":[]}`, want: `providers: duplicate mapping key "providers"`},
		{name: "nested array item", body: `{"a":[{"b":1},{"c":{"x":1,"x":2}}]}`, want: `a[1].c.x: duplicate mapping key "x"`},
		{name: "quoted key", body: `{"a":[{"b":1},{"c.d":{"x":1,"x":2}}]}`, want: `a[1]["c.d"].x: duplicate mapping key "x"`},
		{name: "non-simple root key", body: `{"c.d":{"x":1,"x":2}}`, want: `c.d.x: duplicate mapping key "x"`},
		{name: "empty root key", body: `{"":{"x":1,"x":2}}`, want: `x: duplicate mapping key "x"`},
		{name: "root array", body: `[0,[{"x":1,"x":2}]]`, want: `[1][0].x: duplicate mapping key "x"`},
		{name: "sibling scopes are independent", body: `{"a":{"x":1},"b":{"x":1},"c":[{"x":1},{"x":1,"x":2}]}`, want: `c[1].x: duplicate mapping key "x"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectDuplicateJSONMappingKeys([]byte(tc.body))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("rejectDuplicateJSONMappingKeys() error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := rejectDuplicateJSONMappingKeys([]byte(`{"a":{"x":1},"b":[{"x":1},{"x":1}]}`)); err != nil {
		t.Fatalf("rejectDuplicateJSONMappingKeys(unique) error = %v", err)
	}
}

func TestRejectDuplicateJSONMappingKeysBoundsNestingDepth(t *testing.T) {
	nested := func(depth int) []byte {
		return []byte(strings.Repeat(`[`, depth) + strings.Repeat(`]`, depth))
	}
	if err := rejectDuplicateJSONMappingKeys(nested(maxJSONDuplicateKeyScanDepth)); err != nil {
		t.Fatalf("depth %d error = %v", maxJSONDuplicateKeyScanDepth, err)
	}
	for _, scan := range []struct {
		name string
		fn   func([]byte) error
	}{
		{name: "paths", fn: rejectDuplicateJSONMappingKeys},
		{name: "path-free", fn: validateUnambiguousResponsesJSON},
	} {
		if err := scan.fn(nested(1 << 20)); err == nil {
			t.Fatalf("%s: over-deep JSON accepted", scan.name)
		}
	}
	if err := rejectDuplicateJSONMappingKeys(nested(maxJSONDuplicateKeyScanDepth + 1)); err == nil {
		t.Fatalf("depth %d accepted", maxJSONDuplicateKeyScanDepth+1)
	}
}

func TestExplicitRouteDeepRequestDuplicateKeyValidationIsLinear(t *testing.T) {
	const childEnv = "VEKIL_DUPLICATE_KEY_ALLOCATION_TEST_CHILD"
	if os.Getenv(childEnv) != "1" {
		child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExplicitRouteDeepRequestDuplicateKeyValidationIsLinear$")
		child.Env = append(child.Environ(), childEnv+"=1")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("isolated duplicate-key allocation test failed: %v\n%s", err, output)
		}
		return
	}
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: 2,
		StateBindings: &StateBindingsConfig{Mode: "memory"},
		Providers:     []ProviderConfig{{ID: "azure", Type: string(providerTypeAzureOpenAI), Default: true, BaseURL: "https://example.invalid/openai/v1", APIKey: "key"}},
		ModelRoutes: []ModelRouteConfig{{
			ID: "route", PublicID: "public-model", Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "azure", UpstreamModel: "physical"}},
		}},
	}))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()

	const depth = 9000
	request := func(leaf string) []byte {
		schema := strings.Repeat(`{"properties":`, depth) + leaf + strings.Repeat(`}`, depth)
		return []byte(`{"model":"public-model","input":"hi","tools":[{"type":"function","name":"run","parameters":` + schema + `}]}`)
	}

	body := request(`{}`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := h.validateRouteAwareRequestJSON(body, "public-model", providerEndpointResponses); err != nil {
		t.Fatalf("validateRouteAwareRequestJSON() error = %v", err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("validation allocated %d MiB for a %d KiB request", allocated>>20, len(body)>>10)
	}

	err = h.validateRouteAwareRequestJSON(request(`{"type":"object","type":"string"}`), "public-model", providerEndpointResponses)
	var requestErr *providerRequestError
	if !errors.As(err, &requestErr) || requestErr.statusCode != http.StatusBadRequest {
		t.Fatalf("validateRouteAwareRequestJSON(duplicate) error = %v, want 400 providerRequestError", err)
	}
	wantPath := "tools[0].parameters" + strings.Repeat(".properties", depth) + `.type: duplicate mapping key "type"`
	if !strings.HasSuffix(err.Error(), wantPath) {
		t.Fatalf("duplicate error does not end with the full path; got %d bytes ending %q", len(err.Error()), err.Error()[max(0, len(err.Error())-80):])
	}
}
