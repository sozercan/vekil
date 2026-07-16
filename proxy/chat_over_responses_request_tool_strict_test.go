package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/sozercan/vekil/models"
)

func TestTranslateChatRequestToResponsesFunctionToolStrictDefaults(t *testing.T) {
	tests := []struct {
		name        string
		strictField string
		wantStrict  bool
	}{
		{name: "omitted", wantStrict: false},
		{name: "null", strictField: `,"strict":null`, wantStrict: false},
		{name: "true", strictField: `,"strict":true`, wantStrict: true},
		{name: "false", strictField: `,"strict":false`, wantStrict: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{
				"model":"gpt-public",
				"messages":[{"role":"user","content":"use the tool"}],
				"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}%s}}]
			}`, tt.strictField))

			requireResponsesFunctionToolStrict(t, body, tt.wantStrict)
		})
	}
}

func TestTranslateChatRequestToResponsesRejectsInvalidFunctionToolStrict(t *testing.T) {
	for _, strictValue := range []string{`"false"`, `0`, `{}`, `[]`} {
		t.Run(strictValue, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{
				"model":"gpt-public",
				"messages":[{"role":"user","content":"use the tool"}],
				"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"},"strict":%s}}]
			}`, strictValue))

			_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) {
				t.Fatalf("translateChatRequestToResponses() error = %v, want chatExecutionError", err)
			}
			if executionErr.StatusCode != 400 || executionErr.Param != "tools[0].function.strict" {
				t.Fatalf("translateChatRequestToResponses() error = %+v", executionErr)
			}
		})
	}
}

func TestTranslateChatRequestToResponsesTranslatedFunctionToolsDefaultNonStrict(t *testing.T) {
	tests := []struct {
		name string
		body func(t *testing.T) []byte
	}{
		{
			name: "Anthropic canonical Chat request",
			body: func(t *testing.T) []byte {
				t.Helper()
				maxTokens := 64
				request, err := TranslateAnthropicToOpenAI(&models.AnthropicRequest{
					Model:     "claude-sonnet-4-5",
					MaxTokens: &maxTokens,
					Messages: []models.AnthropicMessage{{
						Role:    "user",
						Content: json.RawMessage(`"use the tool"`),
					}},
					Tools: []models.AnthropicTool{{
						Name:        "lookup",
						Description: "Lookup a value",
						InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`),
					}},
				})
				if err != nil {
					t.Fatalf("TranslateAnthropicToOpenAI() error = %v", err)
				}
				return marshalCanonicalChatRequestWithOmittedStrict(t, request)
			},
		},
		{
			name: "Gemini canonical Chat request",
			body: func(t *testing.T) []byte {
				t.Helper()
				prompt := "use the tool"
				request, err := TranslateGeminiToOpenAI(&models.GeminiGenerateContentRequest{
					Contents: []models.GeminiContent{{
						Role:  "user",
						Parts: []models.GeminiPart{{Text: &prompt}},
					}},
					Tools: []models.GeminiTool{{
						FunctionDeclarations: []models.GeminiFunctionDeclaration{{
							Name:        "lookup",
							Description: "Lookup a value",
							Parameters:  json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`),
						}},
					}},
				}, "gemini-2.5-pro", false)
				if err != nil {
					t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
				}
				return marshalCanonicalChatRequestWithOmittedStrict(t, request)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireResponsesFunctionToolStrict(t, tt.body(t), false)
		})
	}
}

func marshalCanonicalChatRequestWithOmittedStrict(t *testing.T, request *models.OpenAIRequest) []byte {
	t.Helper()
	if len(request.Tools) != 1 {
		t.Fatalf("canonical Chat tools = %#v, want one tool", request.Tools)
	}
	if request.Tools[0].Function.Strict != nil {
		t.Fatalf("canonical Chat tool strict = %v, want omitted", *request.Tools[0].Function.Strict)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal canonical Chat request: %v", err)
	}
	return body
}

func requireResponsesFunctionToolStrict(t *testing.T, body []byte, want bool) {
	t.Helper()
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{UpstreamModel: "gpt-upstream"})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}

	var request struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(plan.Body, &request); err != nil {
		t.Fatalf("decode Responses request: %v", err)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("Responses tools = %#v, want one tool", request.Tools)
	}

	rawStrict, ok := request.Tools[0]["strict"]
	if !ok {
		t.Fatalf("Responses tool = %s, want explicit strict boolean", request.Tools[0])
	}
	var decoded any
	if err := json.Unmarshal(rawStrict, &decoded); err != nil {
		t.Fatalf("decode Responses tool strict: %v", err)
	}
	got, ok := decoded.(bool)
	if !ok {
		t.Fatalf("Responses tool strict = %s, want boolean", rawStrict)
	}
	if got != want {
		t.Fatalf("Responses tool strict = %v, want %v", got, want)
	}
}
