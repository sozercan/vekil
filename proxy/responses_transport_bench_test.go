package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

const benchmarkResponsesCreatedJSON = "{\"type\":\"response.created\",\"response\":{\"id\":\"resp-bench-1\"}}"
const benchmarkResponsesOutputItemJSON = "{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-bench-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}"
const benchmarkResponsesOutputJSON = "{\"type\":\"response.output_item.done\",\"item\":" + benchmarkResponsesOutputItemJSON + "}"
const benchmarkResponsesCompletedJSON = "{\"type\":\"response.completed\",\"response\":{\"id\":\"resp-bench-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}"
const benchmarkResponsesWarmupResponseID = "copilot-proxy-ws-00000000-0000-0000-0000-000000000000"
const benchmarkResponsesWarmupCreatedJSON = "{\"type\":\"response.created\",\"response\":{\"id\":\"" + benchmarkResponsesWarmupResponseID + "\"}}"
const benchmarkResponsesWarmupCompletedJSON = "{\"type\":\"response.completed\",\"response\":{\"id\":\"" + benchmarkResponsesWarmupResponseID + "\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}"
const benchmarkResponsesStreamingSSE = "event: response.created\ndata: " + benchmarkResponsesCreatedJSON + "\n\n" +
	"event: response.output_item.done\ndata: " + benchmarkResponsesOutputJSON + "\n\n" +
	"event: response.completed\ndata: " + benchmarkResponsesCompletedJSON + "\n\n"

var benchmarkResponsesStreamingRequestBody = []byte(`{
  "model": "gpt-5.4",
  "stream": true,
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        {
          "type": "input_text",
          "text": "hello"
        }
      ]
    }
  ]
}`)

var benchmarkResponsesWebSocketRequestFrame = []byte(`{
  "type": "response.create",
  "model": "gpt-5.4",
  "instructions": "You are helpful",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        {
          "type": "input_text",
          "text": "hello"
        }
      ]
    }
  ]
}`)

func BenchmarkResponsesTransportHTTPSStreamingWarm(b *testing.B) {
	benchmarkResponsesTransportHTTPSStreaming(b, false)
}

func BenchmarkResponsesTransportHTTPSStreamingCold(b *testing.B) {
	benchmarkResponsesTransportHTTPSStreaming(b, true)
}

func BenchmarkResponsesTransportWSSWarm(b *testing.B) {
	benchmarkResponsesTransportWSS(b, false)
}

func BenchmarkResponsesTransportWSSCold(b *testing.B) {
	benchmarkResponsesTransportWSS(b, true)
}

func BenchmarkResponsesSessionHTTPSFullReplayLongHistoryWarm(b *testing.B) {
	fixtures := newResponsesSessionBenchmarkFixtures(b)
	env := newResponsesTransportBenchmarkEnvWithHandler(b, newTestProxyHandler(b, func(w http.ResponseWriter, r *http.Request) {
		discardBenchmarkRequestBody(b, r)
		writeBenchmarkResponsesStream(b, w, "")
	}))
	client := newResponsesTransportHTTPClient(b, env.proxyServer, false)

	totalBytes := 2*len(fixtures.fullReplayPOSTBody) + 2*len(benchmarkResponsesStreamingSSE)
	b.ReportAllocs()
	b.SetBytes(int64(totalBytes))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest(http.MethodPost, env.proxyServer.URL+"/v1/responses", bytes.NewReader(fixtures.fullReplayPOSTBody))
		if err != nil {
			b.Fatalf("create long-history HTTPS request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("perform long-history HTTPS request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			b.Fatalf("unexpected long-history HTTPS status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			_ = resp.Body.Close()
			b.Fatalf("drain long-history HTTPS response: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			b.Fatalf("close long-history HTTPS response: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkResponsesSessionWSSFullReplayLongHistoryWarm(b *testing.B) {
	benchmarkResponsesSessionWSSLongHistoryFollowup(b, false)
}

func BenchmarkResponsesSessionWSSDeltaReplayLongHistoryWarm(b *testing.B) {
	benchmarkResponsesSessionWSSLongHistoryFollowup(b, true)
}

func BenchmarkResponsesSessionWSSWarmupLocalWarm(b *testing.B) {
	fixtures := newResponsesSessionBenchmarkFixtures(b)
	var upstreamRequests int

	env := newResponsesTransportBenchmarkEnvWithHandler(b, newTestProxyHandler(b, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		http.Error(w, "warmup benchmark should not hit upstream", http.StatusInternalServerError)
	}))
	dialer := newResponsesTransportWebSocketDialer(b, env.proxyServer)
	conn := dialResponsesTransportWebSocket(b, dialer, env.webSocketURL)
	defer func() {
		if err := conn.Close(); err != nil {
			b.Fatalf("close warmup WSS connection: %v", err)
		}
	}()

	totalBytes := len(fixtures.warmupFrame) + len(benchmarkResponsesWarmupCreatedJSON) + len(benchmarkResponsesWarmupCompletedJSON)
	b.ReportAllocs()
	b.SetBytes(int64(totalBytes))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		runResponsesTransportWebSocketFrame(b, conn, fixtures.warmupFrame, 2)
	}
	b.StopTimer()

	if upstreamRequests != 0 {
		b.Fatalf("warmup benchmark unexpectedly hit upstream %d times", upstreamRequests)
	}
}

func benchmarkResponsesTransportHTTPSStreaming(b *testing.B, cold bool) {
	b.Helper()

	env := newResponsesTransportBenchmarkEnv(b)
	client := newResponsesTransportHTTPClient(b, env.proxyServer, cold)

	totalBytes := len(benchmarkResponsesStreamingRequestBody) + len(benchmarkResponsesStreamingSSE)
	b.ReportAllocs()
	b.SetBytes(int64(totalBytes))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest(http.MethodPost, env.proxyServer.URL+"/v1/responses", bytes.NewReader(benchmarkResponsesStreamingRequestBody))
		if err != nil {
			b.Fatalf("create HTTPS request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("perform HTTPS request: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			b.Fatalf("unexpected HTTPS status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			_ = resp.Body.Close()
			b.Fatalf("drain HTTPS response: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			b.Fatalf("close HTTPS response: %v", err)
		}
	}
}

func benchmarkResponsesTransportWSS(b *testing.B, cold bool) {
	b.Helper()

	env := newResponsesTransportBenchmarkEnv(b)
	dialer := newResponsesTransportWebSocketDialer(b, env.proxyServer)

	totalBytes := len(benchmarkResponsesWebSocketRequestFrame) +
		len(benchmarkResponsesCreatedJSON) +
		len(benchmarkResponsesOutputJSON) +
		len(benchmarkResponsesCompletedJSON)
	b.ReportAllocs()
	b.SetBytes(int64(totalBytes))

	if cold {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			conn := dialResponsesTransportWebSocket(b, dialer, env.webSocketURL)
			runResponsesTransportWebSocketTurn(b, conn)
			if err := conn.Close(); err != nil {
				b.Fatalf("close WSS connection: %v", err)
			}
		}
		return
	}

	conn := dialResponsesTransportWebSocket(b, dialer, env.webSocketURL)
	defer func() {
		if err := conn.Close(); err != nil {
			b.Fatalf("close warm WSS connection: %v", err)
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runResponsesTransportWebSocketTurn(b, conn)
	}
}

func benchmarkResponsesSessionWSSLongHistoryFollowup(b *testing.B, enableDelta bool) {
	b.Helper()

	fixtures := newResponsesSessionBenchmarkFixtures(b)
	handler := newTestProxyHandler(b, func(w http.ResponseWriter, r *http.Request) {
		discardBenchmarkRequestBody(b, r)
		turnState := ""
		if enableDelta {
			turnState = "turn-state-bench"
		}
		writeBenchmarkResponsesStream(b, w, turnState)
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		TurnStateDelta:     enableDelta,
		DisableAutoCompact: true,
	}

	env := newResponsesTransportBenchmarkEnvWithHandler(b, handler)
	dialer := newResponsesTransportWebSocketDialer(b, env.proxyServer)
	conn := dialResponsesTransportWebSocket(b, dialer, env.webSocketURL)
	defer func() {
		if err := conn.Close(); err != nil {
			b.Fatalf("close long-history WSS connection: %v", err)
		}
	}()

	upstreamBodyLen := len(fixtures.fullReplayUpstreamBody)
	if enableDelta {
		upstreamBodyLen = len(fixtures.deltaReplayUpstreamBody)
	}
	totalBytes := len(fixtures.followupFrame) +
		upstreamBodyLen +
		len(benchmarkResponsesStreamingSSE) +
		len(benchmarkResponsesCreatedJSON) +
		len(benchmarkResponsesOutputJSON) +
		len(benchmarkResponsesCompletedJSON)

	b.ReportAllocs()
	b.SetBytes(int64(totalBytes))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		runResponsesTransportWebSocketFrame(b, conn, fixtures.seedFrame, 3)
		b.StartTimer()
		runResponsesTransportWebSocketFrame(b, conn, fixtures.followupFrame, 3)
	}
	b.StopTimer()
}

type responsesTransportBenchmarkEnv struct {
	proxyServer  *httptest.Server
	webSocketURL string
}

type responsesSessionBenchmarkFixtures struct {
	fullReplayPOSTBody      []byte
	seedFrame               []byte
	followupFrame           []byte
	warmupFrame             []byte
	fullReplayUpstreamBody  []byte
	deltaReplayUpstreamBody []byte
}

func newResponsesTransportBenchmarkEnv(b *testing.B) responsesTransportBenchmarkEnv {
	b.Helper()

	handler := newTestProxyHandler(b, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			b.Fatalf("read upstream benchmark request: %v", err)
		}
		defer func() { _ = r.Body.Close() }()

		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid benchmark request JSON", http.StatusBadRequest)
			return
		}
		if !request.Stream {
			http.Error(w, "benchmark request must stream upstream", http.StatusBadRequest)
			return
		}

		writeBenchmarkResponsesStream(b, w, "")
	})

	return newResponsesTransportBenchmarkEnvWithHandler(b, handler)
}

func newResponsesTransportBenchmarkEnvWithHandler(b *testing.B, handler *ProxyHandler) responsesTransportBenchmarkEnv {
	b.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", handler.HandleResponses)
	mux.HandleFunc("GET /v1/responses", handler.HandleResponsesWebSocket)

	proxyServer := httptest.NewTLSServer(mux)
	b.Cleanup(proxyServer.Close)

	return responsesTransportBenchmarkEnv{
		proxyServer:  proxyServer,
		webSocketURL: "wss" + strings.TrimPrefix(proxyServer.URL, "https") + "/v1/responses",
	}
}

func newResponsesSessionBenchmarkFixtures(b *testing.B) responsesSessionBenchmarkFixtures {
	b.Helper()

	history := benchmarkResponsesWebSocketHistory(200, 256)
	followupInput := []json.RawMessage{
		marshalBenchmarkResponsesMessage(b, "user", "Continue using the latest file changes only."),
	}

	seedFrame := marshalBenchmarkResponsesWebSocketFrame(b, history, "", nil)
	followupFrame := marshalBenchmarkResponsesWebSocketFrame(b, followupInput, "resp-bench-1", nil)

	followupRequest, err := parseResponsesWebSocketCreateRequest(followupFrame)
	if err != nil {
		b.Fatalf("parse followup websocket frame: %v", err)
	}

	historyWithSeedOutput := append(cloneRawMessages(history), benchmarkResponsesOutputItemRaw())
	fullReplayUpstreamBody, err := followupRequest.upstreamBody(historyWithSeedOutput, followupRequest.Input)
	if err != nil {
		b.Fatalf("build full-replay upstream body: %v", err)
	}
	deltaReplayUpstreamBody, err := followupRequest.upstreamBody(followupRequest.Input)
	if err != nil {
		b.Fatalf("build delta upstream body: %v", err)
	}

	fullReplayInput := append(cloneRawMessages(historyWithSeedOutput), cloneRawMessages(followupRequest.Input)...)
	fullReplayPOSTBody := marshalBenchmarkResponsesPOSTRequest(b, fullReplayInput)

	generateFalse := false
	warmupFrame := marshalBenchmarkResponsesWebSocketFrame(b, followupInput, "", &generateFalse)

	return responsesSessionBenchmarkFixtures{
		fullReplayPOSTBody:      fullReplayPOSTBody,
		seedFrame:               seedFrame,
		followupFrame:           followupFrame,
		warmupFrame:             warmupFrame,
		fullReplayUpstreamBody:  fullReplayUpstreamBody,
		deltaReplayUpstreamBody: deltaReplayUpstreamBody,
	}
}

func newResponsesTransportHTTPClient(b *testing.B, server *httptest.Server, disableKeepAlives bool) *http.Client {
	b.Helper()

	baseTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		b.Fatalf("unexpected benchmark transport type %T", server.Client().Transport)
	}
	transport := baseTransport.Clone()
	transport.DisableKeepAlives = disableKeepAlives
	b.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func newResponsesTransportWebSocketDialer(b *testing.B, server *httptest.Server) *websocket.Dialer {
	b.Helper()

	baseTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		b.Fatalf("unexpected benchmark transport type %T", server.Client().Transport)
	}

	var tlsConfig *tls.Config
	if baseTransport.TLSClientConfig != nil {
		tlsConfig = baseTransport.TLSClientConfig.Clone()
	}

	return &websocket.Dialer{
		TLSClientConfig: tlsConfig,
	}
}

func dialResponsesTransportWebSocket(b *testing.B, dialer *websocket.Dialer, url string) *websocket.Conn {
	b.Helper()

	conn, resp, err := dialer.Dial(url, nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			b.Fatalf("dial WSS benchmark endpoint %s: %v (%s)", url, err, strings.TrimSpace(string(body)))
		}
		b.Fatalf("dial WSS benchmark endpoint %s: %v", url, err)
	}
	return conn
}

func runResponsesTransportWebSocketTurn(b *testing.B, conn *websocket.Conn) {
	runResponsesTransportWebSocketFrame(b, conn, benchmarkResponsesWebSocketRequestFrame, 3)
}

func runResponsesTransportWebSocketFrame(b *testing.B, conn *websocket.Conn, frame []byte, expectedMessages int) {
	b.Helper()

	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		b.Fatalf("write WSS benchmark frame: %v", err)
	}

	for i := 0; i < expectedMessages; i++ {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			b.Fatalf("read WSS benchmark frame %d: %v", i, err)
		}
		if messageType != websocket.TextMessage {
			b.Fatalf("unexpected WSS benchmark message type %d", messageType)
		}
		if len(payload) == 0 {
			b.Fatal("received empty WSS benchmark payload")
		}
	}
}

func writeBenchmarkResponsesStream(b *testing.B, w http.ResponseWriter, turnState string) {
	b.Helper()

	if turnState != "" {
		w.Header().Set("X-Codex-Turn-State", turnState)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err := io.WriteString(w, benchmarkResponsesStreamingSSE); err != nil {
		b.Fatalf("write upstream benchmark response: %v", err)
	}
}

func discardBenchmarkRequestBody(b *testing.B, r *http.Request) {
	b.Helper()

	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		b.Fatalf("discard benchmark request body: %v", err)
	}
	if err := r.Body.Close(); err != nil {
		b.Fatalf("close benchmark request body: %v", err)
	}
}

func marshalBenchmarkResponsesPOSTRequest(b *testing.B, input []json.RawMessage) []byte {
	b.Helper()

	body, err := json.Marshal(map[string]interface{}{
		"model":        "gpt-5.4",
		"instructions": "You are helpful",
		"stream":       true,
		"input":        input,
	})
	if err != nil {
		b.Fatalf("marshal benchmark POST request: %v", err)
	}
	return body
}

func marshalBenchmarkResponsesWebSocketFrame(b *testing.B, input []json.RawMessage, previousResponseID string, generate *bool) []byte {
	b.Helper()

	body := map[string]interface{}{
		"type":         "response.create",
		"model":        "gpt-5.4",
		"instructions": "You are helpful",
		"tools":        []interface{}{},
		"tool_choice":  "auto",
		"include":      []string{},
		"input":        input,
	}
	if previousResponseID != "" {
		body["previous_response_id"] = previousResponseID
	}
	if generate != nil {
		body["generate"] = *generate
	}

	frame, err := json.Marshal(body)
	if err != nil {
		b.Fatalf("marshal benchmark websocket frame: %v", err)
	}
	return frame
}

func marshalBenchmarkResponsesMessage(b *testing.B, role, text string) json.RawMessage {
	b.Helper()

	encoded, err := json.Marshal(map[string]interface{}{
		"type": "message",
		"role": role,
		"content": []map[string]string{
			{
				"type": "input_text",
				"text": text,
			},
		},
	})
	if err != nil {
		b.Fatalf("marshal benchmark message: %v", err)
	}
	return encoded
}

func benchmarkResponsesOutputItemRaw() json.RawMessage {
	return cloneRawMessage(json.RawMessage(benchmarkResponsesOutputItemJSON))
}
