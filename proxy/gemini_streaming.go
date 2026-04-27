package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
func StreamOpenAIToGemini(w http.ResponseWriter, body io.ReadCloser, observeUsage func(*models.OpenAIUsage)) {
	defer func() { _ = body.Close() }()
	setSSEHeaders(w)

	state := newGeminiStreamState(w, observeUsage)
	sawDone, err := consumeOpenAIStreamChunks(body, state.consumeChunk)
	if err != nil || !sawDone {
		return
	}

	_ = state.finish()
}

type geminiStreamState struct {
	w                  http.ResponseWriter
	observeUsage       func(*models.OpenAIUsage)
	bufferedToolCalls  map[int]*geminiStreamingToolCall
	storedFinishReason string
	storedUsage        *models.OpenAIUsage
}

func newGeminiStreamState(w http.ResponseWriter, observeUsage func(*models.OpenAIUsage)) *geminiStreamState {
	return &geminiStreamState{
		w:                 w,
		observeUsage:      observeUsage,
		bufferedToolCalls: make(map[int]*geminiStreamingToolCall),
	}
}

func (s *geminiStreamState) consumeChunk(chunk models.OpenAIStreamChunk) bool {
	if chunk.Usage != nil {
		s.storedUsage = chunk.Usage
		if s.observeUsage != nil {
			s.observeUsage(chunk.Usage)
		}
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
	var parts []models.GeminiPart
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

		args := strings.TrimSpace(buffered.Arguments.String())
		if buffered.Emitted || buffered.Name == "" || args == "" {
			continue
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

	maxIndex := -1
	for toolIndex := range s.bufferedToolCalls {
		if toolIndex > maxIndex {
			maxIndex = toolIndex
		}
	}

	var parts []models.GeminiPart
	for toolIndex := 0; toolIndex <= maxIndex; toolIndex++ {
		buffered, ok := s.bufferedToolCalls[toolIndex]
		if !ok || buffered.Emitted || buffered.Name == "" {
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
	return writeGeminiSSEData(s.w, data) == nil
}
