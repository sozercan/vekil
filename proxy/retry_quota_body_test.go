package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const quotaBodyFixture = `{"error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`

type quotaBodyFinalRead struct {
	body string
	err  error
}

func (r *quotaBodyFinalRead) Read(p []byte) (int, error) {
	n := copy(p, r.body)
	r.body = r.body[n:]
	if len(r.body) == 0 {
		return n, r.err
	}
	return n, nil
}

func TestCaptureAndDrainResponseBodyTracksCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reader   io.Reader
		complete bool
		wantErr  error
	}{
		{"separate-eof", strings.NewReader(quotaBodyFixture), true, nil},
		{"data-with-eof", &quotaBodyFinalRead{quotaBodyFixture, io.EOF}, true, nil},
		{"data-with-error", &quotaBodyFinalRead{quotaBodyFixture, io.ErrUnexpectedEOF}, false, io.ErrUnexpectedEOF},
		{"later-error", io.MultiReader(strings.NewReader(quotaBodyFixture), &quotaBodyFinalRead{"", io.ErrUnexpectedEOF}), false, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureAndDrainResponseBody(io.NopCloser(tc.reader), upstreamErrorDetailMaxBodyBytes, 0)
			if string(capture.body) != quotaBodyFixture || capture.complete != tc.complete || !errors.Is(capture.err, tc.wantErr) {
				t.Fatalf("capture = %+v, want complete=%v error=%v", capture, tc.complete, tc.wantErr)
			}
			resp := &http.Response{StatusCode: 429, ContentLength: -1, Header: make(http.Header)}
			if restored := restoreFreeUsageLimitResponse(resp, capture); restored != tc.complete {
				t.Fatalf("restore = %v, want %v", restored, tc.complete)
			}
		})
	}
}

func TestRestoreFreeUsageLimitResponseRejectsInvalidGzip(t *testing.T) {
	compressed := gzipTestPayload(t, []byte(quotaBodyFixture))
	badChecksum := bytes.Clone(compressed)
	badChecksum[len(badChecksum)-8] ^= 0xff
	for _, tc := range []struct {
		name, encoding string
		body           []byte
	}{
		{"truncated-footer", "gzip", compressed[:len(compressed)-8]},
		{"invalid-checksum", "gzip", badChecksum},
		{"invalid-header", "gzip", []byte(quotaBodyFixture)},
		{"expanded-body-limit", "gzip", gzipTestPayload(t, []byte(quotaBodyFixture+strings.Repeat(" ", upstreamErrorDetailMaxBodyBytes)))},
		{"trailing-garbage", "gzip", append(bytes.Clone(compressed), []byte("not gzip")...)},
		{"unsupported-encoding", "br", []byte(quotaBodyFixture)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 429, ContentLength: -1, Header: http.Header{"Content-Encoding": {tc.encoding}}}
			if restoreFreeUsageLimitResponse(resp, responseBodyCapture{body: tc.body, complete: true}) {
				t.Fatal("restored unverified or over-budget decoded quota body")
			}
		})
	}
}

func TestDoWithRetry_FreeUsageLimitGzipPreservesEncoding(t *testing.T) {
	compressed := gzipTestPayload(t, []byte(quotaBodyFixture))
	for _, inference := range []bool{false, true} {
		for _, attempts := range []int{1, 3} {
			for _, knownLength := range []bool{false, true} {
				t.Run(fmt.Sprintf("inference=%t/attempts=%d/known-length=%t", inference, attempts, knownLength), func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					deadline := time.AfterFunc(time.Second, cancel)
					defer deadline.Stop()
					var calls atomic.Int32
					length := int64(-1)
					if knownLength {
						length = int64(len(compressed))
					}
					h := &ProxyHandler{
						maxRetries: attempts, retryBaseDelay: time.Nanosecond,
						client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
							calls.Add(1)
							if got := req.Header.Get("Accept-Encoding"); got != "gzip" {
								t.Fatalf("Accept-Encoding = %q, want operator-supplied gzip", got)
							}
							headers := http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"application/json"}, "Retry-After": {"60"}}
							if knownLength {
								headers.Set("Content-Length", strconv.Itoa(len(compressed)))
							}
							return &http.Response{StatusCode: 429, Request: req, ContentLength: length,
								Header: headers, Body: io.NopCloser(bytes.NewReader(compressed))}, nil
						})},
					}
					provider := &providerRuntime{
						id: "quota-gzip", kind: providerTypeOpenAICompatible, baseURL: "http://upstream.test", authType: providerAuthTypeNone,
						extraHeaders: http.Header{"Accept-Encoding": {"gzip"}}, paths: providerEndpointPaths{chatCompletions: providerEndpointChatCompletions},
					}
					if err := sealProviderRequestTemplates(provider); err != nil {
						t.Fatal(err)
					}
					resp, err := h.doWithRetryMode(func() (*http.Request, error) {
						return h.newProviderJSONInferenceRequest(ctx, provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{}`), nil, "")
					}, inference)
					if err != nil || resp == nil {
						t.Fatalf("response = %v, error = %v", resp, err)
					}
					defer func() { _ = resp.Body.Close() }()
					actual, err := io.ReadAll(resp.Body)
					if err != nil || !bytes.Equal(actual, compressed) || resp.StatusCode != 429 || calls.Load() != 1 {
						t.Fatalf("status=%d sends=%d unchanged-body=%t error=%v", resp.StatusCode, calls.Load(), bytes.Equal(actual, compressed), err)
					}
					wantHeaderLength := ""
					if knownLength {
						wantHeaderLength = strconv.Itoa(len(compressed))
					}
					if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Length") != wantHeaderLength || resp.ContentLength != length || resp.Uncompressed || resp.Header.Get("Retry-After") != "60" {
						t.Fatalf("response encoding metadata changed: %+v", resp)
					}
				})
			}
		}
	}
}

func TestExplicitRouteFreeUsageLimitStopsTargetFailover(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				if body["model"] == "deployment-a" {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					if terminal {
						_, _ = io.WriteString(w, quotaBodyFixture)
					} else {
						_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"retry another target"}}`)
					}
					return
				}
				_, _ = io.WriteString(w, `{"id":"second","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop","index":0}]}`)
			}))
			defer server.Close()
			primary := explicitRouteTestProvider("zen", server.URL, "public")
			primary.kind = providerTypeOpenAICompatible
			h, route := explicitRouteTestHandler(t, server.Client(), routeModePriorityFailover, 2, 2, primary, explicitRouteTestProvider("alias", server.URL, "public"))
			// Two model targets share the same configured provider and allowance.
			route.targets[1].provider = primary
			operation := newRouteOperation(route, context.Background())
			ctx := withRouteOperation(context.Background(), operation)
			response, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointChatCompletions, []byte(`{"model":"public-model","messages":[{"role":"user","content":"ping"}]}`), nil, "public-model", false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			sends, switches, _ := operation.snapshot()
			if terminal {
				if response.StatusCode != 429 || string(body) != quotaBodyFixture || calls.Load() != 1 || sends != 1 || switches != 0 {
					t.Fatalf("quota refusal failed over: status=%d body=%s calls=%d sends=%d switches=%d", response.StatusCode, body, calls.Load(), sends, switches)
				}
			} else if response.StatusCode != 200 || calls.Load() != 2 || sends != 2 || switches != 1 {
				t.Fatalf("ordinary 429 lost failover: status=%d calls=%d sends=%d switches=%d", response.StatusCode, calls.Load(), sends, switches)
			}
		})
	}
}
