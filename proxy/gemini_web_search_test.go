package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func TestHandleGeminiGoogleSearchLoopNonStreaming(t *testing.T) {
	var (
		mu            sync.Mutex
		chatRound     int
		capturedCalls []models.OpenAIRequest
	)
	fakeMCP := &fakeGitHubMCP{
		t:          t,
		tools:      []string{"web_search"},
		callResult: `{"content":[{"type":"text","text":"Go 1.26 was released in February 2026. [Source](https://go.dev/doc/go1.26)"}]}`,
	}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp/" {
			fakeMCP.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var oaiReq models.OpenAIRequest
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream chat request: %v", err)
		}
		mu.Lock()
		chatRound++
		round := chatRound
		capturedCalls = append(capturedCalls, oaiReq)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		switch round {
		case 1:
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_ws_1\",\"type\":\"function\",\"function\":{\"name\":\"__vekil_google_search\",\"arguments\":\"{\\\"query\\\":\\\"latest Go release\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		case 2:
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-2\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Go 1.26 is the latest release.\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-2\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":25,\"completion_tokens\":8,\"total_tokens\":33}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			t.Fatalf("unexpected chat round %d", round)
		}
	})

	reqBody := `{
		"contents": [{"role": "user", "parts": [{"text": "What is the latest Go release?"}]}],
		"tools": [{"googleSearch": {}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil || len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("unexpected candidates: %#v", geminiResp.Candidates)
	}
	if got := derefString(geminiResp.Candidates[0].Content.Parts[0].Text); got != "Go 1.26 is the latest release." {
		t.Errorf("text = %q, want grounded answer", got)
	}
	if geminiResp.Candidates[0].GroundingMetadata == nil || len(geminiResp.Candidates[0].GroundingMetadata.WebSearchQueries) != 1 || geminiResp.Candidates[0].GroundingMetadata.WebSearchQueries[0] != "latest Go release" {
		t.Errorf("GroundingMetadata = %#v, want [latest Go release]", geminiResp.Candidates[0].GroundingMetadata)
	}
	if geminiResp.UsageMetadata == nil || geminiResp.UsageMetadata.PromptTokenCount != 35 || geminiResp.UsageMetadata.CandidatesTokenCount != 13 || geminiResp.UsageMetadata.TotalTokenCount != 48 {
		t.Errorf("UsageMetadata = %#v, want accumulated usage (35/13/48)", geminiResp.UsageMetadata)
	}

	fakeMCP.mu.Lock()
	rawArgs := string(fakeMCP.callArgs)
	fakeMCP.mu.Unlock()
	if !strings.Contains(rawArgs, `"query":"latest Go release"`) {
		t.Errorf("MCP callArgs = %s, want latest Go release", rawArgs)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedCalls) != 2 {
		t.Fatalf("capturedCalls = %d, want 2", len(capturedCalls))
	}
	if len(capturedCalls[0].Tools) != 1 || capturedCalls[0].Tools[0].Function.Name != geminiSyntheticWebSearchToolName {
		t.Errorf("round 1 tools = %#v, want synthetic web search tool", capturedCalls[0].Tools)
	}
	if len(capturedCalls[1].Messages) != 3 || capturedCalls[1].Messages[2].Role != "tool" || capturedCalls[1].Messages[2].ToolCallID != "call_ws_1" {
		t.Errorf("round 2 messages = %#v, want assistant + tool message", capturedCalls[1].Messages)
	}
}

func TestHandleGeminiGoogleSearchLoopStreaming(t *testing.T) {
	fakeMCP := &fakeGitHubMCP{
		t:          t,
		tools:      []string{"web_search"},
		callResult: `{"content":[{"type":"text","text":"Result from Bing"}]}`,
	}
	round := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp/" {
			fakeMCP.ServeHTTP(w, r)
			return
		}
		round++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if round == 1 {
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"__vekil_google_search\",\"arguments\":\"{\\\"query\\\":\\\"vekil\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c2\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Grounded stream reply.\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c2\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})

	reqBody := `{
		"contents": [{"role": "user", "parts": [{"text": "Search vekil"}]}],
		"tools": [{"google_search": {}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rawBody), "Grounded stream reply.") || !strings.Contains(string(rawBody), `"webSearchQueries":["vekil"]`) {
		t.Errorf("stream body = %s, want grounded reply and webSearchQueries", string(rawBody))
	}
	if strings.Contains(string(rawBody), geminiSyntheticWebSearchToolName) {
		t.Errorf("stream body leaked synthetic tool name: %s", string(rawBody))
	}
}

func TestHandleGeminiGoogleSearchBoundedRounds(t *testing.T) {
	fakeMCP := &fakeGitHubMCP{
		t:          t,
		tools:      []string{"web_search"},
		callResult: `{"content":[{"type":"text","text":"search hit"}]}`,
	}
	rounds := 0
	var lastTools []models.OpenAITool
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp/" {
			fakeMCP.ServeHTTP(w, r)
			return
		}
		rounds++
		body, _ := io.ReadAll(r.Body)
		var oaiReq models.OpenAIRequest
		_ = json.Unmarshal(body, &oaiReq)
		lastTools = oaiReq.Tools

		if len(oaiReq.Tools) == 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
				ID:      "final",
				Object:  "chat.completion",
				Model:   "gemini-2.5-pro",
				Choices: []models.OpenAIChoice{{Index: 0, Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Final answer after cap."`)}, FinishReason: strPtr("stop")}},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_%d\",\"type\":\"function\",\"function\":{\"name\":\"__vekil_google_search\",\"arguments\":\"{\\\"query\\\":\\\"q%d\\\"}\"}}]},\"finish_reason\":null}]}\n\n", rounds, rounds, rounds)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", rounds)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})

	reqBody := `{
		"contents": [{"role": "user", "parts": [{"text": "Keep searching"}]}],
		"tools": [{"googleSearch": {}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if rounds != geminiMaxWebSearchRounds+1 {
		t.Errorf("rounds = %d, want %d", rounds, geminiMaxWebSearchRounds+1)
	}
	if len(lastTools) != 0 {
		t.Errorf("final round tools = %#v, want empty (synthetic tool removed at cap)", lastTools)
	}
}

func TestHandleGeminiGoogleSearchUnsupportedWithoutCopilot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called when googleSearch is unavailable")
	}))
	defer upstream.Close()

	cfg := ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:      "azure",
			Type:    "azure-openai",
			BaseURL: upstream.URL + "/openai/v1",
			APIKey:  "test-key",
			Models: []ProviderModelConfig{{
				PublicID:   "gemini-2.5-pro",
				Deployment: "gemini-2.5-pro",
				Endpoints:  []string{providerEndpointChatCompletions},
			}},
		}},
	}
	handler, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError), WithProvidersConfig(cfg))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	reqBody := `{
		"contents": [{"role": "user", "parts": [{"text": "Search the web"}]}],
		"tools": [{"googleSearch": {}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", w.Code, w.Body.String())
	}
}
