package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestInspectOpenAIChatRequestFastStrictJSON(t *testing.T) {
	validFast := []string{
		`{"model":"model","messages":[{"role":"user","content":"hello"}]}`,
		` { "model" : "model", "messages" : [ { "role" : "user", "content" : [ { "type" : "text", "text" : "hello\\nworld" } ] } ], "stream": false, "temperature": -1.25e+2 } `,
		`{"model":"model","messages":[{"role":"user","content":"\u263a"}],"tools":[],"stream_options":null}`,
	}
	for _, body := range validFast {
		t.Run(body, func(t *testing.T) {
			if !json.Valid([]byte(body)) {
				t.Fatal("test fixture is not valid JSON")
			}
			strict, ok := inspectOpenAIChatRequestFast([]byte(body))
			if !ok {
				t.Fatal("strict inspection unexpectedly fell back")
			}
			validated, ok := inspectOpenAIChatRequestFastValidated([]byte(body))
			if !ok {
				t.Fatal("validated inspection unexpectedly fell back")
			}
			if strict.message != validated.message || strict.param != validated.param || strict.invalid != validated.invalid ||
				strict.mode != validated.mode || !bytes.Equal(strict.modelRaw, validated.modelRaw) {
				t.Fatalf("strict inspection = %+v, validated = %+v", strict, validated)
			}
		})
	}

	fallbacks := []string{
		`[]`,
		`{"\u006dodel":"model","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"model","messages":[{"role":"user","content":"hello"}],"stream":false,"stream":true}`,
		`{"model":"model","messages":[{"role":"user","content":"hello"}],"Stream":true}`,
		`{"model":"model","messages":` + strings.Repeat(`[`, maxFastRawJSONNestingDepth) + `0` + strings.Repeat(`]`, maxFastRawJSONNestingDepth) + `}`,
	}
	for _, body := range fallbacks {
		t.Run("fallback/"+body[:min(len(body), 48)], func(t *testing.T) {
			if !json.Valid([]byte(body)) {
				t.Fatal("fallback fixture is not valid JSON")
			}
			if _, ok := inspectOpenAIChatRequestFast([]byte(body)); ok {
				t.Fatal("strict inspection accepted a required fallback")
			}
		})
	}

	malformed := []string{
		``,
		`{"model":"model","messages":[}`,
		`{"model":"model" "messages":[]}`,
		`{"model":"model","messages":[],}`,
		`{"model":"model","messages":[1,]}`,
		`{"model":"model","messages":[],"bad":"\x"}`,
		`{"model":"model","messages":[],"bad":"\u12xz"}`,
		"{\"model\":\"model\",\"messages\":[],\"bad\":\"\n\"}",
		`{"model":"model","messages":[],"bad":01}`,
		`{"model":"model","messages":[],"bad":1.}`,
		`{"model":"model","messages":[],"bad":1e+}`,
		`{"model":"model","messages":[],"bad":truth}`,
		`{"model":"model","messages":[]} trailing`,
	}
	for _, body := range malformed {
		t.Run("malformed/"+body, func(t *testing.T) {
			if json.Valid([]byte(body)) {
				t.Fatal("malformed fixture is valid JSON")
			}
			if _, ok := inspectOpenAIChatRequestFast([]byte(body)); ok {
				t.Fatal("strict inspection accepted invalid JSON")
			}
		})
	}
}

func TestRawJSONStringScannersRejectInvalidUTF8(t *testing.T) {
	rawString := []byte{'"', 0xff, '"'}
	if _, _, _, _, ok := scanRawJSONString(rawString, 0); ok {
		t.Fatal("non-strict string scanner accepted invalid UTF-8")
	}
	if _, _, _, _, ok := scanStrictRawJSONString(rawString, 0); ok {
		t.Fatal("strict string scanner accepted invalid UTF-8")
	}

	body := append([]byte(`{"model":"`), 0xff)
	body = append(body, []byte(`","messages":[]}`)...)
	if !json.Valid(body) {
		t.Fatal("encoding/json compatibility fixture is not valid JSON")
	}
	if _, ok := inspectOpenAIChatRequestFast(body); ok {
		t.Fatal("fast request inspection accepted invalid UTF-8 instead of falling back")
	}
	if got, want := extractOpenAIChatCompletionsRequestModel(body), "\ufffd"; got != want {
		t.Fatalf("decoded model = %q, want replacement-rune model %q", got, want)
	}
}

func FuzzInspectOpenAIChatRequestFastStrictJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"model":"model","messages":[{"role":"user","content":"hello"}],"stream":false}`),
		[]byte(`{"model":"model","messages":[{"role":"user","content":[{"type":"text","text":"hello\\nworld"}]}],"tools":[]}`),
		[]byte(`{"model":"model","messages":[],"temperature":-1.25e+2,"metadata":{"nested":[true,false,null]}}`),
		[]byte(`{"model":"model","messages":[}`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		strict, ok := inspectOpenAIChatRequestFast(body)
		if !ok {
			return
		}
		if !json.Valid(body) {
			t.Fatalf("strict inspection accepted invalid JSON: %q", body)
		}
		validated, validatedOK := inspectOpenAIChatRequestFastValidated(body)
		if !validatedOK {
			t.Fatalf("strict inspection accepted input rejected by validated scanner: %q", body)
		}
		if strict.message != validated.message || strict.param != validated.param || strict.invalid != validated.invalid ||
			strict.mode != validated.mode || !bytes.Equal(strict.modelRaw, validated.modelRaw) {
			t.Fatalf("strict inspection = %+v, validated = %+v for %q", strict, validated, body)
		}
	})
}

func TestInspectCanonicalOpenAIChatCompletionResponseFast(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		requestedModel string
		wantOK         bool
		wantPrompt     int
		wantCompletion int
		wantTotal      int
	}{
		{
			name:           "canonical",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			wantOK:         true,
			wantPrompt:     2,
			wantCompletion: 3,
			wantTotal:      5,
		},
		{
			name:           "canonical whitespace and unknown fields",
			requestedModel: "public-model",
			body: ` {
				"id": "chat-1", "object": "chat.completion", "created": 1,
				"model": "upstream-model", "vendor": {"nested": [1, 2, 3]},
				"choices": [{"index": 0, "message": {"role": "assistant", "content": ["ok"]}, "finish_reason": "stop", "logprobs": null}],
				"usage": {"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5, "vendor": true}
			} `,
			wantOK:         true,
			wantPrompt:     2,
			wantCompletion: 3,
			wantTotal:      5,
		},
		{
			name:           "empty choices are canonical",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
			wantOK:         true,
		},
		{
			name:   "missing model allowed without requested model",
			body:   `{"id":"chat-1","object":"chat.completion","created":1,"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
			wantOK: true,
		},
		{
			name:           "missing id falls back",
			requestedModel: "public-model",
			body:           `{"object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "null created falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":null,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "zero created falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":0,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "zero total with nonzero components falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":0}}`,
		},
		{
			name:           "missing message role falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "null message content falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "provider tool shape falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"tool_calls","tool_calls":[]}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "usage details fall back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":1}}}`,
		},
		{
			name:           "escaped key falls back",
			requestedModel: "public-model",
			body:           `{"\u0069d":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "escaped nested key falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"\u0072ole":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "earlier invalid duplicate falls back",
			requestedModel: "public-model",
			body:           `{"id":null,"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		},
		{
			name:           "malformed unknown value falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"vendor":[1,]}`,
		},
		{
			name:           "invalid JSON falls back",
			requestedModel: "public-model",
			body:           `{"id":"chat-1"`,
		},
	}
	tests = append(tests, struct {
		name           string
		body           string
		requestedModel string
		wantOK         bool
		wantPrompt     int
		wantCompletion int
		wantTotal      int
	}{
		name:           "deep unknown value falls back",
		requestedModel: "public-model",
		body: `{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"vendor":` +
			strings.Repeat(`[`, maxFastRawJSONNestingDepth) + `0` + strings.Repeat(`]`, maxFastRawJSONNestingDepth) + `}`,
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, ok := inspectCanonicalOpenAIChatCompletionResponseFast([]byte(tt.body), tt.requestedModel)
			if ok != tt.wantOK {
				t.Fatalf("ok = %t, want %t", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !json.Valid([]byte(tt.body)) {
				t.Fatal("strict response inspection accepted invalid JSON")
			}
			validatedUsage, validatedOK := inspectCanonicalOpenAIChatCompletionResponseFastValidated([]byte(tt.body), tt.requestedModel)
			if !validatedOK || validatedUsage != usage {
				t.Fatalf("strict usage = %+v, validated usage = %+v, ok = %t", usage, validatedUsage, validatedOK)
			}
			if usage.PromptTokens != tt.wantPrompt || usage.CompletionTokens != tt.wantCompletion || usage.TotalTokens != tt.wantTotal {
				t.Fatalf("usage = %+v, want prompt=%d completion=%d total=%d", usage, tt.wantPrompt, tt.wantCompletion, tt.wantTotal)
			}
		})
	}
}

func FuzzInspectCanonicalOpenAIChatCompletionResponseFastStrictJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`),
		[]byte(` {"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"vendor":{"nested":[true,false,null]}} `),
		[]byte(`{"id":"chat-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"vendor":[1,]}`),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		usage, ok := inspectCanonicalOpenAIChatCompletionResponseFast(body, "public-model")
		if !ok {
			return
		}
		if !json.Valid(body) {
			t.Fatalf("strict response inspection accepted invalid JSON: %q", body)
		}
		validatedUsage, validatedOK := inspectCanonicalOpenAIChatCompletionResponseFastValidated(body, "public-model")
		if !validatedOK || validatedUsage != usage {
			t.Fatalf("strict usage = %+v, validated usage = %+v, ok = %t for %q", usage, validatedUsage, validatedOK, body)
		}
	})
}
