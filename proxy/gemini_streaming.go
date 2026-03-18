package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/copilot-proxy/models"
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
	defer func() { _ = body.Close() }()
	setSSEHeaders(w)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	bufferedToolCalls := make(map[int]*geminiStreamingToolCall)
	var storedFinishReason string
	var storedUsage *models.OpenAIUsage

	for scanner.Scan() {
		line := scanner.Text()
		data, ok := parseSSELine(line)
		if !ok {
			continue
		}

		if data == "[DONE]" {
			if err := flushGeminiStreamingToolCalls(w, bufferedToolCalls, true); err != nil {
				return
			}
			_ = writeGeminiStreamingTail(w, storedFinishReason, storedUsage)
			return
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			storedUsage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				var text string
				if err := json.Unmarshal(choice.Delta.Content, &text); err == nil && text != "" {
					candidateIndex := 0
					if err := writeGeminiSSEData(w, models.GeminiGenerateContentResponse{
						Candidates: []models.GeminiCandidate{{
							Content: &models.GeminiContent{
								Role: "model",
								Parts: []models.GeminiPart{{
									Text: stringPtr(text),
								}},
							},
							Index: &candidateIndex,
						}},
					}); err != nil {
						return
					}
				}
			}

			if len(choice.Delta.ToolCalls) > 0 {
				var parts []models.GeminiPart
				for _, toolCall := range choice.Delta.ToolCalls {
					idx := 0
					if toolCall.Index != nil {
						idx = *toolCall.Index
					}

					buffered, ok := bufferedToolCalls[idx]
					if !ok {
						buffered = &geminiStreamingToolCall{}
						bufferedToolCalls[idx] = buffered
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

				if len(parts) > 0 {
					candidateIndex := 0
					if err := writeGeminiSSEData(w, models.GeminiGenerateContentResponse{
						Candidates: []models.GeminiCandidate{{
							Content: &models.GeminiContent{
								Role:  "model",
								Parts: parts,
							},
							Index: &candidateIndex,
						}},
					}); err != nil {
						return
					}
				}
			}

			if choice.FinishReason != nil {
				storedFinishReason = *choice.FinishReason
			}
		}
	}

	// Unexpected EOF or scanner error: do not emit synthetic terminal frames.
	if scanner.Err() != nil {
		return
	}
}

func flushGeminiStreamingToolCalls(w http.ResponseWriter, bufferedToolCalls map[int]*geminiStreamingToolCall, terminal bool) error {
	if len(bufferedToolCalls) == 0 {
		return nil
	}

	maxIdx := -1
	for idx := range bufferedToolCalls {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	var parts []models.GeminiPart
	for idx := 0; idx <= maxIdx; idx++ {
		buffered, ok := bufferedToolCalls[idx]
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
		return nil
	}

	candidateIndex := 0
	return writeGeminiSSEData(w, models.GeminiGenerateContentResponse{
		Candidates: []models.GeminiCandidate{{
			Content: &models.GeminiContent{
				Role:  "model",
				Parts: parts,
			},
			Index: &candidateIndex,
		}},
	})
}

func writeGeminiStreamingTail(w http.ResponseWriter, finishReason string, usage *models.OpenAIUsage) error {
	if finishReason == "" && usage == nil {
		return nil
	}

	response := models.GeminiGenerateContentResponse{}
	if finishReason != "" {
		candidateIndex := 0
		response.Candidates = []models.GeminiCandidate{{
			FinishReason: mapOpenAIFinishReasonToGemini(&finishReason),
			Index:        &candidateIndex,
		}}
	}
	if usage != nil {
		response.UsageMetadata = &models.GeminiUsageMetadata{
			PromptTokenCount:     usage.PromptTokens,
			CandidatesTokenCount: usage.CompletionTokens,
			TotalTokenCount:      usage.TotalTokens,
		}
	}
	return writeGeminiSSEData(w, response)
}
