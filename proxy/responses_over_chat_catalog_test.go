package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

type rocCatalog struct {
	Data   []map[string]any `json:"data"`
	Models []map[string]any `json:"models"`
}

func fetchROCCatalog(t *testing.T, h *ProxyHandler) rocCatalog {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.HandleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var catalog rocCatalog
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func rocCatalogEntry(t *testing.T, entries []map[string]any, key, id string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry[key] == id {
			return entry
		}
	}
	t.Fatalf("catalog has no %s=%q", key, id)
	return nil
}

func TestResponsesOverChatCatalogMarksOnlyOptedInChatModels(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
	h := newROCHandler(t, upstream.server.URL)
	catalog := fetchROCCatalog(t, h)

	gemini := rocCatalogEntry(t, catalog.Data, "id", "gemini-test")
	if gemini[responsesCompatibilityCatalogField] != "responses_over_chat" {
		t.Fatalf("gemini compatibility = %v", gemini[responsesCompatibilityCatalogField])
	}
	if endpoints, _ := gemini["supported_endpoints"].([]any); len(endpoints) != 1 || endpoints[0] != "/chat/completions" {
		t.Fatalf("native endpoints changed: %v", gemini["supported_endpoints"])
	}
	for _, id := range []string{"gpt-native", "chat-no-tools"} {
		if value, ok := rocCatalogEntry(t, catalog.Data, "id", id)[responsesCompatibilityCatalogField]; ok {
			t.Fatalf("%s compatibility = %v, want absent", id, value)
		}
	}

	codexGemini := rocCatalogEntry(t, catalog.Models, "slug", "gemini-test")
	if codexGemini["visibility"] != "list" || codexGemini["supported_in_api"] != true {
		t.Fatalf("codex gemini = %v", codexGemini)
	}
	if modalities, _ := codexGemini["input_modalities"].([]any); len(modalities) != 1 || modalities[0] != "text" {
		t.Fatalf("input_modalities = %v", codexGemini["input_modalities"])
	}
	if codexGemini["use_responses_lite"] != false || codexGemini["supports_search_tool"] != false || codexGemini["supports_reasoning_summaries"] != false {
		t.Fatalf("restrictions = %v", codexGemini)
	}
	if levels, _ := codexGemini["supported_reasoning_levels"].([]any); len(levels) != 3 || codexGemini["default_reasoning_level"] != "medium" {
		t.Fatalf("reasoning levels = %v default=%v", codexGemini["supported_reasoning_levels"], codexGemini["default_reasoning_level"])
	}
	if codexGemini["context_window"] != float64(983040) {
		t.Fatalf("context_window = %v", codexGemini["context_window"])
	}
	native := rocCatalogEntry(t, catalog.Models, "slug", "gpt-native")
	if _, ok := native["use_responses_lite"]; ok {
		t.Fatalf("native entry gained adapter restrictions: %v", native)
	}
	if hidden := rocCatalogEntry(t, catalog.Models, "slug", "chat-no-tools"); hidden["visibility"] != "hide" {
		t.Fatalf("chat-no-tools visibility = %v", hidden["visibility"])
	}
}

func TestResponsesOverChatCatalogUnchangedWithoutOptIn(t *testing.T) {
	upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
	h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	catalog := fetchROCCatalog(t, h)
	gemini := rocCatalogEntry(t, catalog.Data, "id", "gemini-test")
	if _, ok := gemini[responsesCompatibilityCatalogField]; ok {
		t.Fatalf("compatibility advertised without opt-in: %v", gemini)
	}
	codexGemini := rocCatalogEntry(t, catalog.Models, "slug", "gemini-test")
	if codexGemini["visibility"] != "hide" || codexGemini["supported_in_api"] != false {
		t.Fatalf("codex gemini without opt-in = %v", codexGemini)
	}
	if _, ok := codexGemini["use_responses_lite"]; ok {
		t.Fatalf("restrictions added without opt-in: %v", codexGemini)
	}
}

func TestClassifyResponsesCompatibilityTable(t *testing.T) {
	no := false
	cases := []struct {
		name            string
		chat, responses bool
		caps            responsesOverChatCapabilities
		want            responsesExecutionMode
	}{
		{"native wins", true, true, responsesOverChatCapabilities{}, responsesExecutionNative},
		{"responses only", false, true, responsesOverChatCapabilities{}, responsesExecutionNative},
		{"chat only", true, false, responsesOverChatCapabilities{}, responsesExecutionOverChat},
		{"chat without tools", true, false, responsesOverChatCapabilities{toolCalls: &no}, responsesExecutionUnsupported},
		{"neither", false, false, responsesOverChatCapabilities{}, responsesExecutionUnsupported},
	}
	for _, tc := range cases {
		if got, _ := classifyResponsesCompatibility(tc.chat, tc.responses, tc.caps); got != tc.want {
			t.Fatalf("%s: mode = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestResponsesOverChatCatalogStripsUpstreamMarkers(t *testing.T) {
	// A chained or misconfigured upstream may already carry the marker. Only
	// this proxy's own eligible opt-ins may keep it.
	body := []byte(`{"object":"list","etag":"kept","data":[
		{"id":"gpt-native","supported_endpoints":["/chat/completions","/responses"],"vekil_responses_compatibility":"responses_over_chat"},
		{"id":"not-opted","supported_endpoints":["/chat/completions"],"VEKIL_RESPONSES_COMPATIBILITY":"responses_over_chat"},
		{"id":"gemini-test","supported_endpoints":["/chat/completions"],"Vekil_Responses_Compatibility":"untrusted","capabilities":{"supports":{"tool_calls":true}}}]}`)
	assertMarkers := func(t *testing.T, h *ProxyHandler, want map[string]bool) {
		t.Helper()
		rewritten, annotated := h.annotateResponsesOverChatCatalog(body)
		var catalog struct {
			Etag string                       `json:"etag"`
			Data []map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rewritten, &catalog); err != nil {
			t.Fatal(err)
		}
		if catalog.Etag != "kept" {
			t.Fatalf("top-level fields were dropped: %s", rewritten)
		}
		for _, entry := range catalog.Data {
			for _, variant := range []string{"VEKIL_RESPONSES_COMPATIBILITY", "Vekil_Responses_Compatibility"} {
				if _, exists := entry[variant]; exists {
					t.Fatalf("upstream marker spelling %q survived: %s", variant, rewritten)
				}
			}
			var id string
			_ = json.Unmarshal(entry["id"], &id)
			_, marked := entry[responsesCompatibilityCatalogField]
			if marked != want[id] {
				t.Fatalf("%s marked=%v, want %v: %s", id, marked, want[id], rewritten)
			}
			if _, inSet := annotated[id]; inSet != want[id] {
				t.Fatalf("%s annotated=%v, want %v", id, inSet, want[id])
			}
		}
	}

	t.Run("no opt-in", func(t *testing.T) {
		h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture"), logger.NewWithWriter(logger.LevelError, io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.BeginShutdown)
		assertMarkers(t, h, map[string]bool{})
	})
	t.Run("ineligible opt-in", func(t *testing.T) {
		upstream := newROCUpstream(t, rocSSE(t, rocStop, "[DONE]"))
		h := newROCHandler(t, upstream.server.URL)
		assertMarkers(t, h, map[string]bool{"gemini-test": true})
	})
}

func TestResponsesOverChatCatalogETagDescribesServedRepresentation(t *testing.T) {
	const upstreamETag = `"upstream-model-version"`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if validator := r.Header.Get("If-None-Match"); validator != "" && validator != upstreamETag {
			t.Errorf("sent public validator upstream: %q", validator)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", upstreamETag)
		if r.Header.Get("If-None-Match") == upstreamETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, rocCopilotCatalog)
	}))
	t.Cleanup(upstream.Close)
	off, err := NewProxyHandler(auth.NewTestAuthenticator("fixture"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(off.BeginShutdown)
	on := newROCHandler(t, upstream.URL)
	get := func(handler *ProxyHandler) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.HandleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	plain := get(off)
	compatible := get(on)
	if plain.Header().Get("ETag") != upstreamETag || compatible.Header().Get("ETag") == upstreamETag || compatible.Header().Get("ETag") == "" || compatible.Body.String() == plain.Body.String() {
		t.Fatalf("plain ETag = %q, compatible ETag = %q", plain.Header().Get("ETag"), compatible.Header().Get("ETag"))
	}
	if revalidated := get(on); revalidated.Header().Get("ETag") != compatible.Header().Get("ETag") || revalidated.Body.String() != compatible.Body.String() {
		t.Fatal("upstream revalidation changed the local representation or its ETag")
	}
}
