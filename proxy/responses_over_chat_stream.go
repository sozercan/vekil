package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

// responsesOverChatResponse is the Responses object emitted by the
// Responses-over-Chat adapter. Usage is null when the upstream reported none;
// the adapter never invents token counts.
type responsesOverChatResponse struct {
	ID                string                        `json:"id"`
	Object            string                        `json:"object"`
	CreatedAt         int64                         `json:"created_at"`
	Status            string                        `json:"status"`
	Model             string                        `json:"model"`
	ParallelToolCalls bool                          `json:"parallel_tool_calls"`
	Store             bool                          `json:"store"`
	Text              json.RawMessage               `json:"text,omitempty"`
	ToolChoice        json.RawMessage               `json:"tool_choice"`
	Tools             json.RawMessage               `json:"tools"`
	Output            []map[string]any              `json:"output"`
	Usage             *policyResponsesResponseUsage `json:"usage"`
	IncompleteDetails any                           `json:"incomplete_details"`
	Error             any                           `json:"error"`
}

// responsesOverChatFailure is a terminal failure discovered after the upstream
// accepted the request. Before any byte is written it becomes an HTTP error;
// afterwards it becomes a response.failed event.
type responsesOverChatFailure struct {
	status  int
	code    string
	message string
}

func (f *responsesOverChatFailure) Error() string { return f.message }

type responsesOverChatToolCall struct {
	chatIndex   int
	callID      string
	chatName    string
	pending     strings.Builder
	arguments   strings.Builder
	added       bool
	itemID      string
	outputIndex int
	descriptor  policyResponsesToolDescriptor
}

// responsesOverChatFinishedItem pairs a completed output item with its output
// index. The index orders the final output and is never part of the item, so
// items emitted in output_item.done can be replayed verbatim as input.
type responsesOverChatFinishedItem struct {
	outputIndex int
	item        map[string]any
}

type responsesOverChatMessage struct {
	itemID      string
	outputIndex int
	text        strings.Builder
}

// responsesOverChatStream converts canonical Chat stream chunks into Responses
// events as they arrive. With a nil writer it runs the same validation and
// returns only the final object, which is how non-streaming requests are served.
type responsesOverChatStream struct {
	w        http.ResponseWriter
	ctx      context.Context
	response responsesOverChatResponse
	config   policyResponsesResponseConfig
	tools    policyResponsesToolMap

	sequence        int
	nextOutputIndex int
	started         bool
	committed       bool
	semantic        bool
	writeFailed     bool

	message      *responsesOverChatMessage
	sawRefusal   bool
	calls        map[int]*responsesOverChatToolCall
	callOrder    []int
	finished     []responsesOverChatFinishedItem
	finishReason string
	usage        *models.OpenAIUsage
	copilotUsage json.RawMessage
	outputBytes  int

	onCommit   func()
	onSemantic func()
}

func newResponsesOverChatStream(w http.ResponseWriter, publicModel string, translated policyResponsesChatRequest) *responsesOverChatStream {
	config := translated.Response
	if len(bytes.TrimSpace(config.Tools)) == 0 {
		config.Tools = json.RawMessage(`[]`)
	}
	if len(bytes.TrimSpace(config.ToolChoice)) == 0 {
		config.ToolChoice = json.RawMessage(`"auto"`)
	}
	return &responsesOverChatStream{
		w:      w,
		config: config,
		tools:  translated.CallableTools,
		calls:  make(map[int]*responsesOverChatToolCall),
		response: responsesOverChatResponse{
			ID:                newPolicyResponsesID("resp"),
			Object:            "response",
			CreatedAt:         time.Now().Unix(),
			Status:            "in_progress",
			Model:             strings.TrimSpace(publicModel),
			ParallelToolCalls: config.ParallelToolCalls,
			Text:              cloneReplayRawMessage(config.Text),
			ToolChoice:        cloneReplayRawMessage(config.ToolChoice),
			Tools:             cloneReplayRawMessage(config.Tools),
			Output:            []map[string]any{},
		},
	}
}

func (s *responsesOverChatStream) streaming() bool { return s.w != nil }

func (s *responsesOverChatStream) emit(eventType string, payload map[string]any) error {
	if !s.streaming() {
		return nil
	}
	if s.writeFailed {
		return errChatStreamClientWriteFailed
	}
	payload["type"] = eventType
	payload["sequence_number"] = s.sequence
	if err := writeSSEEvent(s.w, eventType, payload); err != nil {
		s.writeFailed = true
		return errors.Join(errChatStreamClientWriteFailed, err)
	}
	s.sequence++
	if !s.committed {
		s.committed = true
		if s.onCommit != nil {
			s.onCommit()
		}
	}
	return nil
}

func (s *responsesOverChatStream) markSemantic() {
	if s.semantic {
		return
	}
	s.semantic = true
	if s.onSemantic != nil {
		s.onSemantic()
	}
}

func (s *responsesOverChatStream) snapshot(status string) responsesOverChatResponse {
	snapshot := s.response
	snapshot.Status = status
	snapshot.Output = append([]map[string]any{}, s.response.Output...)
	return snapshot
}

// start writes response.created and response.in_progress. It is called only
// after the upstream accepted the request.
func (s *responsesOverChatStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	created := s.snapshot("in_progress")
	if err := s.emit("response.created", map[string]any{"response": created}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{"response": created})
}

func (s *responsesOverChatStream) addOutputBytes(n int) error {
	s.outputBytes += n
	if s.outputBytes > responsesOverChatMaxOutputBytes {
		return &responsesOverChatFailure{status: http.StatusBadGateway, code: "response_too_large", message: "upstream response exceeds the Responses-over-Chat output limit"}
	}
	return nil
}

func (s *responsesOverChatStream) openMessage() error {
	if s.message != nil {
		return nil
	}
	s.message = &responsesOverChatMessage{itemID: newPolicyResponsesID("msg"), outputIndex: s.nextOutputIndex}
	s.nextOutputIndex++
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": s.message.outputIndex,
		"item":         map[string]any{"id": s.message.itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	}); err != nil {
		return err
	}
	return s.emit("response.content_part.added", map[string]any{
		"item_id": s.message.itemID, "output_index": s.message.outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (s *responsesOverChatStream) appendText(text string) error {
	if text == "" {
		return nil
	}
	if err := s.addOutputBytes(len(text)); err != nil {
		return err
	}
	if err := s.openMessage(); err != nil {
		return err
	}
	s.message.text.WriteString(text)
	s.markSemantic()
	return s.emit("response.output_text.delta", map[string]any{
		"item_id": s.message.itemID, "output_index": s.message.outputIndex, "content_index": 0,
		"delta": text, "logprobs": []any{},
	})
}

func (s *responsesOverChatStream) closeMessage(status string) error {
	if s.message == nil {
		return nil
	}
	message := s.message
	s.message = nil
	text := message.text.String()
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	if err := s.emit("response.output_text.done", map[string]any{
		"item_id": message.itemID, "output_index": message.outputIndex, "content_index": 0, "text": text, "logprobs": []any{},
	}); err != nil {
		return err
	}
	if err := s.emit("response.content_part.done", map[string]any{
		"item_id": message.itemID, "output_index": message.outputIndex, "content_index": 0, "part": part,
	}); err != nil {
		return err
	}
	item := map[string]any{"id": message.itemID, "type": "message", "status": status, "role": "assistant", "content": []any{part}}
	s.finished = append(s.finished, responsesOverChatFinishedItem{outputIndex: message.outputIndex, item: item})
	return s.emit("response.output_item.done", map[string]any{"output_index": message.outputIndex, "item": item})
}

func (s *responsesOverChatStream) toolCallFor(delta models.OpenAIToolCall) (*responsesOverChatToolCall, error) {
	index := -1
	if delta.Index != nil {
		index = *delta.Index
	} else if id := strings.TrimSpace(delta.ID); id != "" {
		for _, chatIndex := range s.callOrder {
			if s.calls[chatIndex].callID == id {
				index = chatIndex
				break
			}
		}
		if index < 0 {
			index = len(s.callOrder)
			for s.calls[index] != nil {
				index++
			}
		}
	} else if len(s.callOrder) > 0 {
		index = s.callOrder[len(s.callOrder)-1]
	} else {
		index = 0
	}
	if index < 0 || index > policyResponsesMaxFunctionTools {
		return nil, &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned an invalid tool-call index"}
	}
	call := s.calls[index]
	if call == nil {
		if len(s.callOrder) >= policyResponsesMaxFunctionTools {
			return nil, &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned too many tool calls"}
		}
		call = &responsesOverChatToolCall{chatIndex: index}
		s.calls[index] = call
		s.callOrder = append(s.callOrder, index)
	}
	return call, nil
}

func (s *responsesOverChatStream) applyToolCallDelta(delta models.OpenAIToolCall) error {
	if delta.Type != "" && delta.Type != policyResponsesToolKindFunction {
		return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: fmt.Sprintf("upstream returned unsupported tool call type %q", delta.Type)}
	}
	call, err := s.toolCallFor(delta)
	if err != nil {
		return err
	}
	if id := strings.TrimSpace(delta.ID); id != "" {
		if call.callID != "" && call.callID != id {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream changed a tool-call ID mid-stream"}
		}
		if len(id) > policyResponsesMaxToolNameLen {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned an oversized tool-call ID"}
		}
		call.callID = id
	}
	if name := delta.Function.Name; name != "" && name != call.chatName {
		if call.added {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream changed a tool name after the call started"}
		}
		// Some providers fragment names; identical repeats are ignored above.
		call.chatName += name
		if len(call.chatName) > policyResponsesMaxToolNameLen {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned an oversized tool name"}
		}
	}
	if fragment := delta.Function.Arguments; fragment != "" {
		if err := s.addOutputBytes(len(fragment)); err != nil {
			return err
		}
		if call.added {
			call.arguments.WriteString(fragment)
			if err := s.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": call.itemID, "output_index": call.outputIndex, "delta": fragment,
			}); err != nil {
				return err
			}
		} else {
			call.pending.WriteString(fragment)
		}
	}
	// Providers may fragment the name across deltas, and activity on another
	// call index or text says nothing about this call's name. The call is
	// validated and exposed only once its own arguments start, or at a
	// successful finish. Output indexes follow exposure order, so they stay
	// monotonic even when a later Chat index is exposed first.
	if !call.added && call.callID != "" && call.chatName != "" && call.pending.Len() > 0 {
		return s.addToolCall(call)
	}
	return nil
}

// addToolCall exposes a call once its identity is known. Codex only executes a
// call on output_item.done, which is emitted after a successful finish.
func (s *responsesOverChatStream) addToolCall(call *responsesOverChatToolCall) error {
	descriptor, known := s.tools[strings.TrimSpace(call.chatName)]
	if !known {
		return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream called a function tool that is not declared or not allowed by tool_choice"}
	}
	if s.sawRefusal {
		return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned a refusal together with function calls"}
	}
	if err := s.closeMessage("completed"); err != nil {
		return err
	}
	call.descriptor = descriptor
	call.added = true
	call.itemID = newPolicyResponsesID("fc")
	call.outputIndex = s.nextOutputIndex
	s.nextOutputIndex++
	s.markSemantic()
	if err := s.emit("response.output_item.added", map[string]any{
		"output_index": call.outputIndex,
		"item":         s.functionCallItem(call, "in_progress", ""),
	}); err != nil {
		return err
	}
	if pending := call.pending.String(); pending != "" {
		call.pending.Reset()
		call.arguments.WriteString(pending)
		return s.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": call.itemID, "output_index": call.outputIndex, "delta": pending,
		})
	}
	return nil
}

func (s *responsesOverChatStream) functionCallItem(call *responsesOverChatToolCall, status, arguments string) map[string]any {
	item := map[string]any{
		"id": call.itemID, "type": "function_call", "status": status,
		"call_id": call.callID, "name": call.descriptor.Name, "arguments": arguments,
	}
	if call.descriptor.Namespace != "" {
		item["namespace"] = call.descriptor.Namespace
	}
	return item
}

// consume applies one canonical Chat stream chunk.
func (s *responsesOverChatStream) consume(chunk models.OpenAIStreamChunk) error {
	if chunk.Usage != nil {
		usage := *chunk.Usage
		s.usage = &usage
	}
	if !rawJSONIsNullOrEmpty(chunk.CopilotUsage) {
		s.copilotUsage = append(s.copilotUsage[:0], chunk.CopilotUsage...)
	}
	if len(chunk.Choices) > 1 {
		return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned more than one Chat choice"}
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned an unexpected Chat choice index"}
		}
		text, _, err := policyResponsesTextFromChatContent(choice.Delta.Content)
		if err != nil {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned non-text Chat content"}
		}
		if s.finishReason != "" && (text != "" || len(choice.Delta.ToolCalls) > 0) {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream sent output after its finish reason"}
		}
		if text != "" {
			// Text after a function call opens a new message item, so output
			// indexes stay monotonic.
			if err := s.appendText(text); err != nil {
				return err
			}
		}
		refusal, _, err := policyResponsesRefusalFromChatContent(choice.Delta.Refusal)
		if err != nil {
			return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned a non-text refusal"}
		}
		if refusal != "" {
			// Any started call, exposed or still pending, conflicts with a refusal.
			if len(s.callOrder) > 0 {
				return &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned a refusal together with function calls"}
			}
			s.sawRefusal = true
			// Codex accepts output_text but not the refusal content variant.
			if err := s.appendText(refusal); err != nil {
				return err
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			if err := s.applyToolCallDelta(delta); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && s.finishReason == "" {
			s.finishReason = strings.TrimSpace(*choice.FinishReason)
		}
	}
	return nil
}

// finish validates the terminal Chat state and emits the terminal Responses
// event. It returns the final response, or a failure when the upstream turn
// cannot be represented honestly as completed or incomplete.
func (s *responsesOverChatStream) finish() (responsesOverChatResponse, *responsesOverChatFailure) {
	if s.usage != nil {
		usage := policyResponsesUsageFromChat(s.usage)
		s.response.Usage = &usage
	}
	fail := func(failure *responsesOverChatFailure) (responsesOverChatResponse, *responsesOverChatFailure) {
		return s.snapshot("failed"), failure
	}
	executable := false
	switch s.finishReason {
	case "tool_calls", "function_call":
		if len(s.callOrder) == 0 {
			return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream finished with tool_calls but returned no function calls"})
		}
		executable = true
	case "stop":
		// Gemini through Copilot may report stop for a turn that ends in function
		// calls. Complete calls are still the model's answer.
		executable = len(s.callOrder) > 0
		if !executable && s.config.RequiresToolCall {
			return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "tool_call_required", message: "model stopped without the function call required by tool_choice"})
		}
	case "length":
		s.response.IncompleteDetails = map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		s.response.IncompleteDetails = map[string]any{"reason": "content_filter"}
	case "":
		return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "incomplete_upstream_stream", message: "upstream Chat stream ended before a finish reason"})
	default:
		return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: fmt.Sprintf("upstream returned unsupported finish reason %q", s.finishReason)})
	}

	status := "completed"
	if s.response.IncompleteDetails != nil {
		status = "incomplete"
	}
	if executable {
		if !s.config.ParallelToolCalls && len(s.callOrder) > 1 {
			return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned parallel function calls while parallel_tool_calls is false"})
		}
		for _, index := range s.callOrder {
			call := s.calls[index]
			if !call.added {
				if call.chatName == "" {
					return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned a function call without a name"})
				}
				if call.callID == "" {
					call.callID = newPolicyResponsesID("call")
				}
				if err := s.addToolCall(call); err != nil {
					return fail(asResponsesOverChatFailure(err))
				}
			}
			arguments := call.arguments.String()
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			if !json.Valid([]byte(arguments)) {
				return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned invalid JSON function arguments"})
			}
			call.arguments.Reset()
			call.arguments.WriteString(arguments)
		}
		seen := make(map[string]struct{}, len(s.callOrder))
		for _, index := range s.callOrder {
			if _, duplicate := seen[s.calls[index].callID]; duplicate {
				return fail(&responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: "upstream returned duplicate function call IDs"})
			}
			seen[s.calls[index].callID] = struct{}{}
		}
	}
	if err := s.closeMessage(status); err != nil {
		return fail(asResponsesOverChatFailure(err))
	}
	if executable {
		calls := make([]*responsesOverChatToolCall, 0, len(s.callOrder))
		for _, index := range s.callOrder {
			calls = append(calls, s.calls[index])
		}
		sort.SliceStable(calls, func(i, j int) bool { return calls[i].outputIndex < calls[j].outputIndex })
		for _, call := range calls {
			arguments := call.arguments.String()
			if err := s.emit("response.function_call_arguments.done", map[string]any{
				"item_id": call.itemID, "output_index": call.outputIndex, "name": call.descriptor.Name, "arguments": arguments,
			}); err != nil {
				return fail(asResponsesOverChatFailure(err))
			}
			item := s.functionCallItem(call, "completed", arguments)
			s.finished = append(s.finished, responsesOverChatFinishedItem{outputIndex: call.outputIndex, item: item})
			if err := s.emit("response.output_item.done", map[string]any{"output_index": call.outputIndex, "item": item}); err != nil {
				return fail(asResponsesOverChatFailure(err))
			}
		}
	}
	// A truncated turn never exposes its partial function calls as output; Codex
	// would otherwise dispatch incomplete JSON.
	sort.SliceStable(s.finished, func(i, j int) bool { return s.finished[i].outputIndex < s.finished[j].outputIndex })
	output := make([]map[string]any, 0, len(s.finished))
	for _, finished := range s.finished {
		output = append(output, finished.item)
	}
	s.response.Output = output
	final := s.snapshot(status)
	terminal := "response.completed"
	if status == "incomplete" {
		terminal = "response.incomplete"
	}
	if err := s.emit(terminal, map[string]any{"response": final}); err != nil {
		return fail(asResponsesOverChatFailure(err))
	}
	return final, nil
}

func asResponsesOverChatFailure(err error) *responsesOverChatFailure {
	var failure *responsesOverChatFailure
	if errors.As(err, &failure) {
		return failure
	}
	if errors.Is(err, errChatStreamClientWriteFailed) {
		return &responsesOverChatFailure{status: 499, code: "client_write_failed", message: "writing the Responses stream to the client failed"}
	}
	return &responsesOverChatFailure{status: http.StatusBadGateway, code: "server_error", message: err.Error()}
}

// fail emits response.failed after the stream was committed.
func (s *responsesOverChatStream) fail(failure *responsesOverChatFailure) error {
	response := s.snapshot("failed")
	if s.usage != nil {
		usage := policyResponsesUsageFromChat(s.usage)
		response.Usage = &usage
	}
	response.Output = []map[string]any{}
	response.Error = map[string]any{"code": failure.code, "message": failure.message}
	return s.emit("response.failed", map[string]any{"response": response})
}

// streamResponsesOverChat consumes the native Chat SSE body and writes either
// incremental Responses SSE or one final Responses JSON object.
func (h *ProxyHandler) streamResponsesOverChat(w http.ResponseWriter, r *http.Request, upstreamCtx context.Context, resp *http.Response, translated policyResponsesChatRequest, publicModel, toolScope string) {
	lifecycleBody := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
	defer func() { _ = lifecycleBody.Close() }()

	var writer http.ResponseWriter
	var tracked *commitTrackingResponseWriter
	if translated.Stream {
		tracked = &commitTrackingResponseWriter{ResponseWriter: w}
		writer = tracked
		setSSEHeaders(w)
		w.Header().Del("Content-Length")
	}
	state := newResponsesOverChatStream(writer, publicModel, translated)
	state.onCommit = func() { markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame) }
	state.onSemantic = func() { markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic) }

	var aggregator *openAIResponseAggregator
	finalCallback := h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, toolScope)
	if finalCallback != nil {
		aggregator = newOpenAIResponseAggregator()
	}

	var consumeErr error
	if err := state.start(); err != nil {
		consumeErr = err
	}
	if consumeErr == nil {
		// A missing [DONE] after a finish reason is tolerated; a stream that ends
		// before any finish reason fails in finish(). Malformed chunks fail the
		// turn: skipping one could turn a corrupted argument delta into an
		// executable call with different arguments.
		_, readErr := consumeOpenAIStreamChunksStrict(lifecycleBody, func(chunk models.OpenAIStreamChunk) bool {
			if aggregator != nil {
				addChatStreamChunkForToolCapture(aggregator, chunk)
			}
			if err := state.consume(chunk); err != nil {
				consumeErr = err
				return false
			}
			return true
		})
		if consumeErr == nil && readErr != nil {
			consumeErr = readErr
		}
	}
	if !rawJSONIsNullOrEmpty(state.copilotUsage) {
		observeCopilotUsage(r.Context(), state.copilotUsage)
	}
	observeOpenAIUsage(r.Context(), state.usage)

	committed := tracked != nil && tracked.committed
	lifecycle := h.lifecycleStreamHooks(r.Context(), lifecycleBody.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) })

	var failure *responsesOverChatFailure
	var final responsesOverChatResponse
	switch {
	case consumeErr != nil && errors.Is(consumeErr, errChatStreamClientWriteFailed):
		// The client is gone; nothing further can be delivered.
		return
	case consumeErr != nil:
		if lifecycle.suppressTransportCancellation(committed) {
			if committed {
				_ = state.fail(&responsesOverChatFailure{status: http.StatusServiceUnavailable, code: "server_shutting_down", message: "Vekil is shutting down"})
			}
			return
		}
		var streamErr *openAIStreamError
		switch {
		case errors.As(consumeErr, &streamErr):
			failure = &responsesOverChatFailure{status: streamErr.httpStatus(), code: firstNonEmpty(streamErr.Code, "upstream_error"), message: streamErr.Error()}
		case errors.Is(consumeErr, errOpenAIStreamMalformedChunk):
			failure = &responsesOverChatFailure{status: http.StatusBadGateway, code: "invalid_upstream_stream", message: consumeErr.Error()}
		default:
			failure = asResponsesOverChatFailure(consumeErr)
		}
	default:
		final, failure = state.finish()
		if failure != nil && failure.code == "client_write_failed" {
			return
		}
	}

	if aggregator != nil && failure == nil {
		finalCallback(aggregator.buildResponse())
	}
	if failure != nil {
		observeResponseFailureStatus(r.Context(), failure.status)
		if h.log != nil {
			h.log.Warn("responses-over-chat upstream turn failed", logger.F("code", failure.code), logger.F("status", failure.status))
		}
		if state.streaming() {
			_ = state.fail(failure)
			return
		}
		status := failure.status
		if status < http.StatusBadRequest || status > 599 {
			status = http.StatusBadGateway
		}
		writeOpenAIErrorWithDetails(w, status, failure.message, openAIErrorTypeForHTTPStatus(status), "", failure.code)
		return
	}
	if state.streaming() {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(final); err != nil && h.log != nil {
		h.log.Debug("failed to write Responses-over-Chat result", logger.Err(err))
	}
}
