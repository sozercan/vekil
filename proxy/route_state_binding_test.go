package proxy

import (
	"context"
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
