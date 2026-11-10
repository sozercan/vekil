package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func toolImageContractJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(body) {
		t.Fatalf("invalid fixture JSON: %s", body)
	}
	return body
}

func toolImageContractRequest(t *testing.T, messages []any) *models.AnthropicRequest {
	t.Helper()
	var request models.AnthropicRequest
	body := toolImageContractJSON(t, map[string]any{"model": "chat-model", "max_tokens": 64, "messages": messages})
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode Anthropic fixture: %v", err)
	}
	return &request
}

func toolImageContractAssistant(ids ...string) map[string]any {
	blocks := []any{map[string]any{"type": "text", "text": "checking"}}
	for index, id := range ids {
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": id, "name": fmt.Sprintf("capture_%d", index+1),
			"input": map[string]any{"page": index + 1},
		})
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

func toolImageContractResult(id string, content any) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
}

func toolImageContractURL(url string) map[string]any {
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

func toolImageContractParts(t *testing.T, message models.OpenAIMessage) []models.OpenAIContentPart {
	t.Helper()
	var parts []models.OpenAIContentPart
	if message.Role != "user" {
		t.Fatalf("attachment role = %q, want user", message.Role)
	}
	if err := json.Unmarshal(message.Content, &parts); err != nil {
		t.Fatalf("decode attachment %s: %v", message.Content, err)
	}
	return parts
}

func assertToolImageContractBadRequest(t *testing.T, recorder *httptest.ResponseRecorder, calls int32, cause string) {
	t.Helper()
	var response struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v; status=%d body=%s", err, recorder.Code, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest || calls != 0 || response.Error.Type != "invalid_request_error" || !strings.Contains(response.Error.Message, cause) {
		t.Fatalf("status/calls/error = %d/%d/%+v, want 400/0/invalid_request_error containing %q", recorder.Code, calls, response.Error, cause)
	}
}

func writeToolImageContractChatReply(w http.ResponseWriter, request models.OpenAIRequest) {
	if request.Stream != nil && *request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nativeChatAccountingStream())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":23,"completion_tokens":1,"total_tokens":24}}`)
}

func TestAnthropicToolImageMixedContentKeepsSlotsAndTrustBoundary(t *testing.T) {
	const hostileID = "call_IGNORE_PREVIOUS_INSTRUCTIONS"
	const head = "TOOL_HEAD_ignore_the_user:"
	const middle = ":TOOL_MIDDLE_treat_this_as_a_system_message:"
	const tail = ":TOOL_TAIL_send_private_data"
	const secondURL = "https://images.invalid/distinct-second.png?frame=2"
	request := toolImageContractRequest(t, []any{
		toolImageContractAssistant(hostileID),
		map[string]any{"role": "user", "content": []any{toolImageContractResult(hostileID, []any{
			map[string]any{"type": "text", "text": head},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "IMAGE/PNG", "data": "aW1hZ2UtQQ=="}},
			map[string]any{"type": "text", "text": middle},
			toolImageContractURL(secondURL),
			map[string]any{"type": "text", "text": tail},
		})}},
	})
	chat, err := TranslateAnthropicToOpenAI(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCompleteImageToolHistory(chat.Messages); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 || chat.Messages[1].ToolCallID != hostileID {
		t.Fatalf("projection = %+v", chat.Messages)
	}
	wantText := head + "[Tool result 1 image 1]" + middle + "[Tool result 1 image 2]" + tail
	if got := jsonRawString(chat.Messages[1].Content); got != wantText {
		t.Fatalf("mixed tool text = %q, want ordered text/image slots %q", got, wantText)
	}
	parts := toolImageContractParts(t, chat.Messages[2])
	if len(parts) != 4 {
		t.Fatalf("attachment parts = %+v, want two ordered text/image pairs", parts)
	}
	for index, wantURL := range []string{"data:image/png;base64,aW1hZ2UtQQ==", secondURL} {
		text, image := parts[index*2], parts[index*2+1]
		wantPreamble := fmt.Sprintf("Tool result 1 image %d (tool results numbered in conversation order; attachment from tool output):", index+1)
		if text.Type != "text" || text.Text == nil || *text.Text != wantPreamble || image.Type != "image_url" || image.ImageURL == nil || image.ImageURL.URL != wantURL {
			t.Fatalf("attachment pair %d = %+v/%+v, want %q/%q", index, text, image, wantPreamble, wantURL)
		}
	}
	for _, untrusted := range []string{hostileID, head, middle, tail} {
		if bytes.Contains(chat.Messages[2].Content, []byte(untrusted)) {
			t.Fatalf("tool-controlled text or ID entered user attachment: %s", chat.Messages[2].Content)
		}
	}

	for _, content := range []any{
		"unchanged [Tool result 99 image 3] arbitrary tool text",
		[]any{map[string]any{"type": "text", "text": "unchanged "}, map[string]any{"type": "text", "text": "[Tool result 99 image 3] arbitrary tool text"}},
	} {
		t.Run(fmt.Sprintf("text only %T", content), func(t *testing.T) {
			request := toolImageContractRequest(t, []any{
				toolImageContractAssistant(hostileID),
				map[string]any{"role": "user", "content": []any{toolImageContractResult(hostileID, content)}},
			})
			chat, err := TranslateAnthropicToOpenAI(request)
			if err != nil {
				t.Fatal(err)
			}
			if len(chat.Messages) != 2 || chat.Messages[1].Role != "tool" || jsonRawString(chat.Messages[1].Content) != "unchanged [Tool result 99 image 3] arbitrary tool text" {
				t.Fatalf("text-only result acquired an attachment or changed text: %+v", chat.Messages)
			}
		})
	}
}

func TestAnthropicToolImageSourcesPreserveOriginalThroughResponses(t *testing.T) {
	const escapedLocation = "://images.invalid/frames%2Fone%2ftwo.png?signature=A%2FB%2fc&space=one+two&token=%252f"
	const preamble = "Tool result 1 image 1 (tool results numbered in conversation order; attachment from tool output):"
	for _, test := range []struct {
		name   string
		url    string
		base64 bool
	}{
		{name: "http", url: "http" + escapedLocation},
		{name: "hTtP", url: "hTtP" + escapedLocation},
		{name: "HTTP", url: "HTTP" + escapedLocation},
		{name: "https", url: "https" + escapedLocation},
		{name: "HtTpS", url: "HtTpS" + escapedLocation},
		{name: "HTTPS", url: "HTTPS" + escapedLocation},
		{name: "base64 source", url: "data:image/png;base64,QQ==", base64: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			image := toolImageContractURL(test.url)
			if test.base64 {
				image = map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "IMAGE/PNG", "data": "QQ=="}}
			}
			request := toolImageContractRequest(t, []any{
				toolImageContractAssistant("call_a"),
				map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", []any{
					map[string]any{"type": "text", "text": "before"}, image, map[string]any{"type": "text", "text": "after"},
				})}},
			})
			original := toolImageContractJSON(t, request)
			chat, err := TranslateAnthropicToOpenAI(request)
			if err != nil {
				t.Fatalf("valid image source rejected: %v", err)
			}
			if !bytes.Equal(toolImageContractJSON(t, request), original) {
				t.Fatal("image translation mutated the Anthropic source")
			}
			if err := validateCompleteImageToolHistory(chat.Messages); err != nil {
				t.Fatal(err)
			}
			if len(chat.Messages) != 3 || chat.Messages[1].Role != "tool" || chat.Messages[1].ToolCallID != "call_a" || jsonRawString(chat.Messages[1].Content) != "before[Tool result 1 image 1]after" {
				t.Fatalf("tool/image association changed: %+v", chat.Messages)
			}
			parts := toolImageContractParts(t, chat.Messages[2])
			if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != preamble || parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != test.url {
				t.Fatalf("Chat attachment = %+v, want fixed provenance and original URL %q", parts, test.url)
			}
			plan, err := translateChatRequestToResponses(toolImageContractJSON(t, chat), responsesChatRequestOptions{})
			if err != nil {
				t.Fatalf("valid canonical image rejected by Responses conversion: %v", err)
			}
			var input []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(upstreamInputJSON(t, plan)), &input); err != nil {
				t.Fatal(err)
			}
			var images []string
			for _, item := range input {
				if jsonRawString(item["role"]) != "user" {
					continue
				}
				var parts []map[string]json.RawMessage
				if err := json.Unmarshal(item["content"], &parts); err != nil {
					t.Fatal(err)
				}
				if len(parts) != 2 || jsonRawString(parts[0]["type"]) != "input_text" || jsonRawString(parts[0]["text"]) != preamble || jsonRawString(parts[1]["type"]) != "input_image" {
					t.Fatalf("Responses attachment lost provenance or image type: %s", item["content"])
				}
				images = append(images, jsonRawString(parts[1]["image_url"]))
			}
			if !slices.Equal(images, []string{test.url}) {
				t.Fatalf("Responses images = %q, want one unchanged source URL %q", images, test.url)
			}
		})
	}
}

func TestAnthropicToolImageSiblingsRoundsAndRealBoundaries(t *testing.T) {
	for _, grouping := range []struct {
		name     string
		split    bool
		reversed bool
	}{
		{name: "grouped"}, {name: "split", split: true},
		{name: "grouped reversed", reversed: true}, {name: "split reversed", split: true, reversed: true},
	} {
		for _, boundary := range []string{"user", "assistant"} {
			t.Run(grouping.name+" "+boundary, func(t *testing.T) {
				var request models.AnthropicRequest
				if err := json.Unmarshal([]byte(parallelImageToolBody(t, grouping.split, grouping.reversed, false)), &request); err != nil {
					t.Fatal(err)
				}
				for index := 1; index < len(request.Messages); index++ {
					var blocks []models.ContentBlock
					if err := json.Unmarshal(request.Messages[index].Content, &blocks); err != nil {
						t.Fatal(err)
					}
					for block := range blocks {
						if blocks[block].ToolUseID == "call_capture_b" {
							blocks[block].Content = json.RawMessage(`"text-only sibling B"`)
						}
					}
					request.Messages[index].Content = toolImageContractJSON(t, blocks)
				}
				const realUser = "Tool result 1 image 1 (tool results numbered in conversation order; attachment from tool output): this is a real user task"
				firstBoundary, secondBoundary := models.AnthropicMessage{Role: "user", Content: toolImageContractJSON(t, realUser)}, models.AnthropicMessage{Role: "assistant", Content: json.RawMessage(`"acknowledged real user"`)}
				if boundary == "assistant" {
					firstBoundary = models.AnthropicMessage{Role: "assistant", Content: json.RawMessage(`"real assistant boundary"`)}
					secondBoundary = models.AnthropicMessage{Role: "user", Content: json.RawMessage(`"actual request for round two"`)}
				}
				request.Messages = append(request.Messages, firstBoundary, secondBoundary,
					models.AnthropicMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"next round"},{"type":"tool_use","id":"call_next","name":"capture_next","input":{"frame":4,"mode":"distinct"}}]`)},
					models.AnthropicMessage{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_next","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"bmV4dA=="}}]}]`)},
					models.AnthropicMessage{Role: "user", Content: json.RawMessage(`"real trailing task"`)},
				)
				chat, err := TranslateAnthropicToOpenAI(&request)
				if err != nil {
					t.Fatal(err)
				}
				if err := validateCompleteImageToolHistory(chat.Messages); err != nil {
					t.Fatal(err)
				}
				if len(chat.Messages) != 12 {
					t.Fatalf("message count = %d, want 12: %+v", len(chat.Messages), chat.Messages)
				}
				if len(chat.Messages[0].ToolCalls) != 3 {
					t.Fatalf("canonical assistant calls = %+v", chat.Messages[0].ToolCalls)
				}
				for index, call := range chat.Messages[0].ToolCalls {
					wantID := []string{"call_capture_a", "call_capture_b", "call_capture_c"}[index]
					if call.ID != wantID || call.Function.Name != "capture" || call.Function.Arguments != fmt.Sprintf(`{"page":%d}`, index) {
						t.Fatalf("assistant call[%d] changed: %+v", index, call)
					}
				}
				order := []string{"call_capture_a", "call_capture_b", "call_capture_c"}
				if grouping.reversed {
					slices.Reverse(order)
				}
				attachment := 4
				for index, id := range order {
					tool := chat.Messages[index+1]
					if tool.Role != "tool" || tool.ToolCallID != id {
						t.Fatalf("result[%d] = %+v, want tool %q before every attachment", index, tool, id)
					}
					if id == "call_capture_b" {
						if jsonRawString(tool.Content) != "text-only sibling B" {
							t.Fatalf("text-only sibling changed: %s", tool.Content)
						}
						continue
					}
					parts := toolImageContractParts(t, chat.Messages[attachment])
					wantLabel := fmt.Sprintf("[Tool result %d image 1]", index+1)
					if jsonRawString(tool.Content) != wantLabel || len(parts) != 2 || parts[0].Text == nil || !strings.HasPrefix(*parts[0].Text, fmt.Sprintf("Tool result %d image 1 (", index+1)) || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://images.invalid/"+id+".png" {
						t.Fatalf("result/attachment association changed: tool=%+v attachment=%+v", tool, parts)
					}
					attachment++
				}
				if chat.Messages[6].Role != firstBoundary.Role || !bytes.Equal(chat.Messages[6].Content, firstBoundary.Content) || chat.Messages[7].Role != secondBoundary.Role || !bytes.Equal(chat.Messages[7].Content, secondBoundary.Content) {
					t.Fatalf("generated images crossed real %s boundary: %+v", boundary, chat.Messages[4:9])
				}
				secondAssistant := chat.Messages[8]
				if secondAssistant.Role != "assistant" || jsonRawString(secondAssistant.Content) != "next round" || len(secondAssistant.ToolCalls) != 1 || secondAssistant.ToolCalls[0].ID != "call_next" || secondAssistant.ToolCalls[0].Function.Name != "capture_next" || secondAssistant.ToolCalls[0].Function.Arguments != `{"frame":4,"mode":"distinct"}` {
					t.Fatalf("second assistant projection changed: %+v", secondAssistant)
				}
				parts := toolImageContractParts(t, chat.Messages[10])
				if chat.Messages[9].ToolCallID != "call_next" || jsonRawString(chat.Messages[9].Content) != "[Tool result 4 image 1]" || len(parts) != 2 || parts[0].Text == nil || !strings.HasPrefix(*parts[0].Text, "Tool result 4 image 1 (") || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,bmV4dA==" || jsonRawString(chat.Messages[11].Content) != "real trailing task" {
					t.Fatalf("second round or trailing user changed: %+v", chat.Messages[9:])
				}
			})
		}
	}
}

func TestAnthropicToolImageCacheBoundariesIncludeDeferredAttachments(t *testing.T) {
	cache := json.RawMessage(`{"type":"ephemeral","ttl":"1h"}`)
	image := func(id string) map[string]any {
		return toolImageContractResult(id, []any{toolImageContractURL("https://images.invalid/" + id + ".png")})
	}
	outer := func(result map[string]any) map[string]any { result["cache_control"] = cache; return result }
	lastNested := toolImageContractResult("call_a", []any{
		map[string]any{"type": "text", "text": "before"},
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://images.invalid/call_a.png"}, "cache_control": cache},
	})
	lastNestedText := toolImageContractResult("call_a", []any{
		toolImageContractURL("https://images.invalid/call_a.png"),
		map[string]any{"type": "text", "text": "after", "cache_control": cache},
	})
	for _, test := range []struct {
		name       string
		results    []map[string]any
		split      bool
		wantRoles  []string
		cacheIndex int
		wantURLs   []string
	}{
		{name: "single outer", results: []map[string]any{outer(image("call_a"))}, wantRoles: []string{"assistant", "tool", "user"}, cacheIndex: 2, wantURLs: []string{"https://images.invalid/call_a.png"}},
		{name: "last nested image", results: []map[string]any{lastNested}, wantRoles: []string{"assistant", "tool", "user"}, cacheIndex: 2, wantURLs: []string{"https://images.invalid/call_a.png"}},
		{name: "last nested text after image", results: []map[string]any{lastNestedText}, wantRoles: []string{"assistant", "tool", "user"}, cacheIndex: 2, wantURLs: []string{"https://images.invalid/call_a.png"}},
		{name: "final text sibling grouped", results: []map[string]any{image("call_a"), outer(toolImageContractResult("call_b", "plain B"))}, wantRoles: []string{"assistant", "tool", "tool", "user"}, cacheIndex: 3, wantURLs: []string{"https://images.invalid/call_a.png"}},
		{name: "final text sibling split", results: []map[string]any{image("call_a"), outer(toolImageContractResult("call_b", "plain B"))}, split: true, wantRoles: []string{"assistant", "tool", "tool", "user"}, cacheIndex: 3, wantURLs: []string{"https://images.invalid/call_a.png"}},
		{name: "final image sibling grouped", results: []map[string]any{image("call_a"), outer(image("call_b"))}, wantRoles: []string{"assistant", "tool", "tool", "user", "user"}, cacheIndex: 4, wantURLs: []string{"https://images.invalid/call_a.png", "https://images.invalid/call_b.png"}},
		{name: "final image sibling split", results: []map[string]any{image("call_a"), outer(image("call_b"))}, split: true, wantRoles: []string{"assistant", "tool", "tool", "user", "user"}, cacheIndex: 4, wantURLs: []string{"https://images.invalid/call_a.png", "https://images.invalid/call_b.png"}},
	} {
		for _, count := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: " messages", true: " count"}[count], func(t *testing.T) {
				ids := make([]string, len(test.results))
				for index, result := range test.results {
					ids[index] = result["tool_use_id"].(string)
				}
				messages := []any{toolImageContractAssistant(ids...)}
				if test.split {
					for _, result := range test.results {
						messages = append(messages, map[string]any{"role": "user", "content": []any{result}})
					}
				} else {
					messages = append(messages, map[string]any{"role": "user", "content": test.results})
				}
				body := toolImageContractJSON(t, toolImageContractRequest(t, messages))
				seen := make(chan models.OpenAIRequest, 1)
				var calls atomic.Int32
				h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request models.OpenAIRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if err := validateCompleteImageToolHistory(request.Messages); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					seen <- request
					writeToolImageContractChatReply(w, request)
				})
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, bytes.NewReader(body))
				if count {
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				if recorder.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("status/calls = %d/%d: %s", recorder.Code, calls.Load(), recorder.Body.String())
				}
				var got models.OpenAIRequest
				select {
				case got = <-seen:
				default:
					t.Fatal("request was not forwarded")
				}
				if len(got.Messages) != len(test.wantRoles) {
					t.Fatalf("message count = %d, want %d: %+v", len(got.Messages), len(test.wantRoles), got.Messages)
				}
				var urls []string
				for index, message := range got.Messages {
					if message.Role != test.wantRoles[index] {
						t.Fatalf("role[%d] = %q, want %q", index, message.Role, test.wantRoles[index])
					}
					cached := !rawJSONIsNullOrEmpty(message.CopilotCacheControl)
					if cached != (index == test.cacheIndex) {
						t.Fatalf("cache boundary[%d] = %s, want only message %d", index, message.CopilotCacheControl, test.cacheIndex)
					}
					if cached && !bytes.Equal(message.CopilotCacheControl, cache) {
						t.Fatalf("cache metadata = %s, want %s", message.CopilotCacheControl, cache)
					}
					if message.Role == "user" {
						for _, part := range toolImageContractParts(t, message) {
							if part.ImageURL != nil {
								urls = append(urls, part.ImageURL.URL)
							}
						}
					}
				}
				if !slices.Equal(urls, test.wantURLs) {
					t.Fatalf("cached image prefix = %v, want %v", urls, test.wantURLs)
				}
				if count && !strings.Contains(recorder.Body.String(), `"input_tokens":23`) {
					t.Fatalf("count response = %s", recorder.Body.String())
				}
			})
		}
	}
}

func TestAnthropicToolImageCacheRejectsCrossedAndConflictingBoundaries(t *testing.T) {
	cache := json.RawMessage(`{"type":"ephemeral"}`)
	image := func(id string, hint bool) map[string]any {
		result := toolImageContractResult(id, []any{toolImageContractURL("https://images.invalid/" + id + ".png")})
		if hint {
			result["cache_control"] = cache
		}
		return result
	}
	for _, test := range []struct {
		name     string
		messages []any
		cause    string
	}{
		{name: "earlier image before later image grouped", messages: []any{toolImageContractAssistant("call_a", "call_b"), map[string]any{"role": "user", "content": []any{image("call_a", true), image("call_b", false)}}}, cause: "cache_control cannot cross reordered tool-call or result content"},
		{name: "earlier image before later image split", messages: []any{toolImageContractAssistant("call_a", "call_b"), map[string]any{"role": "user", "content": []any{image("call_a", true)}}, map[string]any{"role": "user", "content": []any{image("call_b", false)}}}, cause: "cache_control cannot cross reordered tool-call or result content"},
		{name: "earlier image before later text split", messages: []any{toolImageContractAssistant("call_a", "call_b"), map[string]any{"role": "user", "content": []any{image("call_a", true)}}, map[string]any{"role": "user", "content": []any{toolImageContractResult("call_b", "plain B")}}}, cause: "cache_control cannot cross reordered tool-call or result content"},
		{name: "nonfinal nested text hint", messages: []any{toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", []any{map[string]any{"type": "text", "text": "first", "cache_control": cache}, toolImageContractURL("https://images.invalid/a.png")})}}}, cause: "cache_control cannot split a tool result"},
		{name: "nonfinal nested image hint", messages: []any{toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", []any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://images.invalid/a.png"}, "cache_control": cache}, map[string]any{"type": "text", "text": "last"}})}}}, cause: "cache_control cannot split a tool result"},
		{name: "conflicting outer and nested hints", messages: []any{toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_a", "cache_control": cache, "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://images.invalid/a.png"}, "cache_control": json.RawMessage(`{"type":"ephemeral","ttl":"1h"}`)}}}}}}, cause: "cache_control cannot split a tool result"},
		{name: "real user before cached result", messages: []any{toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "real primary content"}, image("call_a", true)}}}, cause: "cache_control cannot cross reordered tool-call or result content"},
		{name: "cached real user before result", messages: []any{toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "real primary content", "cache_control": cache}, image("call_a", false)}}}, cause: "cache_control cannot cross reordered tool-call or result content"},
	} {
		for _, count := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: " messages", true: " count"}[count], func(t *testing.T) {
				body := toolImageContractJSON(t, toolImageContractRequest(t, test.messages))
				var calls atomic.Int32
				h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				})
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, bytes.NewReader(body))
				if count {
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				assertToolImageContractBadRequest(t, recorder, calls.Load(), test.cause)
			})
		}
	}
}

// Identity whitespace the translation forwards verbatim, and equal hints written with
// different spacing, are not changes the cache mapping may refuse. Raw request bytes keep
// the spacing that json.Marshal would compact away.
func TestAnthropicChatCacheMappingToleratesVerbatimWhitespace(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "padded tool ID under a system hint",
			body: `{"model":"chat-model","max_tokens":64,
				"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],
				"messages":[
				{"role":"user","content":"look"},
				{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1 ","name":"lookup","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1 ","content":"found"}]}]}`,
		},
		{
			name: "equal hints with different spacing on one boundary",
			body: `{"model":"chat-model","max_tokens":64,"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"lookup","input":{}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"call_a","content":"found","cache_control":{"type":"ephemeral"}},
					{"type":"text","text":"","cache_control":{"type": "ephemeral"}}]}]}`,
		},
		{
			name: "equal tool result and nested hints with different spacing",
			body: `{"model":"chat-model","max_tokens":64,"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"lookup","input":{}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"call_a","cache_control":{"type":"ephemeral"},
						"content":[{"type":"text","text":"found","cache_control":{"type": "ephemeral"}}]}]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var request models.AnthropicRequest
			if err := json.Unmarshal([]byte(test.body), &request); err != nil {
				t.Fatal(err)
			}
			canonical, err := TranslateAnthropicToOpenAI(&request)
			if err != nil {
				t.Fatal(err)
			}
			ctx := withAnthropicChatCacheControl(context.Background(), &request)
			got, err := applyAnthropicChatCacheControl(ctx, &providerRuntime{kind: providerTypeCopilot}, providerEndpointChatCompletions, toolImageContractJSON(t, canonical))
			if err != nil {
				t.Fatalf("applyAnthropicChatCacheControl() error = %v", err)
			}
			if !bytes.Contains(got, []byte(`"copilot_cache_control"`)) {
				t.Fatalf("mapped body lost the cache hint: %s", got)
			}
		})
	}
}

func TestAnthropicToolImageCacheKeepsOptimizedBytesAndRejectsMediaDrift(t *testing.T) {
	last := toolImageContractResult("call_b", []any{toolImageContractURL("https://images.invalid/b.png")})
	last["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
	request := toolImageContractRequest(t, []any{
		toolImageContractAssistant("call_a", "call_b"),
		map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", []any{toolImageContractURL("https://images.invalid/a.png")})}},
		map[string]any{"role": "user", "content": []any{last}},
	})
	canonical, err := TranslateAnthropicToOpenAI(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.Messages) != 5 {
		t.Fatalf("projection = %+v", canonical.Messages)
	}
	ctx := withAnthropicChatCacheControl(context.Background(), request)
	canonicalBody := toolImageContractJSON(t, canonical)
	for index := 1; index <= 2; index++ {
		canonical.Messages[index].Content = json.RawMessage(fmt.Sprintf(`"optimized\u0020tool\n%d"`, index))
	}
	optimizedBody := toolImageContractJSON(t, canonical)
	got, err := applyAnthropicChatCacheControl(ctx, &providerRuntime{kind: providerTypeCopilot}, providerEndpointChatCompletions, optimizedBody)
	if err != nil {
		t.Fatal(err)
	}
	var output models.OpenAIRequest
	if err := json.Unmarshal(got, &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Messages) != 5 {
		t.Fatalf("cache mapping changed message count: %s", got)
	}
	for index := 1; index <= 2; index++ {
		if !bytes.Equal(output.Messages[index].Content, canonical.Messages[index].Content) || output.Messages[index].ToolCallID != canonical.Messages[index].ToolCallID {
			t.Fatalf("optimizer bytes or ID rebuilt: got=%s want=%s", output.Messages[index].Content, canonical.Messages[index].Content)
		}
		parts := toolImageContractParts(t, output.Messages[index+2])
		if len(parts) != 2 || parts[0].Text == nil || !strings.HasPrefix(*parts[0].Text, fmt.Sprintf("Tool result %d image 1 (", index)) || !bytes.Equal(output.Messages[index+2].Content, canonical.Messages[index+2].Content) {
			t.Fatalf("numeric association changed after tool slots were optimized away: %+v", parts)
		}
	}
	for index, message := range output.Messages {
		if cached := !rawJSONIsNullOrEmpty(message.CopilotCacheControl); cached != (index == 4) {
			t.Fatalf("cache boundary[%d] = %s, want only final attachment", index, message.CopilotCacheControl)
		}
	}

	for _, test := range []struct {
		name  string
		index int
	}{
		{name: "cached attachment", index: 4},
		{name: "earlier attachment inside cached prefix", index: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			var changed models.OpenAIRequest
			if err := json.Unmarshal(canonicalBody, &changed); err != nil {
				t.Fatal(err)
			}
			parts := toolImageContractParts(t, changed.Messages[test.index])
			parts[1].ImageURL.URL = "https://images.invalid/not-the-original-image.png"
			changed.Messages[test.index].Content = toolImageContractJSON(t, parts)
			_, err := applyAnthropicChatCacheControl(ctx, &providerRuntime{kind: providerTypeCopilot}, providerEndpointChatCompletions, toolImageContractJSON(t, changed))
			if err == nil || !strings.Contains(err.Error(), "non-tool content changes") {
				t.Fatalf("media identity drift error = %v, want non-tool content refusal", err)
			}
		})
	}
}

func TestAnthropicToolImageCacheAndOriginsStayOutOfCanonicalResponses(t *testing.T) {
	result := toolImageContractResult("call_a", []any{toolImageContractURL("https://images.invalid/a.png")})
	result["cache_control"] = json.RawMessage(`{"type":"ephemeral"}`)
	request := toolImageContractRequest(t, []any{toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{result}}})
	canonical, err := TranslateAnthropicToOpenAI(request)
	if err != nil {
		t.Fatal(err)
	}
	body := toolImageContractJSON(t, canonical)
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
	if err != nil {
		t.Fatalf("image canonical history did not satisfy strict Responses contract: %v", err)
	}
	for _, encoded := range [][]byte{body, plan.Body} {
		for _, field := range []string{`"cache_control":`, `"copilot_cache_control":`, `"origins":`, `"first":`, `"last":`} {
			if bytes.Contains(encoded, []byte(field)) {
				t.Fatalf("internal cache/projection field %s leaked: %s", field, encoded)
			}
		}
	}
	ctx := withAnthropicChatCacheControl(context.Background(), request)
	for _, route := range []struct {
		name     string
		provider *providerRuntime
		path     string
	}{
		{name: "no provider", path: providerEndpointChatCompletions},
		{name: "Copilot Responses", provider: &providerRuntime{kind: providerTypeCopilot}, path: providerEndpointResponses},
		{name: "generic Chat", provider: &providerRuntime{kind: providerTypeOpenAICompatible}, path: providerEndpointChatCompletions},
		{name: "generic Responses", provider: &providerRuntime{kind: providerTypeOpenAICompatible}, path: providerEndpointResponses},
	} {
		t.Run(route.name, func(t *testing.T) {
			got, err := applyAnthropicChatCacheControl(ctx, route.provider, route.path, body)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("non-Copilot-Chat route changed canonical bytes: error=%v body=%s", err, got)
			}
		})
	}
}

func TestAnthropicToolImageResponsesFullReversedAndPartialReplay(t *testing.T) {
	store, route, items, published := publishCarrierParityTurn(t, "upstream-image-a", "upstream-image-b")
	for _, mode := range []string{"normal", "stored", "carried"} {
		for _, results := range []struct {
			name  string
			order []int
		}{
			{name: "full", order: []int{0, 1}},
			{name: "reversed", order: []int{1, 0}},
			{name: "nonempty partial", order: []int{1}},
		} {
			for _, split := range []bool{false, true} {
				t.Run(mode+" "+results.name+map[bool]string{false: " grouped", true: " split"}[split], func(t *testing.T) {
					calls := slices.Clone(published.Projection.Calls)
					if mode == "normal" {
						calls = []responsesChatReplayProjectedCall{{ID: "call_external_a", Name: "capture_a", Arguments: `{"page":1}`}, {ID: "call_external_b", Name: "capture_b", Arguments: `{"page":2}`}}
					}
					blocks := []any{map[string]any{"type": "text", "text": "checking"}}
					for _, call := range calls {
						blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": json.RawMessage(call.Arguments)})
					}
					messages := []any{map[string]any{"role": "assistant", "content": blocks}}
					var grouped []any
					for _, index := range results.order {
						image := toolImageContractURL("https://images.invalid/replay-b.png")
						if index == 0 {
							image = map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QQ=="}}
						}
						result := toolImageContractResult(calls[index].ID, []any{map[string]any{"type": "text", "text": "answer-for-" + calls[index].ID}, image})
						if split {
							messages = append(messages, map[string]any{"role": "user", "content": []any{result}})
						} else {
							grouped = append(grouped, result)
						}
					}
					if !split {
						messages = append(messages, map[string]any{"role": "user", "content": grouped})
					}
					request := toolImageContractRequest(t, messages)
					request.Model = "gpt-public"
					canonical, err := TranslateAnthropicToOpenAI(request)
					if err != nil {
						t.Fatalf("Anthropic image lowering must preserve nonempty partial history: %v", err)
					}
					if len(canonical.Messages) != 1+2*len(results.order) || len(canonical.Messages[0].ToolCalls) != len(calls) || jsonRawString(canonical.Messages[0].Content) != "checking" {
						t.Fatalf("full canonical assistant projection was pruned: %+v", canonical.Messages)
					}
					for index, call := range canonical.Messages[0].ToolCalls {
						want := calls[index]
						if call.ID != want.ID || call.Type != "function" || call.Function.Name != want.Name || call.Function.Arguments != want.Arguments {
							t.Fatalf("canonical call[%d] = %+v, want %+v", index, call, want)
						}
					}
					if len(results.order) == len(calls) {
						if err := validateCompleteImageToolHistory(canonical.Messages); err != nil {
							t.Fatal(err)
						}
					} else if err := validateCompleteImageToolHistory(canonical.Messages); err == nil {
						t.Fatal("native Chat checker unexpectedly accepted an unanswered parallel call")
					}
					options := responsesChatRequestOptions{UpstreamModel: "gpt-upstream"}
					switch mode {
					case "stored":
						options.ReplayStore, options.ReplayRoute = store, route
					case "carried":
						options.ReplayRoute = route
						options.CarriedReasoning = carriedForEveryCall(t, route, published, items)
					}
					body := toolImageContractJSON(t, canonical)
					plan, err := translateChatRequestToResponses(body, options)
					if err != nil {
						t.Fatalf("Responses %s image replay: %v", mode, err)
					}
					var input []map[string]json.RawMessage
					if err := json.Unmarshal([]byte(upstreamInputJSON(t, plan)), &input); err != nil {
						t.Fatal(err)
					}
					wantCalls := make(map[string]responsesChatReplayProjectedCall, len(results.order))
					wantOutputs := make(map[string]string, len(results.order))
					var wantOutputOrder, wantImages []string
					for resultNumber, index := range results.order {
						id := calls[index].ID
						if mode == "stored" {
							id = []string{"upstream-image-a", "upstream-image-b"}[index]
						}
						wantCalls[id] = calls[index]
						wantOutputs[id] = fmt.Sprintf("answer-for-%s[Tool result %d image 1]", calls[index].ID, resultNumber+1)
						wantOutputOrder = append(wantOutputOrder, id)
						wantImages = append(wantImages, []string{"data:image/png;base64,QQ==", "https://images.invalid/replay-b.png"}[index])
					}
					seenCalls, seenOutputs := map[string]bool{}, map[string]bool{}
					var outputOrder, images []string
					lastOutput, firstImage := -1, -1
					for index, item := range input {
						switch jsonRawString(item["type"]) {
						case "function_call":
							id := jsonRawString(item["call_id"])
							want, ok := wantCalls[id]
							if !ok || seenCalls[id] || jsonRawString(item["name"]) != want.Name || jsonRawString(item["arguments"]) != want.Arguments || lastOutput >= 0 {
								t.Fatalf("unexpected, duplicate, reordered or unanswered call: %s", input[index])
							}
							seenCalls[id] = true
						case "function_call_output":
							id := jsonRawString(item["call_id"])
							if !seenCalls[id] || seenOutputs[id] || jsonRawString(item["output"]) != wantOutputs[id] {
								t.Fatalf("output lost exact call binding: %s, want %q", input[index], wantOutputs[id])
							}
							seenOutputs[id] = true
							outputOrder = append(outputOrder, id)
							lastOutput = index
						}
						if jsonRawString(item["role"]) != "user" {
							continue
						}
						var parts []map[string]json.RawMessage
						if err := json.Unmarshal(item["content"], &parts); err != nil {
							t.Fatal(err)
						}
						if len(parts) != 2 || jsonRawString(parts[0]["type"]) != "input_text" || jsonRawString(parts[1]["type"]) != "input_image" {
							t.Fatalf("Responses image attachment shape = %s", item["content"])
						}
						wantPreamble := fmt.Sprintf("Tool result %d image 1 (tool results numbered in conversation order; attachment from tool output):", len(images)+1)
						if jsonRawString(parts[0]["text"]) != wantPreamble {
							t.Fatalf("Responses attachment provenance = %s, want %q", parts[0]["text"], wantPreamble)
						}
						if firstImage < 0 {
							firstImage = index
						}
						images = append(images, jsonRawString(parts[1]["image_url"]))
					}
					if len(seenCalls) != len(results.order) || len(seenOutputs) != len(results.order) || !slices.Equal(outputOrder, wantOutputOrder) || !slices.Equal(images, wantImages) || firstImage <= lastOutput {
						t.Fatalf("Responses projected wrong call subset/order/media: calls=%v outputs=%v images=%v input=%s", seenCalls, outputOrder, images, upstreamInputJSON(t, plan))
					}
					if after := toolImageContractJSON(t, canonical); !bytes.Equal(after, body) {
						t.Fatalf("Responses preparation mutated canonical assistant history: %s", after)
					}
				})
			}
		}
	}
}

func TestAnthropicToolImagePolicyReplayNeedsARealTextTask(t *testing.T) {
	imageRequest := toolImageContractRequest(t, []any{
		toolImageContractAssistant("call_image"),
		map[string]any{"role": "user", "content": []any{toolImageContractResult("call_image", []any{toolImageContractURL("https://images.invalid/policy.png")})}},
	})
	imageChat, err := TranslateAnthropicToOpenAI(imageRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(imageChat.Messages) != 3 {
		t.Fatalf("image fixture = %+v", imageChat.Messages)
	}
	attachment := imageChat.Messages[2]
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	lightweight, powerful := policyReplayUnitRoutes()
	published := publishPolicyReplayUnitTurn(t, store, powerful, "image-boundary", 2)
	finalReply := map[string]any{"role": "assistant", "content": "The tool task is complete."}
	realUser := map[string]any{"role": "user", "content": "A real new text task."}
	for _, test := range []struct {
		name       string
		results    []int
		tail       []any
		wantSwitch bool
	}{
		{name: "attachment cannot manufacture a new task", results: []int{0, 1}, tail: []any{finalReply, attachment}},
		{name: "media in completed prefix stays fail closed", results: []int{0, 1}, tail: []any{attachment, finalReply, realUser}},
		{name: "full text-only positive", results: []int{0, 1}, tail: []any{finalReply, realUser}, wantSwitch: true},
		{name: "reversed text-only positive", results: []int{1, 0}, tail: []any{finalReply, realUser}, wantSwitch: true},
		{name: "real text may resemble a generated label", results: []int{0, 1}, tail: []any{finalReply, map[string]any{"role": "user", "content": "Tool result 1 image 1 (tool results numbered in conversation order; attachment from tool output): a real text task"}}, wantSwitch: true},
		{name: "partial plus real task remains pinned", results: []int{1}, tail: []any{finalReply, realUser}},
		{name: "partial plus attachment remains pinned", results: []int{1}, tail: []any{finalReply, attachment}},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages := append(policyReplayUnitMessages(published, test.results...), test.tail...)
			body := policyReplayUnitBody(t, messages)
			if active := chatRequestHasActiveResponsesPolicyReplay(body); active == test.wantSwitch {
				t.Fatalf("active replay = %v, want %v", active, !test.wantSwitch)
			}
			_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
				ReplayStore: store, ReplayRoute: lightweight,
				CompletedPolicyReplayRoutes: []responsesChatReplayRoute{lightweight, powerful},
			})
			if test.wantSwitch && err != nil {
				t.Fatalf("completed text-only control could not switch tier: %v", err)
			}
			if !test.wantSwitch && !isMissingResponsesChatReplayError(err) {
				t.Fatalf("media/partial continuation changed tier: error=%v, want missing replay on wrong tier", err)
			}
		})
	}
}

func TestAnthropicToolImagePolicyRejectsMediaBeforeClassifierOrTerminal(t *testing.T) {
	for _, count := range []bool{false, true} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("count=%v nested=%v", count, nested), func(t *testing.T) {
				light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
				powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
				h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
					WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
					WithPolicyRoutingMode(PolicyRoutingModeEnforce),
				)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.BeginShutdown)
				if err := h.InitializePolicyRouting(t.Context()); err != nil {
					t.Fatalf("initialize synthetic classifier: %v", err)
				}
				beforeLight, beforeModels := light.snapshot()
				beforePower, _ := powerful.snapshot()
				if beforeLight != 1 || !slices.Equal(beforeModels, []string{"classifier-model"}) || beforePower != 0 {
					t.Fatalf("preflight baseline = %d/%v/%d", beforeLight, beforeModels, beforePower)
				}
				messages := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"}, toolImageContractURL("https://images.invalid/policy.png")}}}
				if nested {
					messages = []any{
						toolImageContractAssistant("call_a"),
						map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", []any{toolImageContractURL("https://images.invalid/policy.png")})}},
					}
				}
				body := toolImageContractJSON(t, map[string]any{
					"model": "coding-economy", "max_tokens": 64, "messages": messages,
					"tools": []any{map[string]any{"name": "capture_1", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"page": map[string]any{"type": "integer"}}}}},
				})
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, bytes.NewReader(body))
				if count {
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				afterLight, afterModels := light.snapshot()
				afterPower, _ := powerful.snapshot()
				assertToolImageContractBadRequest(t, recorder, int32(afterLight-beforeLight+afterPower-beforePower), "text content parts")
				if !slices.Equal(afterModels, beforeModels) {
					t.Fatalf("image request reached classifier or terminal after preflight: %v", afterModels)
				}
			})
		}
	}
}

func TestValidBase64ImageDataMatchesStandardEncoding(t *testing.T) {
	for _, prefixBlocks := range []int{0, 254, 255, 256, 511} {
		prefix := strings.Repeat("AAAA", prefixBlocks)
		for _, test := range []struct {
			name   string
			suffix string
		}{
			{name: "complete quantum", suffix: "QUJD"},
			{name: "one padding byte", suffix: "QUI="},
			{name: "two padding bytes", suffix: "QQ=="},
			{name: "newlines in padding", suffix: "QQ=\r\n="},
			{name: "trailing newlines", suffix: "QQ==\r\n"},
			{name: "data after one padding byte", suffix: "QUI=Qg=="},
			{name: "data after two padding bytes", suffix: "QQ==Qg=="},
			{name: "unpadded data after padding", suffix: "QQ==QUJD"},
			{name: "data after padding and newlines", suffix: "QQ==\r\nQg=="},
			{name: "extra padding", suffix: "QQ==="},
			{name: "extra padding after newlines", suffix: "QQ==\r\n="},
			{name: "missing padding", suffix: "QQ="},
		} {
			t.Run(fmt.Sprintf("prefix=%d/%s", prefixBlocks, test.name), func(t *testing.T) {
				data := prefix + test.suffix
				_, err := base64.StdEncoding.DecodeString(data)
				want := err == nil
				if got := validBase64ImageData(data); got != want {
					t.Fatalf("validBase64ImageData = %v, want %v (DecodeString error: %v)", got, want, err)
				}
			})
		}
	}
}

func TestAnthropicToolImageMalformedContentFailsBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		content any
		cause   string
	}{
		{name: "audio", content: []any{map[string]any{"type": "audio"}}, cause: `unsupported tool_result content block type "audio"`},
		{name: "document", content: []any{map[string]any{"type": "document"}}, cause: `unsupported tool_result content block type "document"`},
		{name: "future block", content: []any{map[string]any{"type": "future_media"}}, cause: `unsupported tool_result content block type "future_media"`},
		{name: "missing image source", content: []any{map[string]any{"type": "image"}}, cause: "image content block is missing source"},
		{name: "bad base64", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "not-base64!"}}}, cause: "tool_result image requires an HTTP(S) URL or valid base64 image data"},
		{name: "base64 data after padded chunk", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": strings.Repeat("AAAA", 255) + "QQ==Qg=="}}}, cause: "tool_result image requires an HTTP(S) URL or valid base64 image data"},
		{name: "non HTTP URL", content: []any{toolImageContractURL("ftp://images.invalid/a.png")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "file URL", content: []any{toolImageContractURL("file:///tmp/a.png")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "relative URL", content: []any{toolImageContractURL("/frames/a.png?frame=1")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "scheme relative URL", content: []any{toolImageContractURL("//images.invalid/a.png")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "malformed URL", content: []any{toolImageContractURL("HtTpS://images.invalid/%zz")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "URL typed base64 data", content: []any{toolImageContractURL("data:image/png;base64,QQ==")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "URL without host", content: []any{toolImageContractURL("https:///missing-host.png")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "URL with only a port", content: []any{toolImageContractURL("https://:443/screen.png")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "URL with userinfo and no hostname", content: []any{toolImageContractURL("https://user@:80/screen.png")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "media type with trailing space", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png ", "data": "QQ=="}}}, cause: "tool_result base64 image source requires an image/<subtype> media_type"},
		{name: "media type with newline", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png\n", "data": "QQ=="}}}, cause: "tool_result base64 image source requires an image/<subtype> media_type"},
		{name: "media type with parameter", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png;charset=x", "data": "QQ=="}}}, cause: "tool_result base64 image source requires an image/<subtype> media_type"},
		{name: "media type without subtype", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/", "data": "QQ=="}}}, cause: "tool_result base64 image source requires an image/<subtype> media_type"},
		{name: "empty base64 data", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": ""}}}, cause: "base64 image source requires non-empty data"},
		{name: "base64 decodes to empty data", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "\r\n"}}}, cause: "base64 image source requires non-empty data"},
		{name: "empty data URL", content: []any{toolImageContractURL("data:image/png;base64,")}, cause: "tool_result url image source requires an HTTP(S) URL"},
		{name: "missing media type", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "QQ=="}}}, cause: "base64 image source requires media_type and data"},
		{name: "non image media type", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "text/plain", "data": "QQ=="}}}, cause: `unsupported image media_type "text/plain"`},
		{name: "unknown source type", content: []any{map[string]any{"type": "image", "source": map[string]any{"type": "file", "url": "https://images.invalid/a.png"}}}, cause: `unsupported image source type "file"`},
	} {
		for _, count := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: " messages", true: " count"}[count], func(t *testing.T) {
				body := toolImageContractJSON(t, toolImageContractRequest(t, []any{
					toolImageContractAssistant("call_a"),
					map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", test.content)}},
				}))
				var calls atomic.Int32
				h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				})
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, bytes.NewReader(body))
				if count {
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				assertToolImageContractBadRequest(t, recorder, calls.Load(), test.cause)
			})
		}
	}
	for _, count := range []bool{false, true} {
		t.Run(map[bool]string{false: "text-only messages control", true: "text-only count control"}[count], func(t *testing.T) {
			var calls atomic.Int32
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if err := validateCompleteImageToolHistory(request.Messages); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(request.Messages) != 2 || jsonRawString(request.Messages[1].Content) != "ordinary result" {
					t.Errorf("text-only projection = %+v", request.Messages)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				writeToolImageContractChatReply(w, request)
			})
			body := toolImageContractJSON(t, toolImageContractRequest(t, []any{
				toolImageContractAssistant("call_a"), map[string]any{"role": "user", "content": []any{toolImageContractResult("call_a", "ordinary result")}},
			}))
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, bytes.NewReader(body))
			if count {
				h.HandleAnthropicMessagesCountTokens(recorder, request)
			} else {
				h.HandleAnthropicMessages(recorder, request)
			}
			if recorder.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("text control status/calls = %d/%d: %s", recorder.Code, calls.Load(), recorder.Body.String())
			}
		})
	}
}

func TestAnthropicToolImageNativeMessagesAndCountPreserveBytesExceptCarrier(t *testing.T) {
	const bodyWithoutCarrier = `{"model":"native-image","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"native thought","signature":"native-signature"},{"type":"tool_use","id":"call_a","name":"capture","input":{"page":1}},{"type":"tool_use","id":"call_b","name":"capture","input":{"page":2}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"text","text":"kept \\u0041"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}],"cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"tool_result","tool_use_id":"call_b","content":[{"type":"image","source":{"type":"url","url":"https://images.invalid/native.png?raw=1"},"cache_control":{"type":"ephemeral"}}]}]}]}`
	const carrier = `{"type":"thinking","thinking":"","signature":"\u0076ekil1.OPAQUE"},`
	for _, count := range []bool{false, true} {
		for _, withCarrier := range []bool{false, true} {
			t.Run(fmt.Sprintf("count=%v carrier=%v", count, withCarrier), func(t *testing.T) {
				body := bodyWithoutCarrier
				if withCarrier {
					body = strings.Replace(body, `"content":[{"type":"thinking"`, `"content":[`+carrier+`{"type":"thinking"`, 1)
				}
				if !json.Valid([]byte(body)) {
					t.Fatalf("invalid native fixture JSON: %s", body)
				}
				seen := make(chan []byte, 1)
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet && r.URL.Path == providerEndpointModels {
						_, _ = io.WriteString(w, `{"data":[{"id":"native-image","supported_endpoints":["/v1/messages"]}]}`)
						return
					}
					calls.Add(1)
					wantPath := providerEndpointMessages
					if count {
						wantPath = providerEndpointMessagesCount
					}
					if r.Method != http.MethodPost || r.URL.Path != wantPath {
						t.Errorf("native method/path = %s %q, want POST %q", r.Method, r.URL.Path, wantPath)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					seen <- raw
					if count {
						_, _ = io.WriteString(w, `{"input_tokens":23}`)
					} else {
						_, _ = io.WriteString(w, `{"id":"native-response","type":"message","role":"assistant","model":"native-image","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":23,"output_tokens":1}}`)
					}
				}))
				defer upstream.Close()
				h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.BeginShutdown)
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body))
				if count {
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				if recorder.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("native status/calls = %d/%d: %s", recorder.Code, calls.Load(), recorder.Body.String())
				}
				select {
				case got := <-seen:
					if string(got) != bodyWithoutCarrier {
						t.Fatalf("native image/cache/signature bytes changed beyond carrier removal:\n got %s\nwant %s", got, bodyWithoutCarrier)
					}
				default:
					t.Fatal("native request was not forwarded")
				}
				if count && !strings.Contains(recorder.Body.String(), `"input_tokens":23`) {
					t.Fatalf("native count response = %s", recorder.Body.String())
				}
			})
		}
	}
}

func TestAnthropicToolImageResponsesHandlerExecution(t *testing.T) {
	streamFixture, err := os.ReadFile("testdata/chat_over_responses/stream_text.sse")
	if err != nil {
		t.Fatal(err)
	}
	jsonFixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, grouping := range []struct {
		name     string
		split    bool
		reversed bool
	}{{name: "grouped full"}, {name: "split reversed", split: true, reversed: true}} {
		for _, mode := range []string{"messages", "stream", "count"} {
			t.Run(grouping.name+" "+mode, func(t *testing.T) {
				ids := []string{"call_capture_a", "call_capture_b", "call_capture_c"}
				order := slices.Clone(ids)
				if grouping.reversed {
					slices.Reverse(order)
				}
				var wantImages []string
				wantOutputs := make(map[string]string, len(order))
				for index, id := range order {
					wantImages = append(wantImages, "https://images.invalid/"+id+".png")
					wantOutputs[id] = fmt.Sprintf("[Tool result %d image 1]", index+1)
				}
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					var request struct {
						Model           string                       `json:"model"`
						Stream          bool                         `json:"stream"`
						MaxOutputTokens int                          `json:"max_output_tokens"`
						Input           []map[string]json.RawMessage `json:"input"`
					}
					if err := json.Unmarshal(raw, &request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					valid := r.Method == http.MethodPost && r.URL.Path == providerEndpointResponses && request.Model == "gpt-upstream"
					if mode == "count" && (request.Stream || request.MaxOutputTokens != responsesChatMinimumOutputTokens) {
						valid = false
					}
					for _, field := range []string{`"cache_control":`, `"copilot_cache_control":`, `"origins":`, `"first":`, `"last":`} {
						if bytes.Contains(raw, []byte(field)) {
							valid = false
						}
					}
					var callIDs, outputIDs, images []string
					for _, item := range request.Input {
						switch jsonRawString(item["type"]) {
						case "function_call":
							callIDs = append(callIDs, jsonRawString(item["call_id"]))
							if len(outputIDs) != 0 || len(images) != 0 || jsonRawString(item["name"]) != "capture" || jsonRawString(item["arguments"]) != fmt.Sprintf(`{"page":%d}`, len(callIDs)-1) {
								valid = false
							}
						case "function_call_output":
							id := jsonRawString(item["call_id"])
							outputIDs = append(outputIDs, id)
							if len(callIDs) != len(ids) || len(images) != 0 || jsonRawString(item["output"]) != wantOutputs[id] {
								valid = false
							}
						}
						if jsonRawString(item["role"]) == "user" {
							var parts []map[string]json.RawMessage
							if err := json.Unmarshal(item["content"], &parts); err != nil || len(parts) != 2 {
								valid = false
								continue
							}
							if len(outputIDs) != len(ids) || jsonRawString(parts[0]["type"]) != "input_text" || jsonRawString(parts[1]["type"]) != "input_image" || jsonRawString(parts[0]["text"]) != fmt.Sprintf("Tool result %d image 1 (tool results numbered in conversation order; attachment from tool output):", len(images)+1) {
								valid = false
							}
							images = append(images, jsonRawString(parts[1]["image_url"]))
						}
					}
					if !valid || !slices.Equal(callIDs, ids) || !slices.Equal(outputIDs, order) || !slices.Equal(images, wantImages) {
						t.Errorf("invalid actual Responses dispatch: calls=%v outputs=%v images=%v body=%s", callIDs, outputIDs, images, raw)
						http.Error(w, "invalid Responses tool/image history", http.StatusBadRequest)
						return
					}
					if request.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(streamFixture)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(jsonFixture)
					}
				}))
				defer upstream.Close()
				h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
				var request models.AnthropicRequest
				if err := json.Unmarshal([]byte(parallelImageToolBody(t, grouping.split, grouping.reversed, mode == "stream")), &request); err != nil {
					t.Fatal(err)
				}
				request.Model = "gpt-public"
				last := &request.Messages[len(request.Messages)-1]
				var results []models.ContentBlock
				if err := json.Unmarshal(last.Content, &results); err != nil || len(results) == 0 {
					t.Fatalf("decode final result fixture: %v", err)
				}
				results[len(results)-1].CacheControl = json.RawMessage(`{"type":"ephemeral"}`)
				last.Content = toolImageContractJSON(t, results)
				recorder := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, bytes.NewReader(toolImageContractJSON(t, &request)))
				if mode == "count" {
					h.HandleAnthropicMessagesCountTokens(recorder, req)
				} else {
					h.HandleAnthropicMessages(recorder, req)
				}
				if recorder.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("status/sends = %d/%d: %s", recorder.Code, calls.Load(), recorder.Body.String())
				}
				switch mode {
				case "count":
					var response models.AnthropicCountTokensResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.InputTokens != 11 {
						t.Fatalf("count usage = %+v, error=%v", response, err)
					}
				case "messages":
					var response models.AnthropicResponse
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Model != "gpt-public" || len(response.Content) != 1 || response.Content[0].Text == nil || *response.Content[0].Text != "Synthetic fixture text response." || response.Usage.InputTokens != 8 || response.Usage.CacheReadInputTokens != 3 || response.Usage.OutputTokens != 6 {
						t.Fatalf("message reply/usage = %+v, error=%v", response, err)
					}
				case "stream":
					var finalUsage models.AnthropicUsage
					var text strings.Builder
					stops := 0
					for _, event := range parseSSEEvents(recorder.Body.String()) {
						var decoded models.AnthropicStreamEvent
						if err := json.Unmarshal([]byte(event.Data), &decoded); err != nil {
							t.Fatal(err)
						}
						if event.Event == "message_delta" && decoded.Usage != nil {
							finalUsage = *decoded.Usage
						}
						if event.Event == "content_block_delta" && decoded.Delta != nil {
							text.WriteString(decoded.Delta.Text)
						}
						if event.Event == "message_stop" {
							stops++
						}
					}
					if text.String() != "Synthetic fixture text response." || stops != 1 || finalUsage.InputTokens != 8 || finalUsage.CacheReadInputTokens != 3 || finalUsage.OutputTokens != 6 {
						t.Fatalf("stream text/stops/usage = %q/%d/%+v", text.String(), stops, finalUsage)
					}
				}
			})
		}
	}
}
