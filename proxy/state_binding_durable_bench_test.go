package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

func durableBenchmarkStore(b *testing.B, durable bool, occupancy int) (*stateBindingStore, DurableStateBindingsConfig) {
	b.Helper()
	config := DurableStateBindingsConfig{MaxEntries: occupancy + b.N*8 + 32}
	var s *stateBindingStore
	var err error
	if durable {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			b.Skip("durable mode requires Linux or macOS")
		}
		dir := b.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		config.Path = filepath.Join(dir, "state.db")
		s, err = newDurableStateBindingStore(config)
	} else {
		s, err = newStateBindingStore(stateBindingStoreConfig{maxEntries: config.MaxEntries})
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.close() })
	if occupancy > 0 {
		tokens := make([]stateBindingToken, occupancy)
		for i := range tokens {
			tokens[i] = stateBindingToken{stateBindingTypeResponseID, fmt.Sprintf("seed-%d", i)}
		}
		if r := s.bindAll(tokens, durableFixtureOwner()); r.err != nil {
			b.Fatal(r.err)
		}
	}
	return s, config
}

type stateExposureBenchmarkWriter struct {
	headers http.Header
	first   time.Time
}

func (w *stateExposureBenchmarkWriter) Header() http.Header { return w.headers }
func (w *stateExposureBenchmarkWriter) WriteHeader(int) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
}
func (w *stateExposureBenchmarkWriter) Write(data []byte) (int, error) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
	return len(data), nil
}

func stateExposureBenchmarkBody(batch, group int) []byte {
	items := make([]any, 0, batch-1)
	for i := 1; i < batch; i++ {
		items = append(items, map[string]any{"type": "reasoning", "encrypted_content": fmt.Sprintf("state-%d-%d", group, i), "summary": []any{}})
	}
	body, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("resp-%d", group), "status": "completed", "model": "physical", "output": items})
	return body
}

// This is the real JSON/SSE binding-before-exposure seam with synchronous disk
// commits enabled. It excludes provider/network latency and process startup.
func BenchmarkStateBindingExposure(b *testing.B) {
	for _, durable := range []bool{false, true} {
		for _, occupancy := range []int{0, 8192} {
			for _, batch := range []int{1, 8} {
				for _, stream := range []bool{false, true} {
					for _, repeated := range []bool{false, true} {
						b.Run(fmt.Sprintf("durable=%t/occupancy=%d/batch=%d/sse=%t/repeated=%t", durable, occupancy, batch, stream, repeated), func(b *testing.B) {
							s, config := durableBenchmarkStore(b, durable, occupancy)
							h := &ProxyHandler{stateBindings: s}
							h.stateBindingsOnce.Do(func() {})
							info := explicitRouteResponseInfo{routeID: "route", targetID: "target", publicID: "public", stateIdentity: [32]byte{1}}
							write := func(body []byte, w *stateExposureBenchmarkWriter) error {
								if stream {
									body = []byte(responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+string(body)+`}`))
									reader := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(bytes.NewReader(body)), info)
									defer func() { _ = reader.Close() }()
									_, err := io.Copy(w, reader)
									return err
								}
								resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}
								return writeExplicitResponsesResponse(context.Background(), h, w, resp, info, nil, "")
							}
							if repeated {
								if err := write(stateExposureBenchmarkBody(batch, 0), &stateExposureBenchmarkWriter{headers: make(http.Header)}); err != nil {
									b.Fatal(err)
								}
							}
							samples := make([]int64, 0, min(b.N, 4096))
							b.ReportAllocs()
							b.ResetTimer()
							for i := 0; i < b.N; i++ {
								group := i + 1
								if repeated {
									group = 0
								}
								body := stateExposureBenchmarkBody(batch, group)
								w := &stateExposureBenchmarkWriter{headers: make(http.Header)}
								start := time.Now()
								if err := write(body, w); err != nil {
									b.Fatal(err)
								}
								if len(samples) < cap(samples) {
									samples = append(samples, w.first.Sub(start).Nanoseconds())
								}
							}
							b.StopTimer()
							sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
							b.ReportMetric(float64(samples[len(samples)/2]), "exposure-p50-ns")
							b.ReportMetric(float64(samples[(len(samples)-1)*95/100]), "exposure-p95-ns")
							if durable {
								stat, err := os.Stat(config.Path)
								if err != nil {
									b.Fatal(err)
								}
								b.ReportMetric(float64(stat.Size()), "db-bytes")
								b.ReportMetric(float64(stat.Size())/float64(s.stats().entries), "db-bytes/record")
							}
						})
					}
				}
			}
		}
	}
}

func BenchmarkDurableStateLookupAndOpen(b *testing.B) {
	for _, occupancy := range []int{1, 8192} {
		b.Run(fmt.Sprintf("lookup/occupancy=%d", occupancy), func(b *testing.B) {
			s, _ := durableBenchmarkStore(b, true, occupancy)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if r := s.lookup(stateBindingTypeResponseID, "seed-0"); r.err != nil || r.outcome != stateBindingLookupKnown {
					b.Fatal("lookup failed")
				}
			}
		})
		b.Run(fmt.Sprintf("reopen/occupancy=%d", occupancy), func(b *testing.B) {
			s, config := durableBenchmarkStore(b, true, occupancy)
			if err := s.close(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reopened, err := newDurableStateBindingStore(config)
				if err != nil {
					b.Fatal(err)
				}
				if err := reopened.close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Measure the complete writers, not just the duplicate-key scanner. Existing
// proof avoids fsync variability so allocation growth in nested extensions is
// visible. JSON diagnostic paths must not make that growth quadratic.
func BenchmarkDurableNestedResponseValidation(b *testing.B) {
	for _, depth := range []int{1000, 2000, 4000} {
		for _, stream := range []bool{false, true} {
			b.Run(fmt.Sprintf("depth=%d/sse=%t", depth, stream), func(b *testing.B) {
				s, _ := durableBenchmarkStore(b, true, 0)
				h := &ProxyHandler{stateBindings: s}
				h.stateBindingsOnce.Do(func() {})
				info := explicitRouteResponseInfo{routeID: "route", targetID: "target", publicID: "public", stateIdentity: [32]byte{1}}
				if err := h.bindExplicitStateTokens(info, []stateBindingToken{{stateBindingTypeResponseID, "nested-response-fixture"}}); err != nil {
					b.Fatal(err)
				}
				body := []byte(durableNestedResponseFixture(depth, "0"))
				if stream {
					body = []byte(responsesRouteSSE("response.completed", `{"type":"response.completed","response":`+string(body)+`}`))
				}
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for range b.N {
					if stream {
						reader := normalizeResponsesStreamBodyWithBinding(h, io.NopCloser(bytes.NewReader(body)), info)
						_, err := io.Copy(io.Discard, reader)
						_ = reader.Close()
						if err != nil {
							b.Fatal(err)
						}
					} else {
						w := &stateExposureBenchmarkWriter{headers: make(http.Header)}
						resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
						if err := writeExplicitResponsesResponse(context.Background(), h, w, resp, info, nil, ""); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
