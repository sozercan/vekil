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
const benchmarkResponsesOutputJSON = "{\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-bench-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}"
const benchmarkResponsesCompletedJSON = "{\"type\":\"response.completed\",\"response\":{\"id\":\"resp-bench-1\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}"
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

type responsesTransportBenchmarkEnv struct {
	proxyServer  *httptest.Server
	webSocketURL string
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

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, benchmarkResponsesStreamingSSE); err != nil {
			b.Fatalf("write upstream benchmark response: %v", err)
		}
	})

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
	b.Helper()

	if err := conn.WriteMessage(websocket.TextMessage, benchmarkResponsesWebSocketRequestFrame); err != nil {
		b.Fatalf("write WSS benchmark frame: %v", err)
	}

	for i := 0; i < 3; i++ {
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
