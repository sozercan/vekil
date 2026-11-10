package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sozercan/vekil/logger"
)

// Vekil serves a single-tool MCP endpoint backed by the web_search tool on
// GitHub's MCP server. The endpoint is a tool server, not a relay: clients can
// only list and call web_search, and the GitHub credential never leaves Vekil.
const (
	webSearchMCPToolName              = "web_search"
	webSearchMCPLatestProtocolVersion = "2025-11-25"
	webSearchMCPRequestBodyLimit      = 64 << 10
	webSearchMCPUpstreamBodyLimit     = 4 << 20
	webSearchMCPUpstreamTimeout       = 2 * time.Minute

	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
	jsonRPCInternalError  = -32603
)

var webSearchMCPProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

var webSearchMCPTool = json.RawMessage(`{"name":"web_search","title":"Web search","description":"Search the web with GitHub Copilot's web search. Ask one focused, self-contained natural-language question per call; a search agent queries Bing and answers with cited sources. The answer is the agent's summary, not page text: fetch the cited URLs when exact details matter.","inputSchema":{"type":"object","properties":{"query":{"type":"string","description":"A clear, self-contained question for the search agent."}},"required":["query"]},"annotations":{"readOnlyHint":true,"openWorldHint":true}}`)

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *mcpError) Error() string { return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message) }

// HandleWebSearchMCP serves POST /mcp/web-search as a stateless MCP server
// whose only tool is Copilot's web search. It is available only while Vekil
// uses a Copilot provider, because the search authenticates with that
// provider's GitHub credential.
func (h *ProxyHandler) HandleWebSearchMCP(w http.ResponseWriter, r *http.Request) {
	// MCP clients such as Claude Code send no Origin; browsers always do.
	// Rejecting it keeps web pages, including DNS-rebinding ones, from
	// spending the user's GitHub search through a local Vekil.
	if r.Header.Get("Origin") != "" {
		writeMCPJSON(w, http.StatusForbidden, mcpMessage{JSONRPC: "2.0", Error: &mcpError{Code: jsonRPCInvalidRequest, Message: "browser origins are not allowed"}})
		return
	}
	if h.auth == nil || !h.UsesCopilot() {
		writeMCPJSON(w, http.StatusNotFound, mcpMessage{JSONRPC: "2.0", Error: &mcpError{Code: jsonRPCMethodNotFound, Message: "web search requires a Copilot provider"}})
		return
	}
	body, err := readBodyWithLimit(r, webSearchMCPRequestBodyLimit)
	if err != nil {
		writeMCPJSON(w, readBodyStatusCode(err), mcpMessage{JSONRPC: "2.0", Error: &mcpError{Code: jsonRPCInvalidRequest, Message: err.Error()}})
		return
	}
	var request mcpMessage
	if err := json.Unmarshal(body, &request); err != nil || request.JSONRPC != "2.0" || request.Method == "" {
		writeMCPJSON(w, http.StatusBadRequest, mcpMessage{JSONRPC: "2.0", Error: &mcpError{Code: jsonRPCInvalidRequest, Message: "expected a single JSON-RPC 2.0 request or notification"}})
		return
	}
	if len(request.ID) == 0 {
		// Notifications such as notifications/initialized need no state here.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	response := mcpMessage{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		response.Result = webSearchMCPInitializeResult(request.Params)
	case "ping":
		response.Result = json.RawMessage(`{}`)
	case "tools/list":
		available, err := h.webSearchUpstreamAvailable(r.Context())
		if err != nil {
			h.log.Warn("web search MCP tool discovery failed", logger.Err(err))
			response.Error = &mcpError{Code: jsonRPCInternalError, Message: err.Error()}
			break
		}
		tools := []json.RawMessage{}
		if available {
			tools = append(tools, webSearchMCPTool)
		}
		response.Result, _ = json.Marshal(map[string]any{"tools": tools})
	case "tools/call":
		response.Result, response.Error = h.callWebSearchMCPTool(r.Context(), request.Params)
	default:
		response.Error = &mcpError{Code: jsonRPCMethodNotFound, Message: "method not found: " + request.Method}
	}
	writeMCPJSON(w, http.StatusOK, response)
}

func webSearchMCPInitializeResult(params json.RawMessage) json.RawMessage {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &request)
	version := webSearchMCPLatestProtocolVersion
	if webSearchMCPProtocolVersions[request.ProtocolVersion] {
		version = request.ProtocolVersion
	}
	result, _ := json.Marshal(map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]string{"name": "vekil", "title": "Vekil web search", "version": "1"},
	})
	return result
}

func (h *ProxyHandler) callWebSearchMCPTool(ctx context.Context, params json.RawMessage) (json.RawMessage, *mcpError) {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			Query string `json:"query"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &mcpError{Code: jsonRPCInvalidParams, Message: "invalid tools/call params"}
	}
	if call.Name != webSearchMCPToolName {
		return nil, &mcpError{Code: jsonRPCInvalidParams, Message: "unknown tool: " + call.Name}
	}
	query := strings.TrimSpace(call.Arguments.Query)
	if query == "" {
		return webSearchMCPToolError("query is required"), nil
	}

	result, err := h.callWebSearchUpstream(ctx, query)
	if err != nil {
		h.log.Warn("web search failed", logger.Err(err))
		return webSearchMCPToolError("web search failed: " + err.Error()), nil
	}
	return result, nil
}

func webSearchMCPToolError(message string) json.RawMessage {
	result, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": message}},
		"isError": true,
	})
	return result
}

func writeMCPJSON(w http.ResponseWriter, status int, message mcpMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(message)
}

// webSearchUpstreamAvailable reports whether GitHub's MCP server currently
// offers web_search to this credential; account plans and organization
// policy can withhold it even when the credential authenticates.
func (h *ProxyHandler) webSearchUpstreamAvailable(ctx context.Context) (bool, error) {
	ctx, cancel := h.webSearchMCPContext(ctx)
	defer cancel()
	session, err := h.openWebSearchUpstream(ctx)
	if err != nil {
		return false, err
	}
	raw, err := session.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return false, err
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return false, fmt.Errorf("decode GitHub MCP tools/list: %w", err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == webSearchMCPToolName {
			return true, nil
		}
	}
	return false, nil
}

func (h *ProxyHandler) callWebSearchUpstream(ctx context.Context, query string) (json.RawMessage, error) {
	ctx, cancel := h.webSearchMCPContext(ctx)
	defer cancel()
	session, err := h.openWebSearchUpstream(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := session.call(ctx, "tools/call", map[string]any{
		"name":      webSearchMCPToolName,
		"arguments": map[string]string{"query": query},
	})
	if err != nil {
		return nil, err
	}
	var result struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Content) == 0 {
		return nil, errors.New("GitHub MCP returned a tool result without content")
	}
	return raw, nil
}

// webSearchMCPContext ends upstream calls when the client goes away, the
// upstream timeout passes, or the proxy begins shutdown, whichever is first.
func (h *ProxyHandler) webSearchMCPContext(inbound context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(inbound, webSearchMCPUpstreamTimeout)
	stop := context.AfterFunc(h.lifecycleContext(), cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// webSearchUpstreamSession is one short-lived MCP session with GitHub's
// server. Each tool request opens its own session, which keeps Vekil free of
// session expiry handling at the cost of two small handshake requests.
type webSearchUpstreamSession struct {
	h               *ProxyHandler
	url             string
	token           string
	sessionID       string
	protocolVersion string
	nextID          int
}

func (h *ProxyHandler) openWebSearchUpstream(ctx context.Context) (*webSearchUpstreamSession, error) {
	token, err := h.auth.GetTokenNonInteractive(ctx)
	if err != nil {
		return nil, fmt.Errorf("get GitHub credential: %w", err)
	}
	session := &webSearchUpstreamSession{h: h, url: strings.TrimRight(h.copilotURL, "/") + "/mcp/", token: token}
	raw, err := session.call(ctx, "initialize", map[string]any{
		"protocolVersion": webSearchMCPLatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "vekil", "version": "1"},
	})
	if err != nil {
		return nil, err
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &initialized)
	session.protocolVersion = initialized.ProtocolVersion
	if err := session.notify(ctx, "notifications/initialized"); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *webSearchUpstreamSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.nextID++
	id := json.RawMessage(strconv.Itoa(s.nextID))
	resp, err := s.send(ctx, method, id, params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub MCP %s returned HTTP %d", method, resp.StatusCode)
	}
	if method == "initialize" {
		s.sessionID = strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
	}
	message, err := readMCPResponse(resp, id)
	if err != nil {
		return nil, fmt.Errorf("GitHub MCP %s: %w", method, err)
	}
	if message.Error != nil {
		return nil, fmt.Errorf("GitHub MCP %s: %w", method, message.Error)
	}
	return message.Result, nil
}

func (s *webSearchUpstreamSession) notify(ctx context.Context, method string) error {
	resp, err := s.send(ctx, method, nil, nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webSearchMCPUpstreamBodyLimit))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GitHub MCP %s returned HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (s *webSearchUpstreamSession) send(ctx context.Context, method string, id json.RawMessage, params any) (*http.Response, error) {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		message["id"] = id
	}
	if params != nil {
		message["params"] = params
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// GitHub's MCP server exposes only the named tools, read-only.
	req.Header.Set("X-MCP-Tools", webSearchMCPToolName)
	req.Header.Set("X-MCP-Readonly", "true")
	if s.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	if s.protocolVersion != "" {
		req.Header.Set("Mcp-Protocol-Version", s.protocolVersion)
	}
	resp, err := s.h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub MCP %s: %w", method, err)
	}
	return resp, nil
}

// readMCPResponse returns the JSON-RPC response with the given id from either
// a JSON body or a Streamable HTTP SSE stream.
func readMCPResponse(resp *http.Response, id json.RawMessage) (mcpMessage, error) {
	body := io.LimitReader(resp.Body, webSearchMCPUpstreamBodyLimit)
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		var message mcpMessage
		if err := json.NewDecoder(body).Decode(&message); err != nil {
			return mcpMessage{}, fmt.Errorf("decode response: %w", err)
		}
		return message, nil
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), webSearchMCPUpstreamBodyLimit)
	var data []string
	for {
		more := scanner.Scan()
		line := scanner.Text()
		if more && line != "" {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			continue
		}
		if len(data) > 0 {
			var message mcpMessage
			if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &message); err == nil &&
				bytes.Equal(message.ID, id) && (message.Result != nil || message.Error != nil) {
				return message, nil
			}
			data = data[:0]
		}
		if !more {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return mcpMessage{}, fmt.Errorf("read event stream: %w", err)
	}
	return mcpMessage{}, errors.New("event stream ended without a response")
}
