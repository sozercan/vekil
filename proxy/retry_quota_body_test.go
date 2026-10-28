package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
