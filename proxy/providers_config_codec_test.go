package proxy

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDecodeProvidersConfigJSONMatchesFileJSONDecoder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantCode ConfigErrorCode
	}{
		{
			name: "explicit presence and zeros",
			body: `{
				"schema_version": 2,
				"providers": [{"id":"p","type":"copilot","trust_domain":"","classifier_no_store_supported":false}],
				"model_routes": [{
					"id":"r","public_id":"m","endpoints":[],"targets":[],
					"routing":{"mode":"","max_target_attempts":0,"max_upstream_sends":0},
					"model_picker_enabled":false
				}],
				"policy_profiles": [{
					"id":"policy","public_id":"policy-model","lightweight_route":"r","powerful_route":"r",
					"classifier":{"route":"classifier","recent_turns":0,"observe_sample_rate":0,"timeout_ms":null},
					"data_policy":{"content_forwarding_acknowledged":false}
				}],
				"tool_optimizers":{"enabled":false,"output_reduce":{"enabled":false,"timeout_ms":0,"min_input_bytes":0,"max_input_bytes":0}}
			}`,
		},
		{
			name: "explicit schema zero",
			body: `{"schema_version":0,"providers":[]}`,
		},
		{
			name:     "duplicate nested field",
			body:     `{"providers":[{"id":"p","id":"q","type":"copilot"}]}`,
			wantCode: ConfigErrorDuplicateField,
		},
		{
			name:     "unknown nested field",
			body:     `{"providers":[{"id":"p","type":"copilot","mystery":true}]}`,
			wantCode: ConfigErrorUnknownField,
		},
		{
			name:     "trailing value",
			body:     `{"providers":[]} {}`,
			wantCode: ConfigErrorTrailingValue,
		},
		{
			name:     "top level null",
			body:     `null`,
			wantCode: ConfigErrorInvalidJSON,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var legacy ProvidersConfig
			legacyErr := decodeProvidersConfigFile("providers.json", []byte(tc.body), &legacy)
			got, gotErr := DecodeProvidersConfigJSON([]byte(tc.body))

			if (legacyErr != nil) != (gotErr != nil) {
				t.Fatalf("error parity mismatch: legacy=%v exported=%v", legacyErr, gotErr)
			}
			if gotErr != nil {
				var typed *ConfigError
				if !errors.As(gotErr, &typed) {
					t.Fatalf("DecodeProvidersConfigJSON() error = %T, want *ConfigError", gotErr)
				}
				if typed.Code != tc.wantCode {
					t.Fatalf("error code = %q, want %q (error %v)", typed.Code, tc.wantCode, gotErr)
				}
				return
			}
			assertProvidersConfigPresenceEqual(t, legacy, got)
		})
	}
}

func TestDecodeProvidersConfigJSONTypedPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		wantCode    ConfigErrorCode
		wantPointer string
	}{
		{
			name:        "unknown route field",
			body:        `{"schema_version":2,"providers":[],"model_routes":[{"id":"r","public_id":"m","endpoints":[],"targets":[],"weight":1}]}`,
			wantCode:    ConfigErrorUnknownField,
			wantPointer: "/model_routes/0/weight",
		},
		{
			name:        "duplicate extra header",
			body:        `{"providers":[{"id":"p","type":"copilot","extra_headers":{"X/A":"one","X/A":"two"}}]}`,
			wantCode:    ConfigErrorDuplicateField,
			wantPointer: "/providers/0/extra_headers/X~1A",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeProvidersConfigJSON([]byte(tc.body))
			var typed *ConfigError
			if !errors.As(err, &typed) {
				t.Fatalf("error = %v, want *ConfigError", err)
			}
			if typed.Code != tc.wantCode || typed.Pointer != tc.wantPointer {
				t.Fatalf("error = code %q pointer %q, want %q %q", typed.Code, typed.Pointer, tc.wantCode, tc.wantPointer)
			}
		})
	}

	cfg, err := DecodeProvidersConfigJSON([]byte(`{"schema_version":0,"providers":[]}`))
	if err != nil {
		t.Fatalf("DecodeProvidersConfigJSON() error = %v", err)
	}
	err = ValidateProvidersConfigTyped(cfg)
	var typed *ConfigError
	if !errors.As(err, &typed) {
		t.Fatalf("ValidateProvidersConfigTyped() error = %v, want *ConfigError", err)
	}
	if typed.Code != ConfigErrorInvalidConfig || typed.Pointer != "/schema_version" {
		t.Fatalf("validation error = code %q pointer %q", typed.Code, typed.Pointer)
	}
}

func TestEncodeProvidersConfigJSONPreservesBehaviorSignificantPresence(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"schema_version":2,
		"providers":[{"id":"p","type":"copilot","trust_domain":"","classifier_no_store_supported":false}],
		"model_routes":[{
			"id":"r","exposure":"","internal_purpose":"","public_id":"m","model_picker_enabled":false,"model_picker_category":"",
			"endpoints":[],"targets":[],
			"routing":{"mode":"","max_target_attempts":0,"max_upstream_sends":0}
		}],
		"policy_profiles":[{
			"id":"policy","public_id":"policy-model","lightweight_route":"r","powerful_route":"r",
			"classifier":{
				"route":"classifier","timeout_ms":null,"max_completion_tokens":0,"max_request_bytes":0,
				"recent_turns":0,"max_concurrency":0,"observe_sample_rate":0
			},
			"data_policy":{"content_forwarding_acknowledged":false,"allow_cross_trust_domain":false,"allow_provider_retention":false}
		}],
		"tool_optimizers":{
			"enabled":false,
			"tools":{"shell_function_calls":{"enabled":false,"names":[]}},
			"command_rewrite":{"enabled":false},
			"output_reduce":{"enabled":false,"timeout_ms":0,"min_input_bytes":0,"max_input_bytes":0}
		}
	}`)

	first, err := DecodeProvidersConfigJSON(raw)
	if err != nil {
		t.Fatalf("DecodeProvidersConfigJSON() error = %v", err)
	}
	encoded, err := EncodeProvidersConfigJSON(first)
	if err != nil {
		t.Fatalf("EncodeProvidersConfigJSON() error = %v", err)
	}
	second, err := DecodeProvidersConfigJSON(encoded)
	if err != nil {
		t.Fatalf("round-trip DecodeProvidersConfigJSON() error = %v\n%s", err, encoded)
	}
	encodedAgain, err := EncodeProvidersConfigJSON(second)
	if err != nil {
		t.Fatalf("second EncodeProvidersConfigJSON() error = %v", err)
	}
	if !bytes.Equal(encoded, encodedAgain) {
		t.Fatalf("canonical encoding is not stable:\nfirst  %s\nsecond %s", encoded, encodedAgain)
	}
	assertProvidersConfigPresenceEqual(t, first, second)

	routing := second.ModelRoutes[0].Routing
	if !routing.modeSet || !routing.maxTargetAttemptsSet || !routing.maxUpstreamSendsSet {
		t.Fatalf("route routing presence lost: %+v", routing)
	}
	classifier := second.PolicyProfiles[0].Classifier
	if !classifier.timeoutMSNull || !classifier.recentTurnsSet || classifier.RecentTurns != 0 || !classifier.observeSampleRateSet || classifier.ObserveSampleRate != 0 {
		t.Fatalf("policy classifier presence/zeros lost: %+v", classifier)
	}
	output := second.ToolOptimizers.OutputReduce
	if !output.timeoutMSSet || !output.minInputBytesSet || !output.maxInputBytesSet || output.TimeoutMS != 0 || output.MinInputBytes != 0 || output.MaxInputBytes != 0 {
		t.Fatalf("tool optimizer output zeros lost: %+v", output)
	}
	if second.ModelRoutes[0].ModelPickerEnabled == nil || *second.ModelRoutes[0].ModelPickerEnabled {
		t.Fatalf("explicit false model_picker_enabled lost: %v", second.ModelRoutes[0].ModelPickerEnabled)
	}
	if second.Providers[0].ClassifierNoStoreSupported == nil || *second.Providers[0].ClassifierNoStoreSupported {
		t.Fatalf("explicit false classifier_no_store_supported lost: %v", second.Providers[0].ClassifierNoStoreSupported)
	}
}

func TestEncodeProvidersConfigJSONSemanticRoundTripV1V2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "schema v1",
			body: `{"providers":[{"id":"copilot","type":"copilot","default":true}]}`,
		},
		{
			name: "schema v2",
			body: `{
				"schema_version":2,
				"providers":[{
					"id":"upstream","type":"openai-compatible","base_url":"https://example.test/v1",
					"auth_type":"none","model_discovery":"static"
				}],
				"model_routes":[{
					"id":"route","public_id":"model","endpoints":["/responses"],
					"targets":[{"id":"primary","provider":"upstream","upstream_model":"model"}]
				}]
			}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeProvidersConfigJSON([]byte(tc.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := ValidateProvidersConfigTyped(cfg); err != nil {
				t.Fatalf("original validation: %v", err)
			}
			body, err := EncodeProvidersConfigJSON(cfg)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			roundTrip, err := DecodeProvidersConfigJSON(body)
			if err != nil {
				t.Fatalf("round-trip decode: %v\n%s", err, body)
			}
			if err := ValidateProvidersConfigTyped(roundTrip); err != nil {
				t.Fatalf("round-trip validation: %v\n%s", err, body)
			}
			firstRevision, err := ProvidersConfigRevision(cfg)
			if err != nil {
				t.Fatal(err)
			}
			secondRevision, err := ProvidersConfigRevision(roundTrip)
			if err != nil {
				t.Fatal(err)
			}
			if firstRevision != secondRevision {
				t.Fatalf("revision changed across canonical round trip: %q != %q", firstRevision, secondRevision)
			}
		})
	}
}

func TestManagedProvidersConfigEnvelopeStrictCodec(t *testing.T) {
	t.Parallel()

	cfg := ProvidersConfig{}
	digest, err := ProvidersConfigDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	source := ProvidersConfigSource{
		Kind:            ProvidersConfigSourceImplicitCopilot,
		ID:              string(ProvidersConfigSourceImplicitCopilot),
		BootstrapDigest: digest,
	}
	envelope, err := NewManagedProvidersConfigEnvelope(source, cfg)
	if err != nil {
		t.Fatalf("NewManagedProvidersConfigEnvelope() error = %v", err)
	}
	body, err := EncodeManagedProvidersConfigJSON(envelope)
	if err != nil {
		t.Fatalf("EncodeManagedProvidersConfigJSON() error = %v", err)
	}
	decoded, err := DecodeManagedProvidersConfigJSON(body)
	if err != nil {
		t.Fatalf("DecodeManagedProvidersConfigJSON() error = %v\n%s", err, body)
	}
	if decoded.Source != envelope.Source || decoded.Revision != envelope.Revision || decoded.ManagedSchemaVersion != ManagedProvidersConfigSchemaVersion {
		t.Fatalf("decoded envelope metadata = %+v, want %+v", decoded, envelope)
	}

	tests := []struct {
		name        string
		body        string
		wantCode    ConfigErrorCode
		wantPointer string
	}{
		{
			name:        "unknown envelope field",
			body:        strings.Replace(string(body), `"revision":`, `"mystery":true,"revision":`, 1),
			wantCode:    ConfigErrorUnknownField,
			wantPointer: "/mystery",
		},
		{
			name:        "unknown source field",
			body:        strings.Replace(string(body), `"id":`, `"extra":true,"id":`, 1),
			wantCode:    ConfigErrorUnknownField,
			wantPointer: "/source/extra",
		},
		{
			name:        "duplicate revision",
			body:        strings.Replace(string(body), `"revision":`, `"revision":"`+envelope.Revision+`","revision":`, 1),
			wantCode:    ConfigErrorDuplicateField,
			wantPointer: "/revision",
		},
		{
			name:        "unknown nested config field",
			body:        strings.Replace(string(body), `"providers":`, `"unknown":true,"providers":`, 1),
			wantCode:    ConfigErrorUnknownField,
			wantPointer: "/config/unknown",
		},
		{
			name:        "trailing value",
			body:        string(body) + `{}`,
			wantCode:    ConfigErrorTrailingValue,
			wantPointer: "",
		},
		{
			name:        "unsupported schema",
			body:        strings.Replace(string(body), `"managed_schema_version":1`, `"managed_schema_version":2`, 1),
			wantCode:    ConfigErrorUnsupportedManagedSchema,
			wantPointer: "/managed_schema_version",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeManagedProvidersConfigJSON([]byte(tc.body))
			var typed *ConfigError
			if !errors.As(err, &typed) {
				t.Fatalf("error = %v, want *ConfigError", err)
			}
			if typed.Code != tc.wantCode || typed.Pointer != tc.wantPointer {
				t.Fatalf("error = code %q pointer %q, want %q %q (error %v)", typed.Code, typed.Pointer, tc.wantCode, tc.wantPointer, err)
			}
		})
	}
}

func TestConfigPathToJSONPointer(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"schema_version": "/schema_version",
		"policy_profiles[0].classifier.recent_turns":     "/policy_profiles/0/classifier/recent_turns",
		`providers[1].extra_headers["X/A~B"]`:            "/providers/1/extra_headers/X~1A~0B",
		"model_routes[12].targets[3].upstream_model":     "/model_routes/12/targets/3/upstream_model",
		"model_routes[12].targets[3].upstream_model.":    "",
		"model_routes[broken].targets[3].upstream_model": "",
	}
	for path, want := range tests {
		if got := ConfigPathToJSONPointer(path); got != want {
			t.Errorf("ConfigPathToJSONPointer(%q) = %q, want %q", path, got, want)
		}
	}
}

func assertProvidersConfigPresenceEqual(t *testing.T, want, got ProvidersConfig) {
	t.Helper()
	if want.SchemaVersion != got.SchemaVersion || want.schemaVersionSet != got.schemaVersionSet || want.modelRoutesSet != got.modelRoutesSet || want.policyProfilesSet != got.policyProfilesSet {
		t.Fatalf("top-level presence differs: want=%+v got=%+v", want, got)
	}
	if len(want.Providers) != len(got.Providers) || len(want.ModelRoutes) != len(got.ModelRoutes) || len(want.PolicyProfiles) != len(got.PolicyProfiles) {
		t.Fatalf("config lengths differ: want providers/routes/profiles %d/%d/%d got %d/%d/%d", len(want.Providers), len(want.ModelRoutes), len(want.PolicyProfiles), len(got.Providers), len(got.ModelRoutes), len(got.PolicyProfiles))
	}
	for index := range want.Providers {
		wantProvider, gotProvider := want.Providers[index], got.Providers[index]
		if wantProvider.trustDomainSet != gotProvider.trustDomainSet || wantProvider.classifierNoStoreSupportedSet != gotProvider.classifierNoStoreSupportedSet {
			t.Fatalf("provider[%d] presence differs: want=%+v got=%+v", index, wantProvider, gotProvider)
		}
	}
	for index := range want.ModelRoutes {
		wantRoute, gotRoute := want.ModelRoutes[index], got.ModelRoutes[index]
		if wantRoute.exposureSet != gotRoute.exposureSet || wantRoute.internalPurposeSet != gotRoute.internalPurposeSet || wantRoute.publicIDSet != gotRoute.publicIDSet || wantRoute.modelPickerEnabledSet != gotRoute.modelPickerEnabledSet || wantRoute.modelPickerCategorySet != gotRoute.modelPickerCategorySet {
			t.Fatalf("route[%d] presence differs: want=%+v got=%+v", index, wantRoute, gotRoute)
		}
		if wantRoute.Routing.modeSet != gotRoute.Routing.modeSet || wantRoute.Routing.maxTargetAttemptsSet != gotRoute.Routing.maxTargetAttemptsSet || wantRoute.Routing.maxUpstreamSendsSet != gotRoute.Routing.maxUpstreamSendsSet {
			t.Fatalf("route[%d] routing presence differs: want=%+v got=%+v", index, wantRoute.Routing, gotRoute.Routing)
		}
	}
	for index := range want.PolicyProfiles {
		wantClassifier, gotClassifier := want.PolicyProfiles[index].Classifier, got.PolicyProfiles[index].Classifier
		if wantClassifier.timeoutMSSet != gotClassifier.timeoutMSSet || wantClassifier.maxCompletionTokensSet != gotClassifier.maxCompletionTokensSet || wantClassifier.maxRequestBytesSet != gotClassifier.maxRequestBytesSet || wantClassifier.recentTurnsSet != gotClassifier.recentTurnsSet || wantClassifier.maxConcurrencySet != gotClassifier.maxConcurrencySet || wantClassifier.observeSampleRateSet != gotClassifier.observeSampleRateSet || wantClassifier.timeoutMSNull != gotClassifier.timeoutMSNull || wantClassifier.maxCompletionTokensNull != gotClassifier.maxCompletionTokensNull || wantClassifier.maxRequestBytesNull != gotClassifier.maxRequestBytesNull || wantClassifier.recentTurnsNull != gotClassifier.recentTurnsNull || wantClassifier.maxConcurrencyNull != gotClassifier.maxConcurrencyNull || wantClassifier.observeSampleRateNull != gotClassifier.observeSampleRateNull {
			t.Fatalf("profile[%d] classifier presence differs: want=%+v got=%+v", index, wantClassifier, gotClassifier)
		}
	}
	wantOutput, gotOutput := want.ToolOptimizers.OutputReduce, got.ToolOptimizers.OutputReduce
	if wantOutput.timeoutMSSet != gotOutput.timeoutMSSet || wantOutput.minInputBytesSet != gotOutput.minInputBytesSet || wantOutput.maxInputBytesSet != gotOutput.maxInputBytesSet {
		t.Fatalf("tool optimizer output presence differs: want=%+v got=%+v", wantOutput, gotOutput)
	}
}
