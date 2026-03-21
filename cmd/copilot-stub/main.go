package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

var requestSeq uint64

type envelope struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type chatCompletionsRequest struct {
	envelope
	Messages interface{} `json:"messages"`
}

type responsesRequest struct {
	envelope
	Input interface{} `json:"input"`
}

func main() {
	host := flag.String("host", "127.0.0.1", "Listen host")
	port := flag.String("port", "8081", "Listen port")
	flag.Parse()

	logger := log.New(os.Stdout, "copilot-stub: ", log.LstdFlags)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "ok",
		})
	})
	mux.HandleFunc("GET /models", handleModels)
	mux.HandleFunc("POST /chat/completions", handleChatCompletions)
	mux.HandleFunc("POST /responses", handleResponses)

	addr := net.JoinHostPort(*host, *port)
	logger.Printf("listening on http://%s", addr)
	if err := http.ListenAndServe(addr, logRequests(logger, mux)); err != nil {
		logger.Fatalf("listen failed: %v", err)
	}
}

func handleModels(w http.ResponseWriter, _ *http.Request) {
	setCommonHeaders(w, "")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{
			{
				"id":                  "gpt-5.4",
				"object":              "model",
				"created":             0,
				"owned_by":            "github-copilot",
				"name":                "GPT-5.4",
				"supported_endpoints": []string{"/chat/completions", "/responses"},
				"capabilities": map[string]interface{}{
					"supports": map[string]interface{}{
						"parallel_tool_calls": true,
						"vision":              true,
						"reasoning_effort":    []string{"low", "medium", "high"},
					},
					"limits": map[string]interface{}{
						"max_context_window_tokens": 128000,
					},
				},
				"model_picker_enabled":  true,
				"model_picker_category": "powerful",
			},
			{
				"id":                  "claude-sonnet-4.6",
				"object":              "model",
				"created":             0,
				"owned_by":            "github-copilot",
				"name":                "Claude Sonnet 4.6",
				"supported_endpoints": []string{"/chat/completions", "/v1/messages"},
				"capabilities": map[string]interface{}{
					"supports": map[string]interface{}{
						"parallel_tool_calls": true,
						"vision":              true,
					},
					"limits": map[string]interface{}{
						"max_context_window_tokens": 200000,
					},
				},
				"model_picker_enabled":  true,
				"model_picker_category": "versatile",
			},
			{
				"id":                  "gemini-3.1-pro-preview",
				"object":              "model",
				"created":             0,
				"owned_by":            "github-copilot",
				"name":                "Gemini 3.1 Pro Preview",
				"supported_endpoints": []string{"/chat/completions"},
				"capabilities": map[string]interface{}{
					"supports": map[string]interface{}{
						"vision": true,
					},
					"limits": map[string]interface{}{
						"max_context_window_tokens": 1048576,
					},
				},
				"model_picker_enabled":  true,
				"model_picker_category": "explore",
			},
		},
	})
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatCompletionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("invalid JSON: %v", err),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	model := normalizeModel(req.Model)
	text := cannedText("chat.completions", model, req.Messages)
	if req.Stream {
		writeChatCompletionsStream(w, model, text)
		return
	}

	setCommonHeaders(w, model)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":                 nextID("chatcmpl"),
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              model,
		"system_fingerprint": "fp_stub",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
				"logprobs":      nil,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     12,
			"completion_tokens": 8,
			"total_tokens":      20,
		},
	})
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": map[string]interface{}{
				"message": fmt.Sprintf("invalid JSON: %v", err),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	model := normalizeModel(req.Model)
	text := cannedText("responses", model, req.Input)
	if req.Stream {
		writeResponsesStream(w, model, text)
		return
	}

	responseID := nextID("resp")
	messageID := nextID("msg")
	setCommonHeaders(w, model)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output": []map[string]interface{}{
			{
				"id":   messageID,
				"type": "message",
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type": "output_text",
						"text": text,
					},
				},
			},
		},
		"usage": map[string]interface{}{
			"input_tokens":  11,
			"output_tokens": 9,
			"total_tokens":  20,
		},
	})
}

func writeChatCompletionsStream(w http.ResponseWriter, model, text string) {
	setCommonHeaders(w, model)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	id := nextID("chatcmpl")
	created := time.Now().Unix()
	chunks := splitText(text)

	for i, chunk := range chunks {
		delta := map[string]interface{}{
			"content": chunk,
		}
		if i == 0 {
			delta["role"] = "assistant"
		}
		writeDataLine(w, map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         delta,
					"finish_reason": nil,
				},
			},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeDataLine(w, map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeResponsesStream(w http.ResponseWriter, model, text string) {
	setCommonHeaders(w, model)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	responseID := nextID("resp")
	messageID := nextID("msg")
	outputItem := map[string]interface{}{
		"id":   messageID,
		"type": "message",
		"role": "assistant",
		"content": []map[string]interface{}{
			{
				"type": "output_text",
				"text": text,
			},
		},
	}

	writeEvent(w, "response.created", map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"model":  model,
			"status": "in_progress",
		},
	})
	if flusher != nil {
		flusher.Flush()
	}

	writeEvent(w, "response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done",
		"item": outputItem,
	})
	if flusher != nil {
		flusher.Flush()
	}

	writeEvent(w, "response.completed", map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":     responseID,
			"object": "response",
			"model":  model,
			"status": "completed",
			"output": []map[string]interface{}{
				outputItem,
			},
			"usage": map[string]interface{}{
				"input_tokens":  11,
				"output_tokens": 9,
				"total_tokens":  20,
			},
		},
	})
	if flusher != nil {
		flusher.Flush()
	}
}

func logRequests(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func setCommonHeaders(w http.ResponseWriter, model string) {
	w.Header().Set("X-Models-Etag", `"stub-models-v1"`)
	w.Header().Set("X-Reasoning-Included", "true")
	w.Header().Set("X-Codex-Turn-State", "stub-turn-state")
	if model != "" {
		w.Header().Set("OpenAI-Model", model)
	}
}

func nextID(prefix string) string {
	n := atomic.AddUint64(&requestSeq, 1)
	return fmt.Sprintf("%s-stub-%06d", prefix, n)
}

func normalizeModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "gpt-5.4"
	}
	return strings.TrimSpace(model)
}

func cannedText(endpoint, model string, value interface{}) string {
	prompt := strings.Join(extractTextFragments(value), " | ")
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Sprintf("stub %s reply from %s", endpoint, model)
	}
	if len(prompt) > 96 {
		prompt = prompt[:93] + "..."
	}
	return fmt.Sprintf("stub %s reply from %s: %s", endpoint, model, prompt)
}

func extractTextFragments(value interface{}) []string {
	var fragments []string
	collectTextFragments(value, &fragments)
	return fragments
}

func collectTextFragments(value interface{}, fragments *[]string) {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text != "" {
			*fragments = append(*fragments, text)
		}
	case []interface{}:
		for _, item := range typed {
			collectTextFragments(item, fragments)
		}
	case map[string]interface{}:
		for _, key := range []string{"text", "content", "input", "parts"} {
			if nested, ok := typed[key]; ok {
				collectTextFragments(nested, fragments)
			}
		}
	}
}

func splitText(text string) []string {
	if text == "" {
		return []string{""}
	}
	if len(text) <= 24 {
		return []string{text}
	}
	mid := len(text) / 2
	return []string{text[:mid], text[mid:]}
}

func writeDataLine(w http.ResponseWriter, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeEvent(w http.ResponseWriter, event string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}
