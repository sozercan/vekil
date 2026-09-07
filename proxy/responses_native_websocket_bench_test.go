package proxy

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// Compare warm downstream websocket turns using the same fixed 400-message
// history. Empty continuation input and output keep replay size constant.
func BenchmarkResponsesTransportNativeUpstream(b *testing.B) {
	for _, native := range []bool{false, true} {
		name := "HTTPBridge"
		if native {
			name = "NativeWebSocket"
		}
		b.Run(name, func(b *testing.B) {
			var uploaded atomic.Int64
			const terminal = `{"type":"response.completed","response":{"id":"resp-bench-native","output":[]}}`
			h := newTestProxyHandler(b, func(w http.ResponseWriter, r *http.Request) {
				if !native {
					count, err := io.Copy(io.Discard, r.Body)
					if err != nil {
						b.Error(err)
					}
					uploaded.Add(count)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+terminal+"\n\n")
					return
				}
				conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
				if err != nil {
					b.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					_, body, err := conn.ReadMessage()
					if err != nil {
						return
					}
					uploaded.Add(int64(len(body)))
					if conn.WriteMessage(websocket.TextMessage, []byte(terminal)) != nil {
						return
					}
				}
			})
			h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: native, DisableAutoCompact: true}
			env := newResponsesTransportBenchmarkEnvWithHandler(b, h)
			conn := dialResponsesTransportWebSocket(b, newResponsesTransportWebSocketDialer(b, env.proxyServer), env.webSocketURL)
			defer func() { _ = conn.Close() }()
			seed := marshalBenchmarkResponsesWebSocketFrame(b, benchmarkResponsesWebSocketHistory(200, 256), "", nil)
			delta := marshalBenchmarkResponsesWebSocketFrame(b, nil, "resp-bench-native", nil)
			runResponsesTransportWebSocketFrame(b, conn, seed, 1)
			uploaded.Store(0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runResponsesTransportWebSocketFrame(b, conn, delta, 1)
			}
			b.StopTimer()
			b.ReportMetric(float64(uploaded.Load())/float64(b.N), "upstream-B/op")
		})
	}
}
