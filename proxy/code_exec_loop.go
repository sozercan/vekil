package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

// Sentinel errors for the proxy-mediated code execution loop. They are used to
// fail closed: the loop must never forward an owned tool call to the client, so
// any unrecoverable state returns an error and the handler surfaces it instead
// of leaking a tool call the client might re-execute.
var (
	// errCodeExecMixedTools is returned when a model response mixes owned and
	// unowned tool calls. Mixed ownership is a non-goal for the MVP; forwarding
	// the response would leak the owned call, so the loop fails closed.
	errCodeExecMixedTools = errors.New("code exec: response mixes owned and unowned tool calls")
	// errCodeExecMaxDepth is returned when the internal loop reaches its depth
	// limit while the model is still emitting owned tool calls.
	errCodeExecMaxDepth = errors.New("code exec: internal loop exceeded max depth")
	// errCodeExecNoBackend is returned when the feature is active but no backend
	// is wired in.
	errCodeExecNoBackend = errors.New("code exec: no execution backend configured")
	// errCodeExecMissingCommand is returned when an owned tool call carries no
	// extractable command argument.
	errCodeExecMissingCommand = errors.New("code exec: owned tool call has no command argument")
	// errCodeExecMultiChoice is returned when a response carrying an owned tool
	// call has more than one choice. The internal loop models a single assistant
	// message per turn, so an n>1 response cannot be represented without dropping
	// or duplicating choices; forwarding it could leak an owned call sitting in a
	// choice the loop does not process, so the loop fails closed.
	errCodeExecMultiChoice = errors.New("code exec: owned tool call in multi-choice (n>1) response")
)

// codeExecActive reports whether proxy-mediated code execution is enabled and
// fully wired (config active and a backend present).
func (h *ProxyHandler) codeExecActive() bool {
	return h != nil && h.codeExecConfig.active() && h.codeExecBackend != nil
}

// initializeCodeExec finalizes the code-execution config (env overrides then
// defaults) and selects a backend when the feature is enabled and none was
// injected via WithCodeExecBackend. Selection failures disable the feature with
// a warning rather than aborting startup, so a misconfigured optional feature
// never takes down the proxy.
func (h *ProxyHandler) initializeCodeExec() {
	if h == nil {
		return
	}
	cfg := h.codeExecConfig.withEnvOverrides().withDefaults()
	h.codeExecConfig = cfg
	if !cfg.active() {
		return
	}
	if h.codeExecBackend == nil {
		backend, err := newCodeExecBackend(cfg.Backend)
		if err != nil {
			if h.log != nil {
				h.log.Error("code exec disabled: backend unavailable",
					logger.F("backend", cfg.Backend), logger.Err(err))
			}
			h.codeExecConfig.Enabled = false
			return
		}
		h.codeExecBackend = backend
	}
	if h.log != nil {
		h.log.Info("proxy-mediated code execution enabled",
			logger.F("backend", h.codeExecBackend.Name()),
			logger.F("owned_tools", strings.Join(cfg.OwnedTools, ",")),
			logger.F("working_dir", cfg.Policy.WorkingDir),
		)
	}
}

// newCodeExecBackend constructs a backend by name. Only the local process
// backend is available today; the switch is the extension point where container
// or remote backends attach without touching the loop.
func newCodeExecBackend(name string) (CodeExecutionBackend, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", defaultCodeExecBackend:
		return NewLocalProcessBackend(), nil
	default:
		return nil, fmt.Errorf("unknown code exec backend %q", name)
	}
}

// responseHasOwnedToolCall reports whether resp contains a tool call for a
// configured owned tool in any of its choices. This is the gate the chat
// handler uses to decide whether to divert a buffered response into the
// execution loop. Every choice is inspected — not just Choices[0] — so an owned
// call in a secondary choice of a multi-choice (n>1) response cannot slip past
// the gate and be forwarded to the client.
func (h *ProxyHandler) responseHasOwnedToolCall(resp *models.OpenAIResponse) bool {
	if !h.codeExecActive() || resp == nil {
		return false
	}
	owned, _ := classifyOwnedToolCalls(resp, h.codeExecConfig)
	return len(owned) > 0
}

// maybeInterceptCodeExec runs the proxy-mediated code execution loop when the
// feature is active and the buffered response contains an owned tool call. It
// returns the final response to serialize to the client — which never contains
// an owned tool call — or an error if the internal loop failed and the handler
// must fail closed rather than leak an owned tool call. When the feature is
// inactive or the response carries no owned tool call, it returns the input
// response unchanged so existing transparent behavior is preserved.
func (h *ProxyHandler) maybeInterceptCodeExec(ctx context.Context, requestBody []byte, oaiResp *models.OpenAIResponse) (*models.OpenAIResponse, error) {
	if !h.responseHasOwnedToolCall(oaiResp) {
		return oaiResp, nil
	}
	return h.runCodeExecLoop(ctx, requestBody, oaiResp)
}

// classifyOwnedToolCalls splits the tool calls across every choice into owned
// and unowned sets. All choices are inspected — not just Choices[0] — so an
// owned tool call in a secondary choice of a multi-choice (n>1) response is
// never overlooked. Overlooking it would let the gate report "no owned call"
// and forward the owned call to the client, violating the core invariant. The
// loop still fails closed on n>1 before executing (see runCodeExecLoop); this
// function's job is detection, so it must see every call.
func classifyOwnedToolCalls(resp *models.OpenAIResponse, cfg CodeExecConfig) (owned, unowned []models.OpenAIToolCall) {
	if resp == nil {
		return nil, nil
	}
	for i := range resp.Choices {
		for _, call := range resp.Choices[i].Message.ToolCalls {
			if cfg.ownsTool(call.Function.Name) {
				owned = append(owned, call)
			} else {
				unowned = append(unowned, call)
			}
		}
	}
	return owned, unowned
}

// runCodeExecLoop drives the internal model/tool loop for proxy-mediated code
// execution. It starts from the initial buffered upstream response, executes any
// owned tool calls through the configured backend, feeds structured tool results
// back to the model over an internal (client-invisible) conversation, and
// repeats until the model returns a response with no owned tool calls. Only that
// final response is returned to the caller.
//
// requestBody is the OpenAI chat-completions request body that produced the
// initial response; the loop reuses its model, tools, and sampling fields when
// constructing follow-up turns. The returned response never contains an owned
// tool call — the loop fails closed (error) rather than surface one.
func (h *ProxyHandler) runCodeExecLoop(ctx context.Context, requestBody []byte, initial *models.OpenAIResponse) (*models.OpenAIResponse, error) {
	if h.codeExecBackend == nil {
		return nil, errCodeExecNoBackend
	}

	cfg := h.codeExecConfig
	policy := cfg.Policy.toPolicy()

	baseFields, messages, err := decodeChatRequestForLoop(requestBody)
	if err != nil {
		return nil, fmt.Errorf("code exec: decode request: %w", err)
	}

	current := initial
	maxDepth := cfg.MaxLoopDepth
	if maxDepth <= 0 {
		maxDepth = defaultCodeExecMaxLoopDepth
	}

	for depth := 0; depth < maxDepth; depth++ {
		owned, unowned := classifyOwnedToolCalls(current, cfg)
		if len(owned) == 0 {
			// No owned calls remain: this is the client-visible final response.
			// (Any unowned tool calls are forwarded unchanged as normal.)
			return current, nil
		}
		if len(current.Choices) > 1 {
			// An owned call in a multi-choice (n>1) response cannot be handled by
			// the single-assistant-message internal loop without dropping or
			// duplicating choices. Forwarding it could leak an owned call in a
			// choice the loop does not process, so fail closed.
			return nil, errCodeExecMultiChoice
		}
		if len(unowned) > 0 {
			// Mixed ownership is unsupported for the MVP. Forwarding would leak the
			// owned call, so fail closed.
			return nil, errCodeExecMixedTools
		}

		// This turn is consumed internally: its response never reaches the client,
		// so its usage would otherwise be dropped from metrics/billing. Account it
		// as additive internal usage. This covers the initial turn (first iteration)
		// and every intermediate turn; the final, client-visible turn is metered
		// normally by the handler via observeOpenAIUsage.
		observeInternalOpenAIUsage(ctx, current.Usage)

		assistantMessage := current.Choices[0].Message
		messages = append(messages, assistantMessage)

		for _, call := range owned {
			toolResult, execErr := h.executeOwnedToolCall(ctx, call, policy)
			if execErr != nil {
				return nil, execErr
			}
			messages = append(messages, toolResult)
		}

		next, err := h.resendChatCompletionsForLoop(ctx, baseFields, messages)
		if err != nil {
			return nil, err
		}
		current = next
	}

	return nil, errCodeExecMaxDepth
}

// executeOwnedToolCall extracts the command from an owned tool call, runs it
// through the backend under the given policy, and returns an OpenAI tool-result
// message carrying the structured result. Audit logging records each command
// before execution.
//
// Backend execution is detached from the inbound request context with
// context.WithoutCancel so a client disconnect does not cancel a command
// mid-loop. This matches the follow-up upstream turns (which run under a
// background-rooted context via newInferenceUpstreamContextFrom): both halves of
// an internal turn now complete regardless of client disconnect rather than the
// command being killed while the upstream call would have continued. The
// per-command policy timeout still bounds execution — the backend applies it
// internally — so detaching does not remove the wall-clock limit.
func (h *ProxyHandler) executeOwnedToolCall(ctx context.Context, call models.OpenAIToolCall, policy CodeExecPolicy) (models.OpenAIMessage, error) {
	command, ok := extractCommandArgument(call.Function.Arguments)
	if !ok {
		return models.OpenAIMessage{}, errCodeExecMissingCommand
	}

	if h.log != nil {
		h.log.Info("code exec: executing owned tool call",
			logger.F("tool", call.Function.Name),
			logger.F("tool_use_id", call.ID),
			logger.F("backend", h.codeExecBackend.Name()),
			logger.F("command", command),
		)
	}

	execCtx := context.WithoutCancel(ctx)
	result, err := h.codeExecBackend.RunCommand(execCtx, CodeExecRequest{
		ToolUseID: call.ID,
		ToolName:  call.Function.Name,
		Command:   command,
		Policy:    policy,
	})
	if err != nil {
		return models.OpenAIMessage{}, fmt.Errorf("code exec: backend run: %w", err)
	}
	result.ToolUseID = call.ID

	if h.log != nil {
		h.log.Info("code exec: owned tool call complete",
			logger.F("tool_use_id", call.ID),
			logger.F("exit_code", result.ExitCode),
			logger.F("timed_out", result.TimedOut),
			logger.F("duration_ms", result.DurationMS),
		)
	}

	return buildToolResultMessage(call.ID, result)
}

// extractCommandArgument pulls the shell command out of a tool call's JSON
// arguments. Anthropic's Bash tool and the issue's examples use the "command"
// field; "cmd" and "script" are accepted as tolerant fallbacks.
func extractCommandArgument(arguments string) (string, bool) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &fields); err != nil {
		return "", false
	}
	for _, key := range []string{"command", "cmd", "script"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if strings.TrimSpace(value) != "" {
			return value, true
		}
	}
	return "", false
}

// buildToolResultMessage serializes a structured execution result into an
// OpenAI "tool" role message that answers the given tool call. The content is a
// JSON string so the model receives command, exit code, stdout, stderr,
// duration, and timeout status.
func buildToolResultMessage(toolCallID string, result CodeExecResult) (models.OpenAIMessage, error) {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return models.OpenAIMessage{}, fmt.Errorf("code exec: marshal result: %w", err)
	}
	contentValue, err := json.Marshal(string(resultJSON))
	if err != nil {
		return models.OpenAIMessage{}, fmt.Errorf("code exec: marshal result content: %w", err)
	}
	return models.OpenAIMessage{
		Role:       "tool",
		ToolCallID: toolCallID,
		Content:    json.RawMessage(contentValue),
	}, nil
}

// decodeChatRequestForLoop splits a chat-completions request body into its
// non-message fields (kept raw so model/tools/sampling survive round-trips) and
// the decoded message list the loop extends.
func decodeChatRequestForLoop(body []byte) (map[string]json.RawMessage, []models.OpenAIMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, nil, err
	}
	var messages []models.OpenAIMessage
	if raw, ok := fields["messages"]; ok {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, nil, fmt.Errorf("decode messages: %w", err)
		}
	}
	return fields, messages, nil
}

// resendChatCompletionsForLoop rebuilds the request with the extended message
// list, forces upstream streaming for reliable tool-call aggregation, sends it,
// and aggregates the streamed response back into a buffered OpenAIResponse. Each
// call runs under its own background-rooted upstream context (bounded by the
// streaming timeout) so a client disconnect does not cancel an in-flight
// internal turn, matching the primary buffered chat path.
func (h *ProxyHandler) resendChatCompletionsForLoop(ctx context.Context, baseFields map[string]json.RawMessage, messages []models.OpenAIMessage) (*models.OpenAIResponse, error) {
	body, err := encodeChatRequestForLoop(baseFields, messages)
	if err != nil {
		return nil, fmt.Errorf("code exec: encode follow-up request: %w", err)
	}
	// Force streaming upstream so parallel tool calls aggregate reliably, matching
	// the primary buffered chat path.
	body = injectForceStream(body)

	upstreamCtx, cancel := h.newInferenceUpstreamContextFrom(ctx, true)
	defer cancel()

	resp, err := h.postChatCompletions(upstreamCtx, body)
	if err != nil {
		return nil, fmt.Errorf("code exec: follow-up upstream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("code exec: follow-up upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	oaiResp, err := aggregateStreamToResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("code exec: aggregate follow-up response: %w", err)
	}
	return oaiResp, nil
}

// encodeChatRequestForLoop re-marshals the base request fields with the updated
// message list, dropping stream bookkeeping so injectForceStream can set it
// cleanly.
func encodeChatRequestForLoop(baseFields map[string]json.RawMessage, messages []models.OpenAIMessage) ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(baseFields)+1)
	for key, value := range baseFields {
		fields[key] = value
	}
	encodedMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	fields["messages"] = encodedMessages
	// Remove any prior streaming markers; the follow-up sets its own.
	delete(fields, "stream")
	delete(fields, "stream_options")
	return json.Marshal(fields)
}
