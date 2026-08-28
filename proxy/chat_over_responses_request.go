package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/sozercan/vekil/logger"
)

func newChatInvalidRequest(param, message string) *chatExecutionError {
	return &chatExecutionError{
		StatusCode: http.StatusBadRequest,
		Type:       "invalid_request_error",
		Param:      param,
		Message:    message,
	}
}

type responsesChatRequestOptions struct {
	UpstreamModel       string
	CarriedReasoning    map[string]carriedReplay
	ReplayStore         *responsesChatReplayStore
	ReplayRoute         responsesChatReplayRoute
	Log                 *logger.Logger
	MinimumOutputTokens int
	DropSamplingParams  bool
	// Anthropic clients hold their replay state in a transcript they already sent and
	// cannot repair, so a carrier that cannot answer degrades to the visible turn.
	// Native Chat owns its history and stays loud. Off while probing candidates: the
	// refusal is also how a target is chosen, and a degrade would pick the first one.
	DegradeUnrestorableReplay bool
}

type responsesChatRequestPlan struct {
	Body               []byte
	Stream             bool
	IncludeUsage       bool
	ReplayToolDefaults responsesChatReplayToolDefaults
}

type responsesChatRequestEnvelope struct {
	Model       string                     `json:"model"`
	Input       []json.RawMessage          `json:"input"`
	Stream      bool                       `json:"stream"`
	MaxOutput   *int                       `json:"max_output_tokens,omitempty"`
	Temperature *float64                   `json:"temperature,omitempty"`
	TopP        *float64                   `json:"top_p,omitempty"`
	Tools       []json.RawMessage          `json:"tools,omitempty"`
	ToolChoice  json.RawMessage            `json:"tool_choice,omitempty"`
	Parallel    *bool                      `json:"parallel_tool_calls,omitempty"`
	Text        map[string]json.RawMessage `json:"text,omitempty"`
	Reasoning   map[string]json.RawMessage `json:"reasoning,omitempty"`
	Metadata    map[string]json.RawMessage `json:"metadata,omitempty"`
	Store       *bool                      `json:"store,omitempty"`
	User        *string                    `json:"user,omitempty"`
	PromptCache *string                    `json:"prompt_cache_key,omitempty"`
	SafetyID    *string                    `json:"safety_identifier,omitempty"`
	Include     []string                   `json:"include,omitempty"`
}

func translateChatRequestToResponses(chatBody []byte, options responsesChatRequestOptions) (responsesChatRequestPlan, error) {
	raw, err := decodeChatJSONObject(chatBody, "")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	if err := validateChatResponsesTopLevel(raw); err != nil {
		return responsesChatRequestPlan{}, err
	}

	model, err := requiredJSONString(raw, "model")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	upstreamModel := strings.TrimSpace(options.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = model
	}

	messagesRaw, ok := raw["messages"]
	if !ok {
		return responsesChatRequestPlan{}, newChatInvalidRequest("messages", "messages is required")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil || len(messages) == 0 {
		return responsesChatRequestPlan{}, newChatInvalidRequest("messages", "messages must be a non-empty array")
	}
	input, err := translateChatMessagesToResponses(messages, options)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}

	stream := false
	if value, ok := raw["stream"]; ok {
		if err := json.Unmarshal(value, &stream); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("stream", "stream must be a boolean")
		}
	}

	maxOutput, err := chatMaxOutputTokens(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	if options.MinimumOutputTokens > 0 && (maxOutput == nil || *maxOutput < options.MinimumOutputTokens) {
		minimum := options.MinimumOutputTokens
		maxOutput = &minimum
	}
	if options.MinimumOutputTokens == 0 && maxOutput != nil && *maxOutput < responsesChatMinimumOutputTokens {
		param := "max_tokens"
		if _, ok := raw["max_completion_tokens"]; ok {
			param = "max_completion_tokens"
		}
		return responsesChatRequestPlan{}, newChatInvalidRequest(param, "Responses-backed Chat requires an output token limit of at least 16")
	}

	var temperature, topP *float64
	if value, ok := raw["temperature"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var parsed float64
		if err := json.Unmarshal(value, &parsed); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("temperature", "temperature must be a number")
		}
		temperature = &parsed
	}
	if value, ok := raw["top_p"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var parsed float64
		if err := json.Unmarshal(value, &parsed); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("top_p", "top_p must be a number")
		}
		topP = &parsed
	}
	if options.DropSamplingParams {
		temperature = nil
		topP = nil
	}

	includeUsage, err := parseChatStreamOptions(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	tools, toolNames, replayToolDefaults, err := translateChatTools(raw["tools"])
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	toolChoice, err := translateChatToolChoice(raw["tool_choice"], toolNames)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	var parallel *bool
	if value, ok := raw["parallel_tool_calls"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var parsed bool
		if err := json.Unmarshal(value, &parsed); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("parallel_tool_calls", "parallel_tool_calls must be a boolean")
		}
		parallel = &parsed
	}
	textConfig, err := translateChatTextConfiguration(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	reasoningConfig, err := translateChatReasoningConfiguration(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	metadata, err := optionalJSONObject(raw, "metadata")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	store, err := optionalJSONBool(raw, "store")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	if store == nil {
		defaultStore := false
		store = &defaultStore
	}
	user, err := optionalJSONString(raw, "user")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	promptCache, err := optionalJSONString(raw, "prompt_cache_key")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	safetyID, err := optionalJSONString(raw, "safety_identifier")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}

	envelope := responsesChatRequestEnvelope{
		Model:       upstreamModel,
		Input:       input,
		Stream:      stream,
		MaxOutput:   maxOutput,
		Temperature: temperature,
		TopP:        topP,
		Tools:       tools,
		ToolChoice:  toolChoice,
		Parallel:    parallel,
		Text:        textConfig,
		Reasoning:   reasoningConfig,
		Metadata:    metadata,
		Store:       store,
		User:        user,
		PromptCache: promptCache,
		SafetyID:    safetyID,
		Include:     []string{"reasoning.encrypted_content"},
	}
	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		return responsesChatRequestPlan{}, fmt.Errorf("marshal Responses request: %w", marshalErr)
	}
	return responsesChatRequestPlan{Body: body, Stream: stream, IncludeUsage: includeUsage, ReplayToolDefaults: replayToolDefaults}, nil
}

func requiredJSONString(raw map[string]json.RawMessage, field string) (string, error) {
	value, ok := raw[field]
	if !ok {
		return "", newChatInvalidRequest(field, field+" is required")
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil || strings.TrimSpace(parsed) == "" {
		return "", newChatInvalidRequest(field, field+" must be a non-empty string")
	}
	return strings.TrimSpace(parsed), nil
}

func chatMaxOutputTokens(raw map[string]json.RawMessage) (*int, error) {
	var maxTokens, maxCompletion *int
	for field, target := range map[string]**int{
		"max_tokens":            &maxTokens,
		"max_completion_tokens": &maxCompletion,
	} {
		value, ok := raw[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		var parsed int
		if err := json.Unmarshal(value, &parsed); err != nil || parsed < 0 {
			return nil, newChatInvalidRequest(field, field+" must be a non-negative integer")
		}
		*target = &parsed
	}
	if maxTokens != nil && maxCompletion != nil && *maxTokens != *maxCompletion {
		return nil, newChatInvalidRequest("max_completion_tokens", "max_tokens and max_completion_tokens must match")
	}
	if maxCompletion != nil {
		return maxCompletion, nil
	}
	return maxTokens, nil
}

func translateChatMessagesToResponses(messages []json.RawMessage, options responsesChatRequestOptions) ([]json.RawMessage, error) {
	resultIndices, err := chatToolResultIndices(messages)
	if err != nil {
		return nil, err
	}
	// The slice may grow when one Chat message expands to multiple Responses
	// items. Start with the bounded decoded message count and let append grow it;
	// avoid arithmetic on an untrusted length in the allocation size.
	input := make([]json.RawMessage, 0, len(messages))
	var toolTurnStarts []int
	calls := make(map[string]string)
	results := make(map[string]struct{})
	restoredGroups := make(map[string]struct{})
	// Accumulated across the whole request and flushed once on the way out; see
	// responsesChatRestoreTally for why this is not logged per turn.
	var tally responsesChatRestoreTally
	for index, raw := range messages {
		messageParam := fmt.Sprintf("messages[%d]", index)
		if _, err := validateChatRawObjectFields(raw, messageParam, "role", "content", "refusal", "name", "tool_calls", "tool_call_id"); err != nil {
			return nil, err
		}
		var message struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			Refusal    json.RawMessage   `json:"refusal"`
			Name       string            `json:"name"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
			ToolCallID string            `json:"tool_call_id"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "message must be an object")
		}
		role := strings.TrimSpace(message.Role)
		if strings.TrimSpace(message.Name) != "" {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].name", index), "message name is not supported")
		}
		switch role {
		case "system", "developer", "user":
			if len(bytes.TrimSpace(message.Refusal)) > 0 && !bytes.Equal(bytes.TrimSpace(message.Refusal), []byte("null")) {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", index), "refusal is valid only for assistant messages")
			}
			if len(message.ToolCalls) > 0 || strings.TrimSpace(message.ToolCallID) != "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "tool fields are not valid for this message role")
			}
			content, err := translateChatMessageContent(message.Content, role, index)
			if err != nil {
				return nil, err
			}
			if len(content) == 0 {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].content", index), "message content must not be empty")
			}
			item, _ := json.Marshal(map[string]any{"type": "message", "role": role, "content": content})
			input = append(input, item)
		case "assistant":
			if strings.TrimSpace(message.ToolCallID) != "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "tool_call_id is not valid for assistant messages")
			}
			content, err := translateChatMessageContent(message.Content, role, index)
			if err != nil {
				return nil, err
			}
			refusal, err := translateChatAssistantRefusal(message.Refusal, index)
			if err != nil {
				return nil, err
			}
			if len(message.ToolCalls) == 0 {
				assistantText := assistantHistoryText(content) + refusal
				if assistantText == "" {
					return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "assistant message must contain content, refusal, or tool calls")
				}
				input = appendAssistantHistoryMessage(input, assistantText)
				continue
			}
			toolTurnStarts = append(toolTurnStarts, len(input))

			projected := make([]responsesChatReplayProjectedCall, len(message.ToolCalls))
			syntheticItems := make([]json.RawMessage, len(message.ToolCalls))
			replayCalls := 0
			for callIndex, callRaw := range message.ToolCalls {
				translatedCall, err := translateSyntheticChatToolCall(callRaw, index, callIndex)
				if err != nil {
					return nil, err
				}
				projected[callIndex] = responsesChatReplayProjectedCall{ID: translatedCall.ID, Name: translatedCall.Name, Arguments: translatedCall.Arguments}
				syntheticItems[callIndex] = translatedCall.Item
				if isResponsesChatReplayCallID(translatedCall.ID) {
					replayCalls++
				}
			}
			if replayCalls > 0 && refusal != "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", index), "refusal is not supported on Responses replay tool-call messages")
			}
			if replayCalls != 0 && replayCalls != len(projected) {
				return nil, replayChatExecutionError(responsesChatReplayMixedCode, responsesChatReplayMixedMessage)
			}
			if replayCalls == 0 {
				input, err = appendVisibleAssistantTurn(input, calls, resultIndices, projected, syntheticItems, assistantHistoryText(content)+refusal, index)
				if err != nil {
					return nil, err
				}
				continue
			}
			restored, err := restoreResponsesChatCalls(options, projected, content, &tally)
			if err != nil {
				return nil, err
			}
			if restored.VisibleTranscriptFallback != nil {
				// A client cannot repair a transcript it already sent. Direct Anthropic ingress
				// explicitly trades the missing-state 400 for a turn without hidden reasoning;
				// native Chat never receives this successful fallback result.
				recordResponsesChatReplayDegrade(&tally, projected, content, restored.VisibleTranscriptFallback)
				input, err = appendVisibleAssistantTurn(input, calls, resultIndices, projected, syntheticItems, assistantHistoryText(content)+refusal, index)
				if err != nil {
					return nil, err
				}
				continue
			}
			if _, duplicate := restoredGroups[restored.Key]; duplicate {
				return nil, replayChatExecutionError(responsesChatReplayProjectionCode, "Responses replay group appears more than once in the request.")
			}
			restoredGroups[restored.Key] = struct{}{}
			if restored.Rebuild {
				restored = reconstructCarriedRestore(restored, projected, assistantHistoryText(content))
			}
			resolvedByProxy := make(map[string]responsesChatReplayResolvedCall, len(restored.Calls))
			matchedResults := 0
			for _, call := range restored.Calls {
				resolvedByProxy[call.ProxyCallID] = call
				if resultIndex, ok := resultIndices[call.ProxyCallID]; ok && resultIndex > index {
					matchedResults++
				}
			}
			if matchedResults == 0 {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "Responses-backed assistant tool calls require at least one subsequent tool result")
			}
			if matchedResults == len(restored.Calls) {
				input = append(input, cloneReplayRawMessages(restored.OutputItems)...)
			} else {
				// Live gpt-5.6-sol rejects a complete parallel call group when only a
				// subset has outputs. Replay the visible assistant text plus only the
				// exact calls that have results; the store remains intact for retries.
				if assistantText := assistantHistoryText(content) + refusal; assistantText != "" {
					input = appendAssistantHistoryMessage(input, assistantText)
				}
				for _, projectedCall := range projected {
					resolved := resolvedByProxy[projectedCall.ID]
					if resultIndex, ok := resultIndices[projectedCall.ID]; ok && resultIndex > index {
						input = append(input, cloneReplayRawMessage(resolved.OutputItem))
					}
				}
			}
			for callIndex, projectedCall := range projected {
				if _, duplicate := calls[projectedCall.ID]; duplicate {
					return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_calls[%d].id", index, callIndex), "duplicate tool call ID")
				}
				calls[projectedCall.ID] = resolvedByProxy[projectedCall.ID].UpstreamCallID
			}
		case "tool":
			if len(bytes.TrimSpace(message.Refusal)) > 0 && !bytes.Equal(bytes.TrimSpace(message.Refusal), []byte("null")) {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", index), "refusal is not valid for tool messages")
			}
			if len(message.ToolCalls) > 0 {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_calls", index), "tool_calls is not valid for tool messages")
			}
			callID := strings.TrimSpace(message.ToolCallID)
			if callID == "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "tool_call_id is required")
			}
			upstreamCallID, ok := calls[callID]
			if !ok {
				if isResponsesChatReplayCallID(callID) {
					return nil, missingResponsesChatReplayError()
				}
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "tool result references no prior assistant tool call")
			}
			if _, duplicate := results[callID]; duplicate {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "duplicate tool result")
			}
			results[callID] = struct{}{}
			output, err := compactChatToolOutput(message.Content, index)
			if err != nil {
				return nil, err
			}
			item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": upstreamCallID, "output": output})
			input = append(input, item)
		default:
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].role", index), "unsupported message role")
		}
	}
	// Only on the way out with a request that survived: a failed translate reports itself,
	// and the policy tier probe fails routinely by design.
	tally.flush(options)
	return trimAgedReasoning(input, toolTurnStarts, options), nil
}

// Copilot rejects store and previous_response_id, so reasoning rides every later request
// body: one 1490-turn session replayed 11.3 MB of it, a 100-turn block 0.7 MB of that.
const reasoningToolTurnBlock = 100

// Upstream caches on strict prefix: probed, 3 words changed near the front took cached
// from 9012 to 0. Quantising holds the cutoff still per block; the window runs N..2N-1.
func agedReasoningToolTurns(toolTurns int) int {
	return max(toolTurns/reasoningToolTurnBlock-1, 0) * reasoningToolTurnBlock
}

// Only reasoning is droppable: dropping a function_call instead was measured against
// Copilot as "No tool call found for function call output", so the mandatory floor stays.
func trimAgedReasoning(input []json.RawMessage, toolTurnStarts []int, options responsesChatRequestOptions) []json.RawMessage {
	agedTurns := agedReasoningToolTurns(len(toolTurnStarts))
	if agedTurns == 0 {
		return input
	}
	aged := toolTurnStarts[agedTurns]
	kept := make([]json.RawMessage, 0, len(input))
	items, itemBytes, retained := 0, 0, 0
	for index, item := range input {
		if itemType, _ := carriedItemHeader(item); itemType == "reasoning" {
			if index < aged {
				items++
				itemBytes += len(item)
				continue
			}
			retained++
		}
		kept = append(kept, item)
	}
	if items == 0 {
		return input
	}
	logTrimmedReasoning(options, len(toolTurnStarts), agedTurns, items, itemBytes, retained)
	return kept
}

// Debug, not warn: past 200 tool turns this is the steady state and fires every turn,
// where the warns beside it mark continuity vekil expected to keep and lost.
func logTrimmedReasoning(options responsesChatRequestOptions, toolTurns, agedTurns, items, itemBytes, retained int) {
	if options.Log == nil {
		return
	}
	// retained_reasoning_items is the window's whole point: a starved request keeps none.
	options.Log.Debug("trimmed reasoning from tool turns older than the retained window",
		logger.F("model", options.ReplayRoute.PublicModel),
		logger.F("tool_turns", toolTurns),
		logger.F("aged_turns", agedTurns),
		logger.F("retained_turns", toolTurns-agedTurns),
		logger.F("reasoning_items", items),
		logger.F("reasoning_bytes", itemBytes),
		logger.F("retained_reasoning_items", retained),
	)
}

type responsesChatRestoredCalls struct {
	Key                       string
	OutputItems               []json.RawMessage
	Calls                     []responsesChatReplayResolvedCall
	TextItemIndex             *int
	VisibleTranscriptFallback *responsesChatVisibleTranscriptFallback
	// Rebuild marks a carrier restore whose items are not upstream's own and have to be
	// rebuilt from the transcript before use.
	Rebuild bool
}

// A successful, opt-in recovery outcome rather than an error. The caller rebuilds this
// turn from the visible transcript, using each opaque proxy ID as its stateless call_id.
type responsesChatVisibleTranscriptFallback struct {
	diverged string
	carrier  string
}

// When the replay store and carrier cannot answer, a self-describing ID can
// still recover the upstream call mapping. It carries no hidden items. The
// caller rebuilds those from the transcript through reconstructCarriedRestore.
func selfDescribingRestoredCalls(projected []responsesChatReplayProjectedCall, projectionDigest string) (responsesChatRestoredCalls, bool) {
	if len(projected) == 0 {
		return responsesChatRestoredCalls{}, false
	}
	calls := make([]responsesChatReplayResolvedCall, len(projected))
	for i, projectedCall := range projected {
		upstreamCallID, ok := responsesChatReplayUpstreamCallID(projectedCall.ID)
		if !ok {
			return responsesChatRestoredCalls{}, false
		}
		calls[i] = responsesChatReplayResolvedCall{
			ProxyCallID:     projectedCall.ID,
			UpstreamCallID:  upstreamCallID,
			Name:            strings.TrimSpace(projectedCall.Name),
			OutputItemIndex: i,
		}
	}
	return responsesChatRestoredCalls{
		Key:     "selfid:" + projectionDigest,
		Calls:   calls,
		Rebuild: true,
	}, true
}

// The store is authoritative while it holds the group and its arguments still match.
// Clients rewrite arguments; the carrier does not bind them and holds only the client's
// own ciphertext (see carriedProjectionDigest), so trying it grants no extra reach.
func restoreResponsesChatCalls(options responsesChatRequestOptions, projected []responsesChatReplayProjectedCall, content []map[string]any, tally *responsesChatRestoreTally) (responsesChatRestoredCalls, error) {
	projectionContent, err := json.Marshal(assistantHistoryText(content))
	if err != nil {
		return responsesChatRestoredCalls{}, replayChatExecutionError(responsesChatReplayProjectionCode, responsesChatReplayProjectionMessage)
	}
	var projectionMismatch *responsesChatReplayProjectionMismatchError
	if options.ReplayStore != nil {
		resolution, err := resolveResponsesChatReplay(options.ReplayStore, options.ReplayRoute, responsesChatReplayAssistantProjection{Content: projectionContent, Calls: projected})
		if err == nil {
			return responsesChatRestoredCalls{
				Key:         "group:" + strconv.FormatUint(resolution.GroupID, 10),
				OutputItems: resolution.OutputItems,
				Calls:       resolution.Calls,
			}, nil
		}
		if mapped := mapResponsesChatReplayResolveError(err); !isMissingResponsesChatReplayError(mapped) {
			var projection *responsesChatReplayProjectionError
			if !errors.As(err, &projection) {
				return responsesChatRestoredCalls{}, mapped
			}
			projectionMismatch = &responsesChatReplayProjectionMismatchError{error: mapped, diverged: projection.Reason}
		}
	}
	restored, carrier := carriedRestoredCalls(options.CarriedReasoning, projected, options.ReplayRoute, projectionContent)
	if carrier == "" {
		return restored, nil
	}
	// The ID alone is route-agnostic. Do not let it answer during target
	// probing, where another candidate may still hold the full replay group.
	// DegradeUnrestorableReplay is set only after every candidate refused.
	if options.DegradeUnrestorableReplay {
		diverged := "store_missing"
		if projectionMismatch != nil {
			diverged = projectionMismatch.diverged
		}
		if selfDescribed, ok := selfDescribingRestoredCalls(projected, carriedProjectionDigest(projectionContent, projected)); ok {
			tally.record(diverged, carrier, responsesChatReplayProjectionFingerprint(projected, content), len(projected), false)
			return selfDescribed, nil
		}
	}
	if projectionMismatch != nil {
		if options.DegradeUnrestorableReplay {
			return responsesChatRestoredCalls{VisibleTranscriptFallback: &responsesChatVisibleTranscriptFallback{
				diverged: projectionMismatch.diverged,
				carrier:  carrier,
			}}, nil
		}
		return responsesChatRestoredCalls{}, projectionMismatch
	}
	if options.DegradeUnrestorableReplay {
		// Nothing the carrier claimed travels. The caller rebuilds the turn from the
		// transcript, so a refused carrier is discarded rather than merely tolerated.
		return responsesChatRestoredCalls{VisibleTranscriptFallback: &responsesChatVisibleTranscriptFallback{
			diverged: "store_missing",
			carrier:  carrier,
		}}, nil
	}
	// The reason is known here and was previously discarded, so the wedge this whole
	// mechanism exists to prevent arrived with no way to tell WHICH guard rejected it.
	logResponsesChatCarrierWedge(options, projected, carrier)
	return responsesChatRestoredCalls{}, missingResponsesChatReplayError()
}

func appendVisibleAssistantTurn(input []json.RawMessage, calls map[string]string, resultIndices map[string]int, projected []responsesChatReplayProjectedCall, items []json.RawMessage, assistantText string, index int) ([]json.RawMessage, error) {
	if assistantText != "" {
		input = appendAssistantHistoryMessage(input, assistantText)
	}
	matchedResults := 0
	for _, projectedCall := range projected {
		if resultIndex, ok := resultIndices[projectedCall.ID]; ok && resultIndex > index {
			matchedResults++
		}
	}
	if matchedResults == 0 {
		return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "assistant tool calls require at least one subsequent tool result")
	}
	for callIndex, projectedCall := range projected {
		if matchedResults < len(projected) {
			if resultIndex, ok := resultIndices[projectedCall.ID]; !ok || resultIndex <= index {
				continue
			}
		}
		if _, duplicate := calls[projectedCall.ID]; duplicate {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_calls[%d].id", index, callIndex), "duplicate tool call ID")
		}
		calls[projectedCall.ID] = projectedCall.ID
		input = append(input, items[callIndex])
	}
	return input, nil
}

// Candidate routing needs to distinguish a store-owned projection mismatch from a plain
// miss so it can retry the exact target. Direct Anthropic fallback converts this error into
// a successful responsesChatVisibleTranscriptFallback inside restoreResponsesChatCalls.
type responsesChatReplayProjectionMismatchError struct {
	error
	diverged string
}

func (e *responsesChatReplayProjectionMismatchError) Unwrap() error { return e.error }

func isResponsesChatReplayProjectionError(err error) bool {
	var mismatch *responsesChatReplayProjectionMismatchError
	return errors.As(err, &mismatch)
}

func logCarriedReasoningStarved(log *logger.Logger, starved bool, model string) {
	if log == nil || !starved {
		return
	}
	log.Warn("reasoning carrier budget exhausted; newest turns lost reasoning continuity",
		logger.F("model", model),
		logger.F("budget_bytes", reasoningCarrierRequestBudget),
	)
}

// One line per request, not per turn. Measured on a live session: the per-turn form emitted
// 509 warnings in a single request and 15,779 in half an hour, which buries the anomaly the
// reason-logging exists to surface. Counts and enumerated reasons only -- never content or IDs.
type responsesChatRestoreTally struct {
	turns       int
	calls       int
	degraded    int
	selfID      int
	diverged    map[string]int
	carrier     map[string]int
	fingerprint string
	anomalous   bool
}

func (t *responsesChatRestoreTally) record(diverged, carrier, fingerprint string, calls int, degraded bool) {
	if t.diverged == nil {
		t.diverged, t.carrier = map[string]int{}, map[string]int{}
	}
	if diverged == "" {
		diverged = "unknown"
	}
	if carrier == "" {
		carrier = "unknown"
	}
	t.turns++
	t.calls += calls
	t.diverged[diverged]++
	t.carrier[carrier]++
	if degraded {
		t.degraded++
	} else {
		t.selfID++
	}
	if t.fingerprint == "" {
		t.fingerprint = fingerprint
	}
	if degraded || diverged != "store_missing" || carrier != "absent" {
		t.anomalous = true
	}
}

// The scalar keeps the single-turn shape operators and existing alerts already parse; the
// breakdown only appears once a request actually mixed reasons.
func (t *responsesChatRestoreTally) sole(counts map[string]int) string {
	if len(counts) != 1 {
		return "mixed"
	}
	for reason := range counts {
		return reason
	}
	return "unknown"
}

func (t *responsesChatRestoreTally) flush(options responsesChatRequestOptions) {
	if options.Log == nil || t.turns == 0 {
		return
	}
	fields := []logger.Field{
		logger.F("provider", options.ReplayRoute.ProviderID),
		logger.F("model", options.ReplayRoute.PublicModel),
		logger.F("tool_turns", t.turns),
		logger.F("tool_calls", t.calls),
		logger.F("degraded_turns", t.degraded),
		logger.F("self_describing_turns", t.selfID),
		logger.F("diverged", t.sole(t.diverged)),
		logger.F("carrier", t.sole(t.carrier)),
		logger.F("projection", t.fingerprint),
		logger.F("carried_turns", len(options.CarriedReasoning)),
	}
	if len(t.diverged) > 1 {
		fields = append(fields, logger.F("diverged_counts", t.diverged))
	}
	if len(t.carrier) > 1 {
		fields = append(fields, logger.F("carrier_counts", t.carrier))
	}
	// Only the policy path sets a route id; an empty field reads as data loss.
	if routeID := options.ReplayRoute.RouteID; routeID != "" {
		fields = append(fields, logger.F("route_id", routeID))
	}
	if t.anomalous {
		options.Log.Warn("responses replay projection mismatch; continuing without reasoning continuity", fields...)
		return
	}
	options.Log.Info("responses replay resolved from self-describing tool-call IDs", fields...)
}

// Names the guard that rejected the carrier: absent (not in the request at all),
// route (minted under another model), projection (the turn's content moved), shape,
// or binding (name/order/index). Counts and enumerated reasons only -- never content.
func logResponsesChatCarrierWedge(options responsesChatRequestOptions, projected []responsesChatReplayProjectedCall, carrier string) {
	if options.Log == nil {
		return
	}
	if carrier == "" {
		carrier = "unknown"
	}
	options.Log.Warn("responses replay unavailable and the carrier could not answer",
		logger.F("provider", options.ReplayRoute.ProviderID),
		logger.F("model", options.ReplayRoute.PublicModel),
		logger.F("tool_calls", len(projected)),
		logger.F("carrier", carrier),
		logger.F("carried_turns", len(options.CarriedReasoning)),
	)
}

func recordResponsesChatReplayDegrade(tally *responsesChatRestoreTally, projected []responsesChatReplayProjectedCall, content []map[string]any, fallback *responsesChatVisibleTranscriptFallback) {
	tally.record(fallback.diverged, fallback.carrier, responsesChatReplayProjectionFingerprint(projected, content), len(projected), true)
}

// Vekil's own digest, never the projection itself: that is prompt data.
func responsesChatReplayProjectionFingerprint(projected []responsesChatReplayProjectedCall, content []map[string]any) string {
	projectionContent, err := json.Marshal(assistantHistoryText(content))
	if err != nil {
		return ""
	}
	canonical, err := canonicalReplayJSONValue(projectionContent)
	if err != nil {
		return ""
	}
	return carriedProjectionDigest(canonical, projected)
}

func chatToolResultIndices(messages []json.RawMessage) (map[string]int, error) {
	indices := make(map[string]int)
	for index, raw := range messages {
		messageParam := fmt.Sprintf("messages[%d]", index)
		if _, err := validateChatRawObjectFields(raw, messageParam, "role", "content", "refusal", "name", "tool_calls", "tool_call_id"); err != nil {
			return nil, err
		}
		var message struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		}
		if json.Unmarshal(raw, &message) != nil || strings.TrimSpace(message.Role) != "tool" {
			continue
		}
		callID := strings.TrimSpace(message.ToolCallID)
		if callID == "" {
			continue
		}
		if _, duplicate := indices[callID]; duplicate {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "duplicate tool result")
		}
		indices[callID] = index
	}
	return indices, nil
}

func resolveResponsesChatReplay(store *responsesChatReplayStore, route responsesChatReplayRoute, projection responsesChatReplayAssistantProjection) (responsesChatReplayResolution, error) {
	resolution, err := store.Resolve(route, projection)
	var mismatch *responsesChatReplayProjectionError
	if !errors.As(err, &mismatch) || !replayContentIsNullOrEmpty(projection.Content) {
		return resolution, err
	}
	alternate := json.RawMessage(`""`)
	if bytes.Equal(bytes.TrimSpace(projection.Content), []byte(`""`)) {
		alternate = json.RawMessage("null")
	}
	projection.Content = alternate
	return store.Resolve(route, projection)
}

func replayContentIsNullOrEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`))
}

func mapResponsesChatReplayResolveError(err error) error {
	var replayCode interface{ ReplayCode() string }
	if errors.As(err, &replayCode) {
		switch replayCode.ReplayCode() {
		case responsesChatReplayMissingCode:
			return missingResponsesChatReplayError()
		case responsesChatReplayMixedCode:
			return replayChatExecutionError(responsesChatReplayMixedCode, responsesChatReplayMixedMessage)
		case responsesChatReplayProjectionCode:
			return replayChatExecutionError(responsesChatReplayProjectionCode, responsesChatReplayProjectionMessage)
		case responsesChatReplayClosedCode:
			return &chatExecutionError{StatusCode: http.StatusServiceUnavailable, Type: "server_error", Code: responsesChatReplayClosedCode, Param: "messages", Message: responsesChatReplayClosedMessage}
		}
	}
	return replayChatExecutionError(responsesChatReplayProjectionCode, responsesChatReplayProjectionMessage)
}

// Every caller passes a package constant, so the message is vekil's own diagnosis.
func replayChatExecutionError(code, message string) *chatExecutionError {
	return &chatExecutionError{StatusCode: http.StatusBadRequest, Type: "invalid_request_error", Code: code, Param: "messages", Message: message, staticMessage: true}
}

func translateChatAssistantRefusal(raw json.RawMessage, messageIndex int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var refusal string
	if err := json.Unmarshal(raw, &refusal); err != nil {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", messageIndex), "assistant refusal must be a string or null")
	}
	return refusal, nil
}

func assistantHistoryText(content []map[string]any) string {
	var text strings.Builder
	for _, part := range content {
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}

func appendAssistantHistoryMessage(input []json.RawMessage, text string) []json.RawMessage {
	item, _ := json.Marshal(map[string]any{"role": "assistant", "content": text})
	return append(input, item)
}

func responsesFunctionCallItem(callID, name, arguments string) json.RawMessage {
	item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments})
	return item
}

// Only the fixed-width opaque shape and exact self-describing mint output are
// replay IDs. Accepting the prefix plus a broad length range would reroute
// client-owned tool-call IDs onto the Responses path.
func isResponsesChatReplayCallID(id string) bool {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, responsesChatReplayCallIDPrefix) {
		return false
	}
	if len(id) == responsesChatReplayIDLength {
		return isResponsesChatReplayIDCharset(id[len(responsesChatReplayCallIDPrefix):])
	}
	_, ok := responsesChatReplayUpstreamCallID(id)
	return ok
}

type translatedSyntheticChatToolCall struct {
	ID        string
	Name      string
	Arguments string
	Item      json.RawMessage
}

func translateSyntheticChatToolCall(raw json.RawMessage, messageIndex, callIndex int) (translatedSyntheticChatToolCall, error) {
	param := fmt.Sprintf("messages[%d].tool_calls[%d]", messageIndex, callIndex)
	callObject, err := validateChatRawObjectFields(raw, param, "id", "type", "function", "custom")
	if err != nil {
		return translatedSyntheticChatToolCall{}, err
	}
	var header struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param, "tool call must be an object")
	}
	callID := strings.TrimSpace(header.ID)
	if callID == "" {
		return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".id", "tool call ID is required")
	}
	callType := strings.TrimSpace(header.Type)
	if callType == "" {
		callType = "function"
	}

	name := ""
	arguments := ""
	switch callType {
	case "function":
		if rawCustom, ok := callObject["custom"]; ok && !bytes.Equal(bytes.TrimSpace(rawCustom), []byte("null")) {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".custom", "custom is not valid for a function tool call")
		}
		functionRaw, ok := callObject["function"]
		if !ok {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".function", "function is required")
		}
		functionObject, err := validateChatRawObjectFields(functionRaw, param+".function", "name", "arguments")
		if err != nil {
			return translatedSyntheticChatToolCall{}, err
		}
		rawArguments, ok := functionObject["arguments"]
		if !ok {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".function.arguments", "function arguments string is required")
		}
		if bytes.Equal(bytes.TrimSpace(rawArguments), []byte("null")) || json.Unmarshal(rawArguments, &arguments) != nil {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".function.arguments", "function arguments must be a string")
		}
		if rawName, ok := functionObject["name"]; !ok || json.Unmarshal(rawName, &name) != nil || strings.TrimSpace(name) == "" {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".function.name", "function name is required")
		}
		name = strings.TrimSpace(name)
	case "custom":
		if !isResponsesChatReplayCallID(callID) {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".type", "custom tool-call history is supported only for Responses replay IDs")
		}
		if rawFunction, ok := callObject["function"]; ok && !bytes.Equal(bytes.TrimSpace(rawFunction), []byte("null")) {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".function", "function is not valid for a custom tool call")
		}
		customRaw, ok := callObject["custom"]
		if !ok {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".custom", "custom is required")
		}
		customObject, err := validateChatRawObjectFields(customRaw, param+".custom", "name", "input")
		if err != nil {
			return translatedSyntheticChatToolCall{}, err
		}
		var input string
		if rawName, ok := customObject["name"]; !ok || json.Unmarshal(rawName, &name) != nil || strings.TrimSpace(name) == "" {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".custom.name", "custom tool name is required")
		}
		if rawInput, ok := customObject["input"]; !ok || bytes.Equal(bytes.TrimSpace(rawInput), []byte("null")) || json.Unmarshal(rawInput, &input) != nil {
			return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".custom.input", "custom tool input must be a string")
		}
		name = strings.TrimSpace(name)
		encodedInput, _ := json.Marshal(map[string]string{"input": input})
		arguments = string(encodedInput)
	default:
		return translatedSyntheticChatToolCall{}, newChatInvalidRequest(param+".type", "only function tool calls and replay-backed custom tool calls are supported")
	}

	return translatedSyntheticChatToolCall{
		ID: callID, Name: name, Arguments: arguments, Item: responsesFunctionCallItem(callID, name, arguments),
	}, nil
}

func compactChatToolOutput(raw json.RawMessage, messageIndex int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "tool output is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var parts []json.RawMessage
		if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
			looksLikeContentParts := true
			for _, partRaw := range parts {
				var probe struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(partRaw, &probe) != nil || probe.Type != "text" {
					looksLikeContentParts = false
					break
				}
			}
			if looksLikeContentParts {
				var output strings.Builder
				for partIndex, partRaw := range parts {
					param := fmt.Sprintf("messages[%d].content[%d]", messageIndex, partIndex)
					if _, err := validateChatRawObjectFields(partRaw, param, "type", "text"); err != nil {
						return "", err
					}
					var part struct {
						Type string  `json:"type"`
						Text *string `json:"text"`
					}
					if err := json.Unmarshal(partRaw, &part); err != nil || part.Type != "text" || part.Text == nil {
						return "", newChatInvalidRequest(param, "tool message content supports text parts only")
					}
					output.WriteString(*part.Text)
				}
				return output.String(), nil
			}
		}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "tool output must be valid JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "tool output must be valid JSON")
	}
	return string(encoded), nil
}

func missingResponsesChatReplayError() *chatExecutionError {
	return &chatExecutionError{
		StatusCode:    http.StatusBadRequest,
		Type:          "invalid_request_error",
		Code:          "responses_replay_state_missing",
		Param:         "messages",
		Message:       "Responses-backed tool state is no longer available; restart the assistant tool-call turn.",
		staticMessage: true,
	}
}

func translateChatMessageContent(raw json.RawMessage, role string, messageIndex int) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return []map[string]any{{"type": contentType, "text": text}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "message content must be a string or array")
	}
	translated := make([]map[string]any, 0, len(parts))
	for partIndex, partRaw := range parts {
		var part struct {
			Type     string  `json:"type"`
			Text     *string `json:"text,omitempty"`
			ImageURL *struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			} `json:"image_url,omitempty"`
		}
		param := fmt.Sprintf("messages[%d].content[%d]", messageIndex, partIndex)
		partObject, validationErr := validateChatRawObjectFields(partRaw, param, "type", "text", "image_url")
		if validationErr != nil {
			return nil, validationErr
		}
		if rawImage, ok := partObject["image_url"]; ok && len(bytes.TrimSpace(rawImage)) > 0 && !bytes.Equal(bytes.TrimSpace(rawImage), []byte("null")) {
			if _, err := validateChatRawObjectFields(rawImage, param+".image_url", "url", "detail"); err != nil {
				return nil, err
			}
		}
		if err := json.Unmarshal(partRaw, &part); err != nil {
			return nil, newChatInvalidRequest(param, "content part must be an object")
		}
		switch strings.TrimSpace(part.Type) {
		case "text":
			if part.ImageURL != nil {
				return nil, newChatInvalidRequest(param+".image_url", "image_url is not valid for a text content part")
			}
			if part.Text == nil {
				return nil, newChatInvalidRequest(param+".text", "text is required")
			}
			contentType := "input_text"
			if role == "assistant" {
				contentType = "output_text"
			}
			translated = append(translated, map[string]any{"type": contentType, "text": *part.Text})
		case "image_url":
			if part.Text != nil {
				return nil, newChatInvalidRequest(param+".text", "text is not valid for an image content part")
			}
			if role != "user" {
				return nil, newChatInvalidRequest(param, "image content is supported only in user messages")
			}
			if part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
				return nil, newChatInvalidRequest(param+".image_url.url", "image URL is required")
			}
			imageURL := strings.TrimSpace(part.ImageURL.URL)
			if !validResponsesChatImageURL(imageURL) {
				return nil, newChatInvalidRequest(param+".image_url.url", "image URL must be HTTP(S) or a base64 image data URL")
			}
			image := map[string]any{"type": "input_image", "image_url": imageURL}
			if detail := strings.TrimSpace(part.ImageURL.Detail); detail != "" {
				switch detail {
				case "auto", "low", "high", "original":
					image["detail"] = detail
				default:
					return nil, newChatInvalidRequest(param+".image_url.detail", "unsupported image detail")
				}
			}
			translated = append(translated, image)
		default:
			return nil, newChatInvalidRequest(param+".type", "unsupported content part type")
		}
	}
	return translated, nil
}

func validResponsesChatImageURL(raw string) bool {
	if strings.HasPrefix(raw, "data:image/") {
		comma := strings.IndexByte(raw, ',')
		if comma <= len("data:image/") || !strings.HasSuffix(raw[:comma], ";base64") {
			return false
		}
		_, err := base64.StdEncoding.DecodeString(raw[comma+1:])
		return err == nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func parseChatStreamOptions(raw map[string]json.RawMessage) (bool, error) {
	value, ok := raw["stream_options"]
	if !ok || len(bytes.TrimSpace(value)) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return false, nil
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(value, &options); err != nil {
		return false, newChatInvalidRequest("stream_options", "stream_options must be an object")
	}
	for field := range options {
		if field != "include_usage" {
			return false, newChatInvalidRequestClientField("stream_options", field, "unsupported stream option")
		}
	}
	include := false
	if rawInclude, ok := options["include_usage"]; ok {
		if err := json.Unmarshal(rawInclude, &include); err != nil {
			return false, newChatInvalidRequest("stream_options.include_usage", "include_usage must be a boolean")
		}
	}
	return include, nil
}

func translateChatTools(raw json.RawMessage) ([]json.RawMessage, map[string]struct{}, responsesChatReplayToolDefaults, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, nil, nil, newChatInvalidRequest("tools", "tools must be an array")
	}
	translated := make([]json.RawMessage, 0, len(tools))
	names := make(map[string]struct{}, len(tools))
	toolDefaults := make(responsesChatReplayToolDefaults)
	for i, toolRaw := range tools {
		toolParam := fmt.Sprintf("tools[%d]", i)
		toolObject, err := validateChatRawObjectFields(toolRaw, toolParam, "type", "function")
		if err != nil {
			return nil, nil, nil, err
		}
		var functionObject map[string]json.RawMessage
		if functionRaw, ok := toolObject["function"]; ok {
			functionObject, err = validateChatRawObjectFields(functionRaw, toolParam+".function", "name", "description", "parameters", "strict")
			if err != nil {
				return nil, nil, nil, err
			}
		}
		strict := false
		if strictRaw, ok := functionObject["strict"]; ok && !bytes.Equal(bytes.TrimSpace(strictRaw), []byte("null")) {
			if err := json.Unmarshal(strictRaw, &strict); err != nil {
				return nil, nil, nil, newChatInvalidRequest(toolParam+".function.strict", "strict must be a boolean")
			}
		}
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			} `json:"function"`
		}
		if err := json.Unmarshal(toolRaw, &tool); err != nil {
			return nil, nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d]", i), "tool must be an object")
		}
		if strings.TrimSpace(tool.Type) != "function" {
			return nil, nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].type", i), "only function tools are supported")
		}
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			return nil, nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].function.name", i), "function name is required")
		}
		if _, duplicate := names[name]; duplicate {
			return nil, nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].function.name", i), "function names must be unique")
		}
		names[name] = struct{}{}
		parameters := tool.Function.Parameters
		if len(bytes.TrimSpace(parameters)) == 0 || bytes.Equal(bytes.TrimSpace(parameters), []byte("null")) {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		} else {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(parameters, &schema); err != nil || schema == nil {
				return nil, nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].function.parameters", i), "function parameters must be a JSON object")
			}
		}
		if defaults := replayOptionalDefaultsFromJSONSchema(parameters); len(defaults) > 0 {
			toolDefaults[name] = defaults
		}
		flattened := map[string]any{
			"type":       "function",
			"name":       name,
			"parameters": parameters,
			"strict":     strict,
		}
		if tool.Function.Description != "" {
			flattened["description"] = tool.Function.Description
		}
		encoded, err := json.Marshal(flattened)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("marshal function tool: %w", err)
		}
		translated = append(translated, encoded)
	}
	if len(toolDefaults) == 0 {
		toolDefaults = nil
	}
	return translated, names, toolDefaults, nil
}

func replayOptionalDefaultsFromJSONSchema(parameters json.RawMessage) responsesChatReplayOptionalDefaults {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Properties) == 0 {
		return nil
	}
	required := make(map[string]struct{}, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = struct{}{}
	}
	defaults := make(responsesChatReplayOptionalDefaults)
	for name, propertyRaw := range schema.Properties {
		if _, isRequired := required[name]; isRequired {
			continue
		}
		var property map[string]json.RawMessage
		if json.Unmarshal(propertyRaw, &property) != nil {
			continue
		}
		defaultRaw, ok := property["default"]
		if !ok {
			continue
		}
		canonical, err := canonicalReplayJSONValue(defaultRaw)
		if err == nil {
			defaults[name] = cloneReplayRawMessage(canonical)
		}
	}
	if len(defaults) == 0 {
		return nil
	}
	return defaults
}

func translateChatToolChoice(raw json.RawMessage, toolNames map[string]struct{}) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		switch choice {
		case "none", "auto":
			return json.Marshal(choice)
		case "required":
			if len(toolNames) == 0 {
				return nil, newChatInvalidRequest("tool_choice", "required tool_choice needs declared tools")
			}
			return json.Marshal(choice)
		default:
			return nil, newChatInvalidRequest("tool_choice", "unsupported tool_choice")
		}
	}
	choiceObject, validationErr := validateChatRawObjectFields(raw, "tool_choice", "type", "function")
	if validationErr != nil {
		return nil, validationErr
	}
	if functionRaw, ok := choiceObject["function"]; ok {
		if _, err := validateChatRawObjectFields(functionRaw, "tool_choice.function", "name"); err != nil {
			return nil, err
		}
	}
	var object struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || object.Type != "function" || strings.TrimSpace(object.Function.Name) == "" {
		return nil, newChatInvalidRequest("tool_choice", "tool_choice must be none, auto, required, or a named function")
	}
	name := strings.TrimSpace(object.Function.Name)
	if _, ok := toolNames[name]; !ok {
		return nil, newChatInvalidRequest("tool_choice", "named tool_choice must reference a declared function")
	}
	return json.Marshal(map[string]string{"type": "function", "name": name})
}

func translateChatTextConfiguration(raw map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	text := make(map[string]json.RawMessage, 2)
	if formatRaw, ok := raw["response_format"]; ok && len(bytes.TrimSpace(formatRaw)) > 0 && !bytes.Equal(bytes.TrimSpace(formatRaw), []byte("null")) {
		format, err := translateChatResponseFormat(formatRaw)
		if err != nil {
			return nil, err
		}
		text["format"] = format
	}
	if verbosityRaw, ok := raw["verbosity"]; ok && !bytes.Equal(bytes.TrimSpace(verbosityRaw), []byte("null")) {
		var verbosity string
		if err := json.Unmarshal(verbosityRaw, &verbosity); err != nil {
			return nil, newChatInvalidRequest("verbosity", "verbosity must be a string")
		}
		switch verbosity {
		case "low", "medium", "high":
		default:
			return nil, newChatInvalidRequest("verbosity", "unsupported verbosity")
		}
		text["verbosity"] = json.RawMessage(strconvQuote(verbosity))
	}
	if len(text) == 0 {
		return nil, nil
	}
	return text, nil
}

func translateChatResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	formatObject, err := validateChatRawObjectFields(raw, "response_format", "type", "json_schema")
	if err != nil {
		return nil, err
	}
	var format struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, newChatInvalidRequest("response_format", "response_format must be an object")
	}
	switch format.Type {
	case "text", "json_object":
		return json.Marshal(map[string]string{"type": format.Type})
	case "json_schema":
		var schema struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if _, err := validateChatRawObjectFields(formatObject["json_schema"], "response_format.json_schema", "name", "description", "schema", "strict"); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(format.JSONSchema, &schema); err != nil {
			return nil, newChatInvalidRequest("response_format.json_schema", "json_schema must be an object")
		}
		if strings.TrimSpace(schema.Name) == "" {
			return nil, newChatInvalidRequest("response_format.json_schema.name", "json schema name is required")
		}
		var schemaObject map[string]json.RawMessage
		if err := json.Unmarshal(schema.Schema, &schemaObject); err != nil || schemaObject == nil {
			return nil, newChatInvalidRequest("response_format.json_schema.schema", "json schema must be an object")
		}
		flattened := map[string]any{"type": "json_schema", "name": strings.TrimSpace(schema.Name), "schema": schema.Schema}
		if schema.Description != "" {
			flattened["description"] = schema.Description
		}
		if schema.Strict != nil {
			flattened["strict"] = *schema.Strict
		}
		return json.Marshal(flattened)
	default:
		return nil, newChatInvalidRequest("response_format.type", "unsupported response_format type")
	}
}

func translateChatReasoningConfiguration(raw map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	value, ok := raw["reasoning_effort"]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var effort string
	if err := json.Unmarshal(value, &effort); err != nil || strings.TrimSpace(effort) == "" {
		return nil, newChatInvalidRequest("reasoning_effort", "reasoning_effort must be a non-empty string")
	}
	return map[string]json.RawMessage{"effort": json.RawMessage(strconvQuote(strings.TrimSpace(effort)))}, nil
}

func optionalJSONObject(raw map[string]json.RawMessage, field string) (map[string]json.RawMessage, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(value, &parsed); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be an object")
	}
	return parsed, nil
}

func optionalJSONBool(raw map[string]json.RawMessage, field string) (*bool, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var parsed bool
	if err := json.Unmarshal(value, &parsed); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be a boolean")
	}
	return &parsed, nil
}

func optionalJSONString(raw map[string]json.RawMessage, field string) (*string, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be a string")
	}
	return &parsed, nil
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func validateChatResponsesTopLevel(raw map[string]json.RawMessage) error {
	allowed := map[string]struct{}{
		"model": {}, "messages": {}, "stream": {}, "stream_options": {},
		"temperature": {}, "top_p": {}, "max_tokens": {}, "max_completion_tokens": {},
		"tools": {}, "tool_choice": {}, "parallel_tool_calls": {},
		"response_format": {}, "reasoning_effort": {}, "verbosity": {},
		"metadata": {}, "store": {}, "user": {}, "prompt_cache_key": {}, "safety_identifier": {},
		"stop": {}, "n": {},
	}
	unknown := make([]string, 0)
	for field := range raw {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		field := unknown[0]
		// field is the CLIENT's own unknown key, not a name vekil chose; the top level is its
		// only trusted parent and there is nothing above that, so nothing of it is loggable.
		return newChatInvalidRequestClientField("", field, field+" is not supported for Responses-backed Chat completions")
	}
	if stop, ok := raw["stop"]; ok {
		empty, err := emptyChatStop(stop)
		if err != nil {
			return err
		}
		if !empty {
			return newChatInvalidRequest("stop", "stop is not supported for Responses-backed Chat completions")
		}
	}
	if value, ok := raw["n"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var n int
		if err := json.Unmarshal(value, &n); err != nil || n != 1 {
			return newChatInvalidRequest("n", "n must be 1 for Responses-backed Chat completions")
		}
	}
	return nil
}

func emptyChatStop(raw json.RawMessage) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == "", nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err == nil {
		return len(multiple) == 0, nil
	}
	return false, newChatInvalidRequest("stop", "stop must be a string or array of strings")
}

func decodeChatJSONObject(body []byte, param string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return nil, newChatInvalidRequest(param, "invalid JSON in request body")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, newChatInvalidRequest(param, "request body must be a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, newChatInvalidRequest(param, "invalid JSON object")
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, newChatInvalidRequest(param, "invalid JSON object key")
		}
		if _, duplicate := object[key]; duplicate {
			return nil, newChatInvalidRequestClientField(param, key, "duplicate JSON field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, newChatInvalidRequest(param, "invalid JSON object value")
		}
		object[key] = append(json.RawMessage(nil), value...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, newChatInvalidRequest(param, "invalid JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, newChatInvalidRequest(param, "request body must contain one JSON object")
	}
	return object, nil
}

func validateChatRawObjectFields(raw json.RawMessage, param string, allowedFields ...string) (map[string]json.RawMessage, error) {
	object, err := decodeChatJSONObject(raw, param)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(allowedFields))
	for _, field := range allowedFields {
		allowed[field] = struct{}{}
	}
	unknown := make([]string, 0)
	for field := range object {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, newChatInvalidRequestClientField(param, unknown[0], "unsupported JSON field")
	}
	return object, nil
}
