// Package proxy implements HTTP handlers that forward requests to GitHub
// Copilot's backend. It provides Anthropic-to-OpenAI translation for the
// /v1/messages endpoint and near zero-copy passthrough for OpenAI endpoints.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/models"
)

// ProxyHandler holds dependencies for all HTTP handlers.
type ProxyHandler struct {
	auth           *auth.Authenticator
	client         *http.Client
	copilotURL     string
	log            *logger.Logger
	maxRetries     int
	retryBaseDelay time.Duration
}

// NewProxyHandler creates a ProxyHandler with connection pooling and HTTP/2.
func NewProxyHandler(a *auth.Authenticator, log *logger.Logger) *ProxyHandler {
	return &ProxyHandler{
		auth: a,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		copilotURL: "https://api.githubcopilot.com",
		log:        log,
	}
}

func setCopilotHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("editor-version", "vscode/1.95.0")
	req.Header.Set("editor-plugin-version", "copilot-chat/0.26.7")
	req.Header.Set("user-agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("copilot-integration-id", "vscode-chat")
	req.Header.Set("x-github-api-version", "2025-04-01")
	req.Header.Set("x-request-id", uuid.New().String())
	req.Header.Set("openai-intent", "conversation-panel")
	req.Header.Set("Content-Type", "application/json")
}

// HandleAnthropicMessages handles POST /v1/messages by translating the Anthropic
// request to OpenAI format, forwarding to Copilot, and translating the response back.
func (h *ProxyHandler) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	defer r.Body.Close()

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON in request body")
		return
	}

	oaiReq, err := TranslateAnthropicToOpenAI(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}

	// Force streaming to upstream for non-streaming requests to ensure
	// reliable parallel tool call support. The upstream may not return
	// parallel tool calls in non-streaming mode.
	forceStream := !req.Stream
	if forceStream {
		b := true
		oaiReq.Stream = &b
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("failed to get token: %v", err))
		return
	}

	oaiBody, err := json.Marshal(oaiReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to marshal request")
		return
	}

	// Use background context to avoid cancellation from client disconnects
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.copilotURL+"/chat/completions", bytes.NewReader(oaiBody))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeAnthropicError(w, status, "api_error", fmt.Sprintf("upstream request failed: %v", err))
		return
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		h.log.Error("upstream error", logger.F("endpoint", "anthropic"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
		writeAnthropicError(w, resp.StatusCode, "api_error", fmt.Sprintf("upstream error (%d): %s", resp.StatusCode, string(errBody)))
		return
	}

	if req.Stream {
		StreamOpenAIToAnthropic(w, resp.Body, req.Model, "msg_"+uuid.New().String())
		return
	}

	// Non-streaming: aggregate the forced-streaming upstream response
	oaiResp, err := aggregateStreamToResponse(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to aggregate upstream response")
		return
	}

	anthropicResp := TranslateOpenAIToAnthropic(oaiResp, req.Model)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropicResp)
}

// HandleOpenAIChatCompletions handles POST /v1/chat/completions by forwarding the
// request to Copilot with only auth headers injected (near zero-copy passthrough).
func (h *ProxyHandler) HandleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	// Buffer the full body for retry support and inspection
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	// Detect if the client requested streaming and if tools are present
	var partial struct {
		Stream *bool           `json:"stream,omitempty"`
		Tools  json.RawMessage `json:"tools,omitempty"`
	}
	json.Unmarshal(bodyBytes, &partial)
	isStreaming := partial.Stream != nil && *partial.Stream
	hasTools := len(partial.Tools) > 0 && string(partial.Tools) != "null"

	// Inject parallel_tool_calls: true when tools are present
	bodyBytes = injectParallelToolCalls(bodyBytes)

	// Force streaming to upstream for non-streaming requests with tools
	// to ensure reliable parallel tool call support.
	forceStream := !isStreaming && hasTools
	if forceStream {
		bodyBytes = injectForceStream(bodyBytes)
	}

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.copilotURL+"/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeOpenAIError(w, status, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	if isStreaming && resp.StatusCode == http.StatusOK {
		StreamOpenAIPassthrough(w, resp.Body)
		return
	}

	if forceStream && resp.StatusCode == http.StatusOK {
		// Aggregate forced-streaming response back to non-streaming
		oaiResp, err := aggregateStreamToResponse(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to aggregate upstream response", "server_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(oaiResp)
		return
	}

	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// HandleResponses handles POST /v1/responses by forwarding the request to
// Copilot's responses endpoint with only auth headers injected.
func (h *ProxyHandler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	// Peek at the stream field without buffering the full body
	var buf bytes.Buffer
	tee := io.TeeReader(r.Body, &buf)

	var partial struct {
		Stream *bool `json:"stream,omitempty"`
	}
	json.NewDecoder(tee).Decode(&partial)
	isStreaming := partial.Stream != nil && *partial.Stream

	// Reconstruct body: already-read bytes + remainder
	body := io.MultiReader(&buf, r.Body)

	token, err := h.auth.GetToken(r.Context())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get token: %v", err), "server_error")
		return
	}

	// Buffer the full body for retry support
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.copilotURL+"/responses", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		setCopilotHeaders(req, token)
		return req, nil
	})
	if err != nil {
		status := http.StatusBadGateway
		if ue, ok := err.(*upstreamError); ok {
			status = ue.statusCode
		}
		writeOpenAIError(w, status, fmt.Sprintf("upstream request failed: %v", err), "server_error")
		return
	}

	if isStreaming && resp.StatusCode == http.StatusOK {
		StreamOpenAIPassthrough(w, resp.Body)
		return
	}

	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// HandleHealthz handles GET /healthz and returns {"status":"ok"}.
func (h *ProxyHandler) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// HandleModels handles GET /v1/models and returns a list of available models.
func (h *ProxyHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models := []string{
		"gpt-4o",
		"gpt-4o-mini",
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4.1-nano",
		"gpt-5.3-codex",
		"o1",
		"o1-mini",
		"o1-preview",
		"o3",
		"o3-mini",
		"o4-mini",
		"claude-3.5-sonnet",
		"claude-sonnet-4",
		"claude-sonnet-4.5",
		"claude-haiku-4.5",
		"claude-opus-4",
		"claude-opus-4.5",
		"claude-sonnet-4.6",
		"claude-opus-4.6",
		"claude-opus-4.6-fast",
	}

	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	var data []modelObj
	for _, m := range models {
		data = append(data, modelObj{
			ID:      m,
			Object:  "model",
			Created: 0,
			OwnedBy: "github-copilot",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(models.AnthropicError{
		Type: "error",
		Error: struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}{
			Type:    errType,
			Message: message,
		},
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    nil,
		},
	})
}

// injectParallelToolCalls adds parallel_tool_calls: true to an OpenAI request
// body when tools are present but the flag is not already set.
func injectParallelToolCalls(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, hasTools := m["tools"]; !hasTools {
		return body
	}
	if _, hasPTC := m["parallel_tool_calls"]; hasPTC {
		return body
	}
	m["parallel_tool_calls"] = json.RawMessage("true")
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

// injectForceStream adds stream: true and stream_options to a request body
// for forced streaming to the upstream.
func injectForceStream(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = json.RawMessage("true")
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}
