package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/sozercan/vekil/models"
)

type geminiStreamingToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Emitted   bool
}

func writeGeminiSSEData(w http.ResponseWriter, data interface{}) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return err
	}

	payload := bytes.TrimRight(buf.Bytes(), "\n")
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	return nil
}

// StreamOpenAIToGemini translates upstream OpenAI SSE into Gemini-style
// data-only SSE frames.
func StreamOpenAIToGemini(w http.ResponseWriter, body io.ReadCloser) {
	StreamOpenAIToGeminiWithFinalResponse(w, body, nil, nil)
}

// StreamOpenAIToGeminiWithFinalResponse translates upstream OpenAI SSE into
// Gemini-style data-only SSE frames and optionally invokes onFinalResponse with
// the complete aggregated OpenAI response after the translated stream finishes
// successfully. onError, when non-nil, is invoked if the upstream stream errors
// or ends before [DONE] after the SSE headers were committed, so the request can
// be recorded as a failure even though its HTTP status was sent as 200.
func StreamOpenAIToGeminiWithFinalResponse(
	w http.ResponseWriter,
	body io.ReadCloser,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	streamOpenAIToGeminiWithLifecycle(w, body, onError, onFinalResponse, streamLifecycleHooks{}, onUsageCallbacks...)
}

func streamOpenAIToGeminiWithLifecycle(
	w http.ResponseWriter,
	body io.ReadCloser,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	lifecycle streamLifecycleHooks,
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	defer func() { _ = body.Close() }()
	trackedWriter := &commitTrackingResponseWriter{ResponseWriter: w}
	w = trackedWriter
	setSSEHeaders(w)

	state := newGeminiStreamState(w)
	var aggregator *openAIResponseAggregator
	if onFinalResponse != nil {
		aggregator = newOpenAIResponseAggregator()
	}
	onUsage := firstOpenAIUsageCallback(onUsageCallbacks)

	sawDone, err := consumeOpenAIStreamChunks(body, func(chunk models.OpenAIStreamChunk) bool {
		if onUsage != nil && chunk.Usage != nil {
			onUsage(chunk.Usage)
		}
		if !state.consumeChunk(chunk) {
			return false
		}
		if aggregator != nil {
			aggregator.addChunk(chunk)
		}
		return true
	})
	if state.upstreamProtocolError != nil {
		if onError != nil && !state.clientWriteFailed {
			onError(http.StatusBadGateway)
		}
		state.writeError(http.StatusBadGateway, state.upstreamProtocolError.Error())
		return
	}
	if err != nil {
		var streamErr *openAIStreamError
		if errors.As(err, &streamErr) {
			status := streamErr.httpStatus()
			if onError != nil && !state.clientWriteFailed {
				onError(status)
			}
			state.writeError(status, streamErr.Error())
			return
		}
		if lifecycle.suppressTransportCancellation(trackedWriter.committed) {
			if trackedWriter.committed && !state.clientWriteFailed {
				state.writeError(http.StatusServiceUnavailable, "server shutting down")
			}
			return
		}
		if onError != nil && !state.clientWriteFailed {
			onError(http.StatusBadGateway)
		}
		state.writeError(http.StatusBadGateway, fmt.Sprintf("upstream stream read failed: %v", err))
		return
	}
	if !sawDone {
		if lifecycle.suppressTransportCancellation(trackedWriter.committed) {
			if trackedWriter.committed && !state.clientWriteFailed {
				state.writeError(http.StatusServiceUnavailable, "server shutting down")
			}
			return
		}
		if onError != nil && !state.clientWriteFailed {
			onError(http.StatusBadGateway)
		}
		state.writeError(http.StatusBadGateway, "upstream stream ended before [DONE]")
		return
	}

	if !state.finish() || onFinalResponse == nil {
		return
	}
	onFinalResponse(aggregator.buildResponse())
}

func aggregateGeminiStreamToResponseWithProgress(body io.ReadCloser) (*models.OpenAIResponse, upstreamSemanticProgress, error) {
	defer func() { _ = body.Close() }()

	aggregator := newOpenAIResponseAggregator()
	var protocolErr error
	sawDone, progress, err := consumeOpenAIStreamChunksWithProgress(body, func(chunk models.OpenAIStreamChunk) bool {
		for _, choice := range chunk.Choices {
			if err := validateGeminiToolCallIndexes(choice.Delta.ToolCalls); err != nil {
				protocolErr = err
				return false
			}
		}
		aggregator.addChunk(chunk)
		return true
	})
	if protocolErr != nil {
		return nil, progress, protocolErr
	}
	if err != nil {
		return nil, progress, err
	}
	if !sawDone {
		return nil, progress, fmt.Errorf("stream ended before [DONE]")
	}

	return aggregator.buildResponse(), progress, nil
}

type geminiStreamState struct {
	w                     http.ResponseWriter
	bufferedToolCalls     map[int]*geminiStreamingToolCall
	storedFinishReason    string
	storedUsage           *models.OpenAIUsage
	upstreamProtocolError error
	// clientWriteFailed is set when a write to the client fails (client
	// disconnected), so the caller can distinguish a client abort from an
	// upstream failure and avoid mislabeling it as a 502.
	clientWriteFailed bool
}

func newGeminiStreamState(w http.ResponseWriter) *geminiStreamState {
	return &geminiStreamState{
		w:                 w,
		bufferedToolCalls: make(map[int]*geminiStreamingToolCall),
	}
}

func (s *geminiStreamState) consumeChunk(chunk models.OpenAIStreamChunk) bool {
	if chunk.Usage != nil {
		s.storedUsage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		if !s.consumeChoice(choice) {
			return false
		}
	}

	return true
}

func (s *geminiStreamState) consumeChoice(choice models.OpenAIStreamChoice) bool {
	if choice.Delta.Content != nil {
		var text string
		if err := json.Unmarshal(choice.Delta.Content, &text); err == nil && text != "" {
			if !s.emitText(text) {
				return false
			}
		}
	}
	if choice.Delta.Refusal != nil {
		var refusal string
		if err := json.Unmarshal(choice.Delta.Refusal, &refusal); err == nil && refusal != "" {
			if !s.emitText(refusal) {
				return false
			}
		}
	}

	if len(choice.Delta.ToolCalls) > 0 && !s.consumeToolCalls(choice.Delta.ToolCalls) {
		return false
	}

	if choice.FinishReason != nil {
		s.storedFinishReason = *choice.FinishReason
	}

	return true
}

func (s *geminiStreamState) emitText(text string) bool {
	candidateIndex := 0
	return s.writeData(models.GeminiGenerateContentResponse{
		Candidates: []models.GeminiCandidate{{
			Content: &models.GeminiContent{
				Role: "model",
				Parts: []models.GeminiPart{{
					Text: stringPtr(text),
				}},
			},
			Index: &candidateIndex,
		}},
	})
}

func (s *geminiStreamState) consumeToolCalls(toolCalls []models.OpenAIToolCall) bool {
	if err := validateGeminiToolCallIndexes(toolCalls); err != nil {
		s.upstreamProtocolError = err
		return false
	}

	for _, toolCall := range toolCalls {
		toolIndex := 0
		if toolCall.Index != nil {
			toolIndex = *toolCall.Index
		}

		buffered, ok := s.bufferedToolCalls[toolIndex]
		if !ok {
			buffered = &geminiStreamingToolCall{}
			s.bufferedToolCalls[toolIndex] = buffered
		}
		if toolCall.ID != "" {
			buffered.ID = toolCall.ID
		}
		if toolCall.Function.Name != "" {
			buffered.Name = toolCall.Function.Name
		}
		if toolCall.Function.Arguments != "" {
			buffered.Arguments.WriteString(toolCall.Function.Arguments)
		}
	}

	return true
}

func validateGeminiToolCallIndexes(toolCalls []models.OpenAIToolCall) error {
	for _, toolCall := range toolCalls {
		if toolCall.Index != nil && *toolCall.Index < 0 {
			return fmt.Errorf("upstream stream tool call index %d is invalid; indices must be non-negative", *toolCall.Index)
		}
	}
	return nil
}

func (s *geminiStreamState) finish() bool {
	if !s.flushToolCalls(true) {
		return false
	}

	return s.writeTail()
}

func (s *geminiStreamState) flushToolCalls(terminal bool) bool {
	if len(s.bufferedToolCalls) == 0 {
		return true
	}

	toolIndexes := make([]int, 0, len(s.bufferedToolCalls))
	for toolIndex := range s.bufferedToolCalls {
		if toolIndex < 0 {
			s.upstreamProtocolError = fmt.Errorf("upstream stream tool call index %d is invalid; indices must be non-negative", toolIndex)
			return false
		}
		toolIndexes = append(toolIndexes, toolIndex)
	}
	sort.Ints(toolIndexes)

	var parts []models.GeminiPart
	for _, toolIndex := range toolIndexes {
		buffered := s.bufferedToolCalls[toolIndex]
		if buffered.Emitted || buffered.Name == "" {
			continue
		}

		args := strings.TrimSpace(buffered.Arguments.String())
		if args == "" && !terminal {
			continue
		}
		if args == "" {
			args = `{}`
		}

		normalized, err := canonicalizeJSON(json.RawMessage(args))
		if err != nil {
			continue
		}

		parts = append(parts, models.GeminiPart{
			FunctionCall: &models.GeminiFunctionCall{
				ID:   buffered.ID,
				Name: buffered.Name,
				Args: normalized,
			},
		})
		buffered.Emitted = true
	}

	if len(parts) == 0 {
		return true
	}

	candidateIndex := 0
	return s.writeData(models.GeminiGenerateContentResponse{
		Candidates: []models.GeminiCandidate{{
			Content: &models.GeminiContent{
				Role:  "model",
				Parts: parts,
			},
			Index: &candidateIndex,
		}},
	})
}

func (s *geminiStreamState) writeTail() bool {
	if s.storedFinishReason == "" && s.storedUsage == nil {
		return true
	}

	response := models.GeminiGenerateContentResponse{}
	if s.storedFinishReason != "" {
		candidateIndex := 0
		response.Candidates = []models.GeminiCandidate{{
			FinishReason: mapOpenAIFinishReasonToGemini(&s.storedFinishReason),
			Index:        &candidateIndex,
		}}
	}
	if s.storedUsage != nil {
		response.UsageMetadata = &models.GeminiUsageMetadata{
			PromptTokenCount:     s.storedUsage.PromptTokens,
			CandidatesTokenCount: s.storedUsage.CompletionTokens,
			TotalTokenCount:      s.storedUsage.TotalTokens,
		}
	}

	return s.writeData(response)
}

func (s *geminiStreamState) writeData(data interface{}) bool {
	if writeGeminiSSEData(s.w, data) != nil {
		s.clientWriteFailed = true
		return false
	}
	return true
}

func (s *geminiStreamState) writeError(statusCode int, message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream ended unexpectedly"
	}
	return s.writeData(models.GeminiErrorResponse{
		Error: models.GeminiError{
			Code:    statusCode,
			Message: message,
			Status:  mapGeminiUpstreamStatus(statusCode),
		},
	})
}
