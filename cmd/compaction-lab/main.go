package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

type labScenario func(context.Context) error

type fakeUpstream struct {
	mu       sync.Mutex
	requests []upstreamRequest
}

type upstreamRequest struct {
	Path   string
	Header http.Header
	Body   map[string]json.RawMessage
}

func main() {
	scenarioFlag := flag.String("scenario", "all", "scenario to run: all, compact-shape, unknown-token, remote-compaction-v2, remote-compaction-v2-previous-response, websocket-response-processed, websocket-remote-compaction-followup")
	flag.Parse()

	scenarios := map[string]labScenario{
		"compact-shape":                          runCompactShapeScenario,
		"unknown-token":                          runUnknownTokenScenario,
		"remote-compaction-v2":                   runRemoteCompactionV2Scenario,
		"remote-compaction-v2-previous-response": runRemoteCompactionV2PreviousResponseScenario,
		"websocket-response-processed":           runWebSocketResponseProcessedScenario,
		"websocket-remote-compaction-followup":   runWebSocketRemoteCompactionFollowUpScenario,
	}
	order := []string{"compact-shape", "unknown-token", "remote-compaction-v2", "remote-compaction-v2-previous-response", "websocket-response-processed", "websocket-remote-compaction-followup"}
	selected, err := selectedScenarios(*scenarioFlag, order, scenarios)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, name := range selected {
		if err := scenarios[name](ctx); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("ok %s\n", name)
	}
}

func selectedScenarios(input string, order []string, scenarios map[string]labScenario) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" || input == "all" {
		return order, nil
	}

	parts := strings.Split(input, ",")
	selected := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if _, ok := scenarios[name]; !ok {
			names := make([]string, 0, len(scenarios)+1)
			names = append(names, "all")
			for scenarioName := range scenarios {
				names = append(names, scenarioName)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("unknown scenario %q; valid scenarios: %s", name, strings.Join(names, ", "))
		}
		selected = append(selected, name)
	}
	if len(selected) == 0 {
		return nil, errors.New("no scenarios selected")
	}
	return selected, nil
}

func runCompactShapeScenario(ctx context.Context) error {
	env, err := newLabEnv()
	if err != nil {
		return err
	}
	defer env.close()

	body := map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItem("system", "system compact context"),
			messageItem("user", "hello remote compact"),
			messageItem("assistant", "FIRST_REMOTE_REPLY"),
		},
	}
	respBody, err := postJSON(ctx, env.proxyURL+"/v1/responses/compact", nil, body)
	if err != nil {
		return err
	}

	output, err := responseOutput(respBody)
	if err != nil {
		return err
	}
	if len(output) != 3 {
		return fmt.Errorf("expected retained system/user messages plus compaction item, got %d output items: %s", len(output), respBody)
	}
	if got, err := messageText(output[0], "system"); err != nil || got != "system compact context" {
		if err != nil {
			return err
		}
		return fmt.Errorf("expected compact response to retain original system history, got %q", got)
	}
	if got, err := messageText(output[1], "user"); err != nil || got != "hello remote compact" {
		if err != nil {
			return err
		}
		return fmt.Errorf("expected compact response to retain original user history, got %q", got)
	}
	if got := itemType(output[2]); got != "compaction" {
		return fmt.Errorf("expected final output item to be compaction, got %q", got)
	}
	for _, item := range output {
		if role := itemRole(item); role == "assistant" {
			return fmt.Errorf("compact response included duplicate assistant summary message: %s", item)
		}
	}
	return nil
}

func runUnknownTokenScenario(ctx context.Context) error {
	env, err := newLabEnv()
	if err != nil {
		return err
	}
	defer env.close()

	const opaqueToken = "opaque+server/token/with/slashes=="
	body := map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": opaqueToken,
			},
			messageItem("user", "new request wins"),
		},
	}
	if _, err := postJSON(ctx, env.proxyURL+"/v1/responses", nil, body); err != nil {
		return err
	}

	requests := env.upstream.snapshot()
	if len(requests) != 1 {
		return fmt.Errorf("expected one upstream request, got %d", len(requests))
	}
	input, err := rawJSONArray(requests[0].Body["input"])
	if err != nil {
		return err
	}
	if len(input) != 2 {
		return fmt.Errorf("expected two upstream input items, got %d: %s", len(input), requests[0].Body["input"])
	}
	first, err := rawJSONObject(input[0])
	if err != nil {
		return err
	}
	if got := rawJSONToString(first["type"]); got != "compaction" {
		return fmt.Errorf("expected unknown token to remain a compaction item, got %q", got)
	}
	if got := rawJSONToString(first["encrypted_content"]); got != opaqueToken {
		return fmt.Errorf("expected unknown token to be preserved, got %q", got)
	}
	return nil
}

func runRemoteCompactionV2Scenario(ctx context.Context) error {
	env, err := newLabEnv()
	if err != nil {
		return err
	}
	defer env.close()

	body := map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			messageItem("user", "USER_ONE"),
			messageItem("assistant", "FIRST_REMOTE_REPLY"),
			map[string]interface{}{"type": "compaction_trigger"},
		},
		"tools":               []interface{}{},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"stream":              true,
	}
	headers := http.Header{"X-Codex-Beta-Features": []string{"remote_compaction_v2"}}
	respBody, err := postJSON(ctx, env.proxyURL+"/v1/responses", headers, body)
	if err != nil {
		return err
	}

	events, err := parseSSEData(bytes.NewReader(respBody))
	if err != nil {
		return err
	}
	if !hasCompactionOutput(events) {
		return fmt.Errorf("expected remote compaction v2 SSE to contain a compaction output item, got %#v", events)
	}

	requests := env.upstream.snapshot()
	if len(requests) != 1 {
		return fmt.Errorf("expected one compact upstream request, got %d", len(requests))
	}
	instructions := rawJSONToString(requests[0].Body["instructions"])
	if !strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
		return fmt.Errorf("expected compact prompt instructions, got %q", instructions)
	}
	input, err := rawJSONArray(requests[0].Body["input"])
	if err != nil {
		return err
	}
	for _, item := range input {
		if itemType(item) == "compaction_trigger" {
			return fmt.Errorf("compact upstream request forwarded compaction_trigger: %s", requests[0].Body["input"])
		}
	}
	return nil
}

func runRemoteCompactionV2PreviousResponseScenario(ctx context.Context) error {
	env, err := newLabEnv()
	if err != nil {
		return err
	}
	defer env.close()

	body := map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev",
		"input":                []interface{}{map[string]interface{}{"type": "compaction_trigger"}},
		"stream":               true,
	}
	headers := http.Header{"X-Codex-Beta-Features": []string{"remote_compaction_v2"}}
	respBody, err := postJSON(ctx, env.proxyURL+"/v1/responses", headers, body)
	if err != nil {
		return err
	}

	events, err := parseSSEData(bytes.NewReader(respBody))
	if err != nil {
		return err
	}
	if !hasCompactionOutput(events) {
		return fmt.Errorf("expected remote compaction v2 previous-response SSE to contain a compaction output item, got %#v", events)
	}

	requests := env.upstream.snapshot()
	if len(requests) != 1 {
		return fmt.Errorf("expected one compact upstream request, got %d", len(requests))
	}
	if got := rawJSONToString(requests[0].Body["previous_response_id"]); got != "resp-prev" {
		return fmt.Errorf("expected compact fallback to preserve previous_response_id, got %q", got)
	}
	input, err := rawJSONArray(requests[0].Body["input"])
	if err != nil {
		return err
	}
	if len(input) != 0 {
		return fmt.Errorf("expected compact fallback to strip only the trigger from delta input, got %s", requests[0].Body["input"])
	}
	return nil
}

func runWebSocketResponseProcessedScenario(ctx context.Context) error {
	env, err := newLabEnv()
	if err != nil {
		return err
	}
	defer env.close()

	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, env.wsURL()+"/v1/responses", nil)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := writeWebSocketJSON(ctx, conn, responseCreateFrame("hello")); err != nil {
		return err
	}
	if _, err := readWebSocketJSON(ctx, conn); err != nil {
		return err
	}
	if _, err := readWebSocketJSON(ctx, conn); err != nil {
		return err
	}

	if err := writeWebSocketJSON(ctx, conn, map[string]interface{}{"type": "response.processed", "response_id": "resp-1"}); err != nil {
		return err
	}
	if err := writeWebSocketJSON(ctx, conn, responseCreateFrame("second")); err != nil {
		return err
	}

	created, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	if created["type"] != "response.created" {
		return fmt.Errorf("expected response.created after response.processed, got %v", created["type"])
	}
	completed, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	if completed["type"] != "response.completed" {
		return fmt.Errorf("expected response.completed after response.processed, got %v", completed["type"])
	}

	requests := env.upstream.snapshot()
	if len(requests) != 2 {
		return fmt.Errorf("expected response.processed not to create upstream request; got %d upstream requests", len(requests))
	}
	return nil
}

func runWebSocketRemoteCompactionFollowUpScenario(ctx context.Context) error {
	env, err := newLabEnv()
	if err != nil {
		return err
	}
	defer env.close()

	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, env.wsURL()+"/v1/responses", nil)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := writeWebSocketJSON(ctx, conn, responseCreateFrame("first")); err != nil {
		return err
	}
	firstCreated, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	if _, err := readWebSocketJSON(ctx, conn); err != nil {
		return err
	}
	firstID, err := websocketResponseID(firstCreated)
	if err != nil {
		return err
	}

	compact := responseCreateFrameWithInput([]interface{}{map[string]interface{}{"type": "compaction_trigger"}})
	compact["previous_response_id"] = firstID
	if err := writeWebSocketJSON(ctx, conn, compact); err != nil {
		return err
	}
	if _, err := readWebSocketJSON(ctx, conn); err != nil {
		return err
	}
	compactionOutput, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	if compactionOutput["type"] != "response.output_item.done" {
		return fmt.Errorf("expected compaction output item, got %v", compactionOutput["type"])
	}
	compactCompleted, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	compactionID, err := websocketResponseID(compactCompleted)
	if err != nil {
		return err
	}

	if err := writeWebSocketJSON(ctx, conn, map[string]interface{}{"type": "response.processed", "response_id": compactionID}); err != nil {
		return err
	}
	followUp := responseCreateFrame("after")
	followUp["previous_response_id"] = compactionID
	if err := writeWebSocketJSON(ctx, conn, followUp); err != nil {
		return err
	}
	followUpCreated, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	if followUpCreated["type"] != "response.created" {
		return fmt.Errorf("expected normal follow-up response.created, got %v", followUpCreated["type"])
	}
	followUpCompleted, err := readWebSocketJSON(ctx, conn)
	if err != nil {
		return err
	}
	if followUpCompleted["type"] != "response.completed" {
		return fmt.Errorf("expected normal follow-up response.completed, got %v", followUpCompleted["type"])
	}

	requests := env.upstream.snapshot()
	if len(requests) != 3 {
		return fmt.Errorf("expected first turn, compaction, and normal follow-up upstream requests; got %d", len(requests))
	}
	if got := requests[1].Header.Get("X-Codex-Turn-State"); got != "" {
		return fmt.Errorf("compaction request used turn-state delta instead of full history: %q", got)
	}
	if got := requests[2].Header.Get("X-Codex-Turn-State"); got != "" {
		return fmt.Errorf("follow-up request used stale turn state after synthetic compaction: %q", got)
	}
	if strings.Contains(rawJSONToString(requests[2].Body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
		return fmt.Errorf("follow-up request repeated compaction instead of normal response: %s", requests[2].Body["input"])
	}
	compactInput, err := rawJSONArray(requests[1].Body["input"])
	if err != nil {
		return err
	}
	compactInputBytes, err := json.Marshal(compactInput)
	if err != nil {
		return err
	}
	if !strings.Contains(string(compactInputBytes), "first") {
		return fmt.Errorf("compaction request did not include full prior websocket history: %s", requests[1].Body["input"])
	}
	input, err := rawJSONArray(requests[2].Body["input"])
	if err != nil {
		return err
	}
	if len(input) != 2 {
		return fmt.Errorf("expected follow-up replay to contain compaction plus new user message, got %d: %s", len(input), requests[2].Body["input"])
	}
	if got := itemType(input[0]); got != "message" || itemRole(input[0]) != "developer" || !strings.Contains(string(input[0]), "lab checkpoint summary") {
		return fmt.Errorf("expected follow-up replay to start with developer checkpoint, got %s", input[0])
	}
	if got := itemType(input[1]); got == "compaction_trigger" {
		return fmt.Errorf("follow-up replay included stale compaction_trigger: %s", requests[2].Body["input"])
	}
	if itemRole(input[1]) != "user" || !strings.Contains(string(input[1]), "after") {
		return fmt.Errorf("expected follow-up replay to include latest user message, got %s", input[1])
	}
	return nil
}

type labEnv struct {
	proxyURL string
	close    func()
	upstream *fakeUpstream
}

func newLabEnv() (*labEnv, error) {
	fake := &fakeUpstream{}
	upstream := httptest.NewServer(fake)

	handler, err := proxy.NewProxyHandler(
		auth.NewTestAuthenticator("lab-token"),
		logger.New(logger.LevelError),
		proxy.WithCopilotBaseURL(upstream.URL),
		proxy.WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{
			Enabled:            true,
			TurnStateDelta:     true,
			DisableAutoCompact: true,
		}),
	)
	if err != nil {
		upstream.Close()
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", handler.HandleResponses)
	mux.HandleFunc("POST /v1/responses/compact", handler.HandleCompact)
	mux.HandleFunc("GET /v1/responses", handler.HandleResponsesWebSocket)

	server := httptest.NewServer(mux)
	return &labEnv{
		proxyURL: server.URL,
		upstream: fake,
		close: func() {
			server.Close()
			upstream.Close()
		},
	}, nil
}

func (e *labEnv) wsURL() string {
	return "ws" + strings.TrimPrefix(e.proxyURL, "http")
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","supported_endpoints":["/responses"]}]}`))
		return
	}
	if r.URL.Path != "/responses" {
		http.NotFound(w, r)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, upstreamRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	requestNumber := len(f.requests)
	f.mu.Unlock()

	if strings.Contains(rawJSONToString(body["instructions"]), "CONTEXT CHECKPOINT COMPACTION") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"lab checkpoint summary"}]}]}`))
		return
	}

	stream := rawJSONToBool(body["stream"])
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", fmt.Sprintf("turn-state-%d", requestNumber))
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-%d\"}}\n\n", requestNumber)
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-%d\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n", requestNumber)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"resp-%d","object":"response","status":"completed","output":[]}`, requestNumber)
}

func (f *fakeUpstream) snapshot() []upstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upstreamRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func postJSON(ctx context.Context, url string, headers http.Header, payload interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST %s returned HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func writeWebSocketJSON(ctx context.Context, conn *websocket.Conn, payload interface{}) error {
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("write websocket JSON: %w", err)
	}
	return nil
}

func readWebSocketJSON(ctx context.Context, conn *websocket.Conn) (map[string]interface{}, error) {
	deadline, _ := ctx.Deadline()
	if deadline.IsZero() {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	for {
		var payload map[string]interface{}
		if err := conn.ReadJSON(&payload); err != nil {
			return nil, fmt.Errorf("read websocket JSON: %w", err)
		}
		if payload["type"] == "codex.response.metadata" {
			continue
		}
		return payload, nil
	}
}

func responseCreateFrame(text string) map[string]interface{} {
	return responseCreateFrameWithInput([]interface{}{messageItem("user", text)})
}

func responseCreateFrameWithInput(input []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type":                "response.create",
		"model":               "gpt-5.4",
		"instructions":        "You are helpful",
		"input":               input,
		"tools":               []interface{}{},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"store":               false,
		"stream":              true,
		"include":             []string{},
	}
}

func websocketResponseID(payload map[string]interface{}) (string, error) {
	response, ok := payload["response"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("websocket payload missing response object: %#v", payload)
	}
	id, ok := response["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("websocket payload missing response id: %#v", payload)
	}
	return id, nil
}

func messageItem(role, text string) map[string]interface{} {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]interface{}{
		"type": "message",
		"role": role,
		"content": []map[string]string{
			{"type": contentType, "text": text},
		},
	}
}

func responseOutput(body []byte) ([]json.RawMessage, error) {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode response JSON %s: %w", string(body), err)
	}
	return rawJSONArray(resp["output"])
}

func rawJSONArray(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decode JSON array %s: %w", string(raw), err)
	}
	return items, nil
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("decode JSON object %s: %w", string(raw), err)
	}
	return item, nil
}

func rawJSONToString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawJSONToBool(raw json.RawMessage) bool {
	var value bool
	_ = json.Unmarshal(raw, &value)
	return value
}

func itemType(raw json.RawMessage) string {
	item, err := rawJSONObject(raw)
	if err != nil {
		return ""
	}
	return rawJSONToString(item["type"])
}

func itemRole(raw json.RawMessage) string {
	item, err := rawJSONObject(raw)
	if err != nil {
		return ""
	}
	return rawJSONToString(item["role"])
}

func messageText(raw json.RawMessage, wantRole string) (string, error) {
	item, err := rawJSONObject(raw)
	if err != nil {
		return "", err
	}
	if gotType := rawJSONToString(item["type"]); gotType != "message" {
		return "", fmt.Errorf("expected message item, got %q in %s", gotType, raw)
	}
	if gotRole := rawJSONToString(item["role"]); gotRole != wantRole {
		return "", fmt.Errorf("expected role %q, got %q in %s", wantRole, gotRole, raw)
	}
	content, err := rawJSONArray(item["content"])
	if err != nil {
		return "", err
	}
	var pieces []string
	for _, rawPart := range content {
		part, err := rawJSONObject(rawPart)
		if err != nil {
			return "", err
		}
		if len(part["text"]) == 0 {
			continue
		}
		pieces = append(pieces, rawJSONToString(part["text"]))
	}
	return strings.Join(pieces, "\n"), nil
}

func parseSSEData(r io.Reader) ([]map[string]interface{}, error) {
	scanner := bufio.NewScanner(r)
	var dataLines []string
	var events []map[string]interface{}
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return nil
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("decode SSE data %s: %w", data, err)
		}
		events = append(events, event)
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return events, nil
}

func hasCompactionOutput(events []map[string]interface{}) bool {
	for _, event := range events {
		if event["type"] != "response.output_item.done" {
			continue
		}
		item, ok := event["item"].(map[string]interface{})
		if !ok {
			return false
		}
		if item["type"] == "compaction" && strings.TrimSpace(fmt.Sprint(item["encrypted_content"])) != "" {
			return true
		}
	}
	return false
}
