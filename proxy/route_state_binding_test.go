package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExtractExplicitResponsesRequestStateOnlyReadsResponseItems(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
		wantErr    bool
	}{
		{
			name: "top-level metadata is ordinary data",
			body: `{
				"model":"route",
				"metadata":{"encrypted_content":"metadata-value"},
				"input":[{"type":"message","role":"user","content":"hello"}]
			}`,
		},
		{
			name: "nested item metadata is ordinary data",
			body: `{
				"model":"route",
				"input":[{
					"type":"message",
					"role":"user",
					"metadata":{"encrypted_content":42},
					"content":[{
						"type":"input_text",
						"text":"hello",
						"metadata":{"encrypted_content":"content-metadata-value"}
					}]
				}]
			}`,
		},
		{
			name: "non-state response item field is ordinary data",
			body: `{
				"model":"route",
				"input":[{
					"type":"message",
					"role":"user",
					"encrypted_content":"message-extension-value",
					"content":"hello"
				}]
			}`,
		},
		{
			name: "response input items own opaque state",
			body: `{
				"model":"route",
				"input":[
					{"type":"reasoning","encrypted_content":"reasoning-state"},
					{"type":"compaction","encrypted_content":"compaction-state"}
				]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeEncryptedContent, value: "reasoning-state"},
				{stateType: stateBindingTypeEncryptedContent, value: "compaction-state"},
			},
		},
		{
			name: "malformed response item state is rejected",
			body: `{
				"model":"route",
				"input":[{"type":"reasoning","encrypted_content":42}]
			}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesRequestState([]byte(tc.body), nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("extractExplicitResponsesRequestState() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("extractExplicitResponsesRequestState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestIsProxyOwnedEncryptedContentRequiresDecodableSyntheticCheckpoint(t *testing.T) {
	validPayload, err := json.Marshal(syntheticCompactionPayload{Summary: "valid checkpoint"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	validEncoded := base64.RawURLEncoding.EncodeToString(validPayload)
	malformedEncoded := base64.RawURLEncoding.EncodeToString([]byte(`{"summary":`))

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "current checkpoint", value: syntheticCompactionPrefix + validEncoded, want: true},
		{name: "legacy checkpoint", value: legacySyntheticCompactionPrefix + validEncoded, want: true},
		{name: "review example malformed token", value: syntheticCompactionPrefix + "not-base64"},
		{name: "current malformed base64", value: syntheticCompactionPrefix + "***"},
		{name: "legacy malformed base64", value: legacySyntheticCompactionPrefix + "***"},
		{name: "current malformed payload", value: syntheticCompactionPrefix + malformedEncoded},
		{name: "legacy malformed payload", value: legacySyntheticCompactionPrefix + malformedEncoded},
		{name: "unsupported version", value: "vekil.compaction.v2:" + validEncoded},
		{name: "missing version", value: "vekil.compaction:" + validEncoded},
		{name: "lookalike prefix", value: "x" + syntheticCompactionPrefix + validEncoded},
		{name: "current prefix only", value: syntheticCompactionPrefix},
		{name: "legacy prefix only", value: legacySyntheticCompactionPrefix},
		{name: "legacy plaintext summary", value: "legacy plaintext checkpoint summary"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProxyOwnedEncryptedContent(tc.value); got != tc.want {
				t.Fatalf("isProxyOwnedEncryptedContent(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func syntheticCheckpointTokenForTest(t *testing.T, prefix, summary string) string {
	t.Helper()
	payload, err := json.Marshal(syntheticCompactionPayload{Summary: summary})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(payload)
}

func malformedSyntheticCheckpointTokensForTest(t *testing.T) []struct {
	name  string
	token string
} {
	t.Helper()
	validPayload, err := json.Marshal(syntheticCompactionPayload{Summary: "valid checkpoint"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	validEncoded := base64.RawURLEncoding.EncodeToString(validPayload)
	malformedEncoded := base64.RawURLEncoding.EncodeToString([]byte(`{"summary":`))

	return []struct {
		name  string
		token string
	}{
		{name: "review example malformed token", token: syntheticCompactionPrefix + "not-base64"},
		{name: "current malformed base64", token: syntheticCompactionPrefix + "***"},
		{name: "legacy malformed base64", token: legacySyntheticCompactionPrefix + "***"},
		{name: "current malformed payload", token: syntheticCompactionPrefix + malformedEncoded},
		{name: "legacy malformed payload", token: legacySyntheticCompactionPrefix + malformedEncoded},
		{name: "unsupported version", token: "vekil.compaction.v2:" + validEncoded},
		{name: "malformed prefix", token: "vekil.compaction.v1" + validEncoded},
		{name: "current prefix only", token: syntheticCompactionPrefix},
		{name: "legacy prefix only", token: legacySyntheticCompactionPrefix},
	}
}

func TestExtractExplicitResponsesRequestStatePreviousResponseID(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
		wantErr    string
	}{
		{
			name: "absent",
			body: `{"model":"route"}`,
		},
		{
			name: "null",
			body: `{"model":"route","previous_response_id":null}`,
		},
		{
			name: "valid string",
			body: `{"model":"route","previous_response_id":"  resp-123  "}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-123"},
			},
		},
		{
			name:    "empty string",
			body:    `{"model":"route","previous_response_id":""}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "whitespace string",
			body:    `{"model":"route","previous_response_id":"   "}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "number",
			body:    `{"model":"route","previous_response_id":42}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "boolean",
			body:    `{"model":"route","previous_response_id":false}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "object",
			body:    `{"model":"route","previous_response_id":{}}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
		{
			name:    "array",
			body:    `{"model":"route","previous_response_id":[]}`,
			wantErr: "previous_response_id must be a non-empty string",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesRequestState([]byte(tc.body), nil)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("extractExplicitResponsesRequestState() error = nil, want error")
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("extractExplicitResponsesRequestState() error = %q, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractExplicitResponsesRequestState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestExtractExplicitResponsesOutputStateOnlyReadsResponseItems(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantTokens []stateBindingToken
	}{
		{
			name: "nested response output",
			body: `{
				"type":"response.completed",
				"metadata":{"encrypted_content":"event-metadata"},
				"response":{
					"id":"resp-nested",
					"metadata":{"encrypted_content":"response-metadata"},
					"output":[{
						"type":"reasoning",
						"encrypted_content":"reasoning-state",
						"metadata":{"encrypted_content":"item-metadata"}
					}]
				}
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-nested"},
				{stateType: stateBindingTypeEncryptedContent, value: "reasoning-state"},
			},
		},
		{
			name: "output item event",
			body: `{
				"type":"response.output_item.done",
				"metadata":{"encrypted_content":"event-metadata"},
				"item":{
					"type":"compaction",
					"encrypted_content":"compaction-state",
					"metadata":{"encrypted_content":"item-metadata"}
				}
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeEncryptedContent, value: "compaction-state"},
			},
		},
		{
			name: "non-state output item field is ordinary data",
			body: `{
				"id":"resp-message-extension",
				"object":"response",
				"output":[{
					"type":"message",
					"encrypted_content":"message-extension-value",
					"content":[]
				}]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-message-extension"},
			},
		},
		{
			name: "top-level response output",
			body: `{
				"id":"resp-top-level",
				"object":"response",
				"metadata":{"encrypted_content":"response-metadata"},
				"output":[{
					"type":"message",
					"content":[{
						"type":"output_text",
						"text":"hello",
						"metadata":{"encrypted_content":"content-metadata"}
					}]
				}]
			}`,
			wantTokens: []stateBindingToken{
				{stateType: stateBindingTypeResponseID, value: "resp-top-level"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractExplicitResponsesOutputState([]byte(tc.body))
			if err != nil {
				t.Fatalf("extractExplicitResponsesOutputState() error = %v", err)
			}
			requireStateBindingTokens(t, got, tc.wantTokens)
		})
	}
}

func TestApplyExplicitRequestStateBindingIgnoresMetadataButRejectsUnknownArtifact(t *testing.T) {
	route := &modelRoute{
		public:  publicModelContract{routeID: "route-a"},
		targets: []targetBinding{{id: "target-a"}},
		policy:  routePolicy{mode: routeModePrimaryOnly, maxTargetAttempts: 1, maxUpstreamSends: 1},
	}

	t.Run("metadata remains ordinary data", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		operation := newRouteOperation(route, context.Background())
		body := []byte(`{
			"model":"route",
			"metadata":{"encrypted_content":"ordinary-metadata"},
			"input":[{"type":"message","role":"user","content":"hello"}]
		}`)
		if err := h.applyExplicitRequestStateBinding(operation, body, nil); err != nil {
			t.Fatalf("applyExplicitRequestStateBinding() error = %v", err)
		}
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})

	t.Run("unknown reasoning artifact is rejected", func(t *testing.T) {
		h := newRouteStateBindingTestHandler(t, 8)
		operation := newRouteOperation(route, context.Background())
		body := []byte(`{
			"model":"route",
			"input":[{"type":"reasoning","encrypted_content":"unknown-provider-state"}]
		}`)
		err := h.applyExplicitRequestStateBinding(operation, body, nil)
		var requestErr *providerRequestError
		if !errors.As(err, &requestErr) || requestErr.statusCode != 400 {
			t.Fatalf("applyExplicitRequestStateBinding() error = %v, want provider 400", err)
		}
		if got := operation.pinnedTarget(); got != "" {
			t.Fatalf("pinned target = %q, want none", got)
		}
	})
}

func TestBindExplicitStateTokensRecordsExactConcurrentEvictions(t *testing.T) {
	const workers = 256

	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 1, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-a"}
	if result := store.bind(stateBindingTypeResponseID, "seed", owner); result.outcome != stateBindingLookupKnown {
		t.Fatalf("seed bind outcome = %s, want known", result.outcome)
	}

	h := &ProxyHandler{
		stats:         newStatsCollector(),
		stateBindings: store,
	}
	h.stateBindingsOnce.Do(func() {})
	info := explicitRouteResponseInfo{routeID: owner.routeID, targetID: owner.targetID}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			err := h.bindExplicitStateTokens(info, []stateBindingToken{{
				stateType: stateBindingTypeResponseID,
				value:     fmt.Sprintf("response-%03d", i),
			}})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("bindExplicitStateTokens() error = %v", err)
	}

	storeEvictions := store.stats().evictions
	if storeEvictions != workers {
		t.Fatalf("store evictions = %d, want %d", storeEvictions, workers)
	}
	if got := h.stats.snapshot().StateBindingEvictions; got != int64(storeEvictions) {
		t.Fatalf("recorded state binding evictions = %d, want exact store delta %d", got, storeEvictions)
	}
}

func newRouteStateBindingTestHandler(t *testing.T, maxEntries int) *ProxyHandler {
	t.Helper()
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	h := &ProxyHandler{
		stats:         newStatsCollector(),
		stateBindings: newTestStateBindingStore(t, maxEntries, time.Hour, clock.Now),
	}
	h.stateBindingsOnce.Do(func() {})
	return h
}

func requireStateBindingTokens(t *testing.T, got, want []stateBindingToken) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tokens = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
