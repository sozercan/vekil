package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sozercan/vekil/models"
)

func TestTranslateGeminiToOpenAI(t *testing.T) {
	t.Run("system text and user text", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Model: "models/gemini-2.5-pro",
			SystemInstruction: &models.GeminiContent{
				Parts: []models.GeminiPart{
					{Text: stringPtr("Be concise")},
					{Text: stringPtr("Return JSON")},
				},
			},
			Contents: []models.GeminiContent{{
				Role: "user",
				Parts: []models.GeminiPart{
					{Text: stringPtr("Hello")},
				},
			}},
			GenerationConfig: &models.GeminiGenerationConfig{
				Temperature:      floatPtr(0.2),
				TopP:             floatPtr(0.8),
				MaxOutputTokens:  intPtr(256),
				ResponseMimeType: "application/json",
			},
		}

		got, err := TranslateGeminiToOpenAI(req, "models/gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if got.Model != "gemini-2.5-pro" {
			t.Fatalf("Model = %q, want %q", got.Model, "gemini-2.5-pro")
		}
		if len(got.Messages) != 2 {
			t.Fatalf("len(Messages) = %d, want 2", len(got.Messages))
		}

		var systemText string
		if err := json.Unmarshal(got.Messages[0].Content, &systemText); err != nil {
			t.Fatalf("unmarshal system content: %v", err)
		}
		if systemText != "Be concise\nReturn JSON" {
			t.Errorf("system content = %q, want %q", systemText, "Be concise\nReturn JSON")
		}

		var userText string
		if err := json.Unmarshal(got.Messages[1].Content, &userText); err != nil {
			t.Fatalf("unmarshal user content: %v", err)
		}
		if userText != "Hello" {
			t.Errorf("user content = %q, want %q", userText, "Hello")
		}

		if got.MaxTokens == nil || *got.MaxTokens != 256 {
			t.Errorf("MaxTokens = %v, want 256", got.MaxTokens)
		}
		if got.Stream != nil {
			t.Errorf("Stream = %v, want nil", got.Stream)
		}

		var responseFormat map[string]interface{}
		if err := json.Unmarshal(got.ResponseFormat, &responseFormat); err != nil {
			t.Fatalf("unmarshal response_format: %v", err)
		}
		if responseFormat["type"] != "json_object" {
			t.Errorf("response_format.type = %v, want json_object", responseFormat["type"])
		}
	})

	t.Run("function call history matches earliest unmatched call", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{
				{
					Role: "model",
					Parts: []models.GeminiPart{
						{FunctionCall: &models.GeminiFunctionCall{Name: "search", Args: json.RawMessage(`{"q":"first"}`)}},
					},
				},
				{
					Role: "model",
					Parts: []models.GeminiPart{
						{FunctionCall: &models.GeminiFunctionCall{Name: "search", Args: json.RawMessage(`{"q":"second"}`)}},
					},
				},
				{
					Role: "user",
					Parts: []models.GeminiPart{
						{FunctionResponse: &models.GeminiFunctionResponse{Name: "search", Response: json.RawMessage(`{"ok":1}`)}},
					},
				},
				{
					Role: "user",
					Parts: []models.GeminiPart{
						{FunctionResponse: &models.GeminiFunctionResponse{Name: "search", Response: json.RawMessage(`{"ok":2}`)}},
					},
				},
			},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if len(got.Messages) != 4 {
			t.Fatalf("len(Messages) = %d, want 4", len(got.Messages))
		}
		if got.Messages[0].ToolCalls[0].ID != "gemini-fc-0-0" {
			t.Errorf("first tool call ID = %q, want %q", got.Messages[0].ToolCalls[0].ID, "gemini-fc-0-0")
		}
		if got.Messages[1].ToolCalls[0].ID != "gemini-fc-1-0" {
			t.Errorf("second tool call ID = %q, want %q", got.Messages[1].ToolCalls[0].ID, "gemini-fc-1-0")
		}
		if got.Messages[2].ToolCallID != "gemini-fc-0-0" {
			t.Errorf("first tool response ToolCallID = %q, want %q", got.Messages[2].ToolCallID, "gemini-fc-0-0")
		}
		if got.Messages[3].ToolCallID != "gemini-fc-1-0" {
			t.Errorf("second tool response ToolCallID = %q, want %q", got.Messages[3].ToolCallID, "gemini-fc-1-0")
		}
	})

	t.Run("function response IDs match explicit Gemini call IDs", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{
				{
					Role: "model",
					Parts: []models.GeminiPart{
						{FunctionCall: &models.GeminiFunctionCall{ID: "call-first", Name: "search", Args: json.RawMessage(`{"q":"first"}`)}},
					},
				},
				{
					Role: "model",
					Parts: []models.GeminiPart{
						{FunctionCall: &models.GeminiFunctionCall{ID: "call-second", Name: "search", Args: json.RawMessage(`{"q":"second"}`)}},
					},
				},
				{
					Role: "user",
					Parts: []models.GeminiPart{
						{FunctionResponse: &models.GeminiFunctionResponse{ID: "call-second", Name: "search", Response: json.RawMessage(`{"ok":2}`)}},
					},
				},
				{
					Role: "user",
					Parts: []models.GeminiPart{
						{FunctionResponse: &models.GeminiFunctionResponse{ID: "call-first", Name: "search", Response: json.RawMessage(`{"ok":1}`)}},
					},
				},
			},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if len(got.Messages) != 4 {
			t.Fatalf("len(Messages) = %d, want 4", len(got.Messages))
		}
		if got.Messages[0].ToolCalls[0].ID != "call-first" {
			t.Errorf("first tool call ID = %q, want %q", got.Messages[0].ToolCalls[0].ID, "call-first")
		}
		if got.Messages[1].ToolCalls[0].ID != "call-second" {
			t.Errorf("second tool call ID = %q, want %q", got.Messages[1].ToolCalls[0].ID, "call-second")
		}
		if got.Messages[2].ToolCallID != "call-second" {
			t.Errorf("first tool response ToolCallID = %q, want %q", got.Messages[2].ToolCallID, "call-second")
		}
		if got.Messages[3].ToolCallID != "call-first" {
			t.Errorf("second tool response ToolCallID = %q, want %q", got.Messages[3].ToolCallID, "call-first")
		}
	})

	t.Run("user inline image becomes multimodal content array", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{{
				Role: "user",
				Parts: []models.GeminiPart{
					{Text: stringPtr("Describe ")},
					{InlineData: json.RawMessage(`{"mimeType":"image/png","data":"AQID"}`)},
					{Text: stringPtr("in one sentence.")},
				},
			}},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if len(got.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want 1", len(got.Messages))
		}

		var parts []models.OpenAIContentPart
		if err := json.Unmarshal(got.Messages[0].Content, &parts); err != nil {
			t.Fatalf("unmarshal multimodal content: %v", err)
		}

		if len(parts) != 3 {
			t.Fatalf("len(parts) = %d, want 3", len(parts))
		}
		if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "Describe " {
			t.Errorf("parts[0] = %#v, want text part", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
			t.Fatalf("parts[1] = %#v, want image_url part", parts[1])
		}
		if parts[1].ImageURL.URL != "data:image/png;base64,AQID" {
			t.Errorf("parts[1].ImageURL.URL = %q, want data URL", parts[1].ImageURL.URL)
		}
		if parts[2].Type != "text" || parts[2].Text == nil || *parts[2].Text != "in one sentence." {
			t.Errorf("parts[2] = %#v, want trailing text part", parts[2])
		}
	})

	t.Run("tools, tool choice, structured output, and streaming", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{{
				Role: "user",
				Parts: []models.GeminiPart{
					{Text: stringPtr("Use the tool")},
				},
			}},
			Tools: []models.GeminiTool{{
				FunctionDeclarations: []models.GeminiFunctionDeclaration{{
					Name:        "lookup_weather",
					Description: "Looks up weather",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
				}},
			}},
			ToolConfig: &models.GeminiToolConfig{
				FunctionCallingConfig: &models.GeminiFunctionCallingConfig{
					Mode:                 "ANY",
					AllowedFunctionNames: []string{"lookup_weather"},
				},
			},
			GenerationConfig: &models.GeminiGenerationConfig{
				ResponseMimeType: "application/json",
				ResponseSchema:   json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`),
			},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", true)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if got.Stream == nil || !*got.Stream {
			t.Fatalf("Stream = %v, want true", got.Stream)
		}
		if got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
			t.Fatalf("StreamOptions = %#v, want include_usage", got.StreamOptions)
		}
		if len(got.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(got.Tools))
		}
		if got.ParallelToolCalls == nil || !*got.ParallelToolCalls {
			t.Fatalf("ParallelToolCalls = %v, want true", got.ParallelToolCalls)
		}

		var toolChoice map[string]interface{}
		if err := json.Unmarshal(got.ToolChoice, &toolChoice); err != nil {
			t.Fatalf("unmarshal tool_choice: %v", err)
		}
		if toolChoice["type"] != "function" {
			t.Errorf("tool_choice.type = %v, want function", toolChoice["type"])
		}

		var responseFormat map[string]interface{}
		if err := json.Unmarshal(got.ResponseFormat, &responseFormat); err != nil {
			t.Fatalf("unmarshal response_format: %v", err)
		}
		if responseFormat["type"] != "json_schema" {
			t.Errorf("response_format.type = %v, want json_schema", responseFormat["type"])
		}
	})

	t.Run("tools accept parametersJsonSchema", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{{
				Role: "user",
				Parts: []models.GeminiPart{
					{Text: stringPtr("Use the tool")},
				},
			}},
			Tools: []models.GeminiTool{{
				FunctionDeclarations: []models.GeminiFunctionDeclaration{{
					Name: "lookup_weather",
					ParametersJSONSchema: json.RawMessage(`{
						"type":"object",
						"properties":{"city":{"type":"string"}},
						"required":["city"]
					}`),
				}},
			}},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if len(got.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(got.Tools))
		}

		var parameters map[string]interface{}
		if err := json.Unmarshal(got.Tools[0].Function.Parameters, &parameters); err != nil {
			t.Fatalf("unmarshal tool parameters: %v", err)
		}
		if parameters["type"] != "object" {
			t.Fatalf("tool parameters type = %v, want object", parameters["type"])
		}
		properties, ok := parameters["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("tool parameters properties = %#v, want object", parameters["properties"])
		}
		if _, ok := properties["city"]; !ok {
			t.Fatalf("tool parameters properties = %#v, want city property", properties)
		}
	})

	t.Run("allowed function names narrow the forwarded tool list", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{{
				Role: "user",
				Parts: []models.GeminiPart{
					{Text: stringPtr("Use one of the allowed tools")},
				},
			}},
			Tools: []models.GeminiTool{{
				FunctionDeclarations: []models.GeminiFunctionDeclaration{
					{Name: "lookup_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
					{Name: "lookup_time", Parameters: json.RawMessage(`{"type":"object"}`)},
					{Name: "delete_everything", Parameters: json.RawMessage(`{"type":"object"}`)},
				},
			}},
			ToolConfig: &models.GeminiToolConfig{
				FunctionCallingConfig: &models.GeminiFunctionCallingConfig{
					Mode:                 "ANY",
					AllowedFunctionNames: []string{"lookup_weather", "lookup_time"},
				},
			},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if len(got.Tools) != 2 {
			t.Fatalf("len(Tools) = %d, want 2", len(got.Tools))
		}
		if got.Tools[0].Function.Name != "lookup_weather" {
			t.Errorf("tools[0].function.name = %q, want lookup_weather", got.Tools[0].Function.Name)
		}
		if got.Tools[1].Function.Name != "lookup_time" {
			t.Errorf("tools[1].function.name = %q, want lookup_time", got.Tools[1].Function.Name)
		}

		var toolChoice string
		if err := json.Unmarshal(got.ToolChoice, &toolChoice); err != nil {
			t.Fatalf("unmarshal tool_choice: %v", err)
		}
		if toolChoice != "required" {
			t.Errorf("tool_choice = %q, want required", toolChoice)
		}
	})

	t.Run("auto mode keeps auto tool choice while filtering allowed tools", func(t *testing.T) {
		req := &models.GeminiGenerateContentRequest{
			Contents: []models.GeminiContent{{
				Role: "user",
				Parts: []models.GeminiPart{
					{Text: stringPtr("Pick automatically from the allowed tools")},
				},
			}},
			Tools: []models.GeminiTool{{
				FunctionDeclarations: []models.GeminiFunctionDeclaration{
					{Name: "lookup_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
					{Name: "lookup_time", Parameters: json.RawMessage(`{"type":"object"}`)},
					{Name: "delete_everything", Parameters: json.RawMessage(`{"type":"object"}`)},
				},
			}},
			ToolConfig: &models.GeminiToolConfig{
				FunctionCallingConfig: &models.GeminiFunctionCallingConfig{
					Mode:                 "AUTO",
					AllowedFunctionNames: []string{"lookup_weather", "lookup_time"},
				},
			},
		}

		got, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
		if err != nil {
			t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
		}

		if len(got.Tools) != 2 {
			t.Fatalf("len(Tools) = %d, want 2", len(got.Tools))
		}
		if got.Tools[0].Function.Name != "lookup_weather" {
			t.Errorf("tools[0].function.name = %q, want lookup_weather", got.Tools[0].Function.Name)
		}
		if got.Tools[1].Function.Name != "lookup_time" {
			t.Errorf("tools[1].function.name = %q, want lookup_time", got.Tools[1].Function.Name)
		}

		var toolChoice string
		if err := json.Unmarshal(got.ToolChoice, &toolChoice); err != nil {
			t.Fatalf("unmarshal tool_choice: %v", err)
		}
		if toolChoice != "auto" {
			t.Errorf("tool_choice = %q, want auto", toolChoice)
		}
	})
}

func TestDecodeGeminiGenerateContentRequestSnakeCase(t *testing.T) {
	body := []byte(`{
		"model": "models/gemini-2.5-pro",
		"system_instruction": {
			"parts": [{"text":"Be terse"}]
		},
		"contents": [{
			"role": "user",
			"parts": [
				{"text":"Describe "},
				{"inline_data":{"mime_type":"image/png","data":"AQID"}}
			]
		}],
		"tools": [{
			"function_declarations": [{
				"name": "lookup_weather",
				"parameters": {"type":"object","properties":{"city":{"type":"string"}}}
			}]
		}],
		"tool_config": {
			"function_calling_config": {
				"mode": "ANY",
				"allowed_function_names": ["lookup_weather"]
			}
		},
		"generation_config": {
			"top_k": 64,
			"max_output_tokens": 64,
			"response_json_schema": {"type":"object","properties":{"answer":{"type":"string"}}},
			"thinking_config": {
				"include_thoughts": true,
				"thinking_budget": 8192,
				"thinking_level": "HIGH"
			},
			"presence_penalty": 0.1,
			"frequency_penalty": 0.2,
			"seed": 7
		}
	}`)

	req, err := decodeGeminiGenerateContentRequest(body)
	if err != nil {
		t.Fatalf("decodeGeminiGenerateContentRequest() error = %v", err)
	}
	if req.GenerationConfig == nil {
		t.Fatal("GenerationConfig = nil, want non-nil")
	}
	if req.GenerationConfig.TopK == nil || *req.GenerationConfig.TopK != 64 {
		t.Fatalf("TopK = %v, want 64", req.GenerationConfig.TopK)
	}

	var thinking struct {
		IncludeThoughts *bool   `json:"includeThoughts"`
		ThinkingBudget  *int    `json:"thinkingBudget"`
		ThinkingLevel   *string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(req.GenerationConfig.ThinkingConfig, &thinking); err != nil {
		t.Fatalf("unmarshal thinkingConfig: %v", err)
	}
	if thinking.IncludeThoughts == nil || !*thinking.IncludeThoughts {
		t.Fatalf("IncludeThoughts = %v, want true", thinking.IncludeThoughts)
	}
	if thinking.ThinkingBudget == nil || *thinking.ThinkingBudget != 8192 {
		t.Fatalf("ThinkingBudget = %v, want 8192", thinking.ThinkingBudget)
	}
	if thinking.ThinkingLevel == nil || *thinking.ThinkingLevel != "HIGH" {
		t.Fatalf("ThinkingLevel = %v, want HIGH", thinking.ThinkingLevel)
	}

	got, err := TranslateGeminiToOpenAI(req, "models/gemini-2.5-pro", false)
	if err != nil {
		t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
	}

	if len(got.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(got.Messages))
	}

	var systemText string
	if err := json.Unmarshal(got.Messages[0].Content, &systemText); err != nil {
		t.Fatalf("unmarshal system content: %v", err)
	}
	if systemText != "Be terse" {
		t.Errorf("system content = %q, want %q", systemText, "Be terse")
	}

	var parts []models.OpenAIContentPart
	if err := json.Unmarshal(got.Messages[1].Content, &parts); err != nil {
		t.Fatalf("unmarshal user content: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "Describe " {
		t.Errorf("parts[0] = %#v, want text part", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AQID" {
		t.Errorf("parts[1] = %#v, want image_url data URL", parts[1])
	}

	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "lookup_weather" {
		t.Fatalf("Tools = %#v, want lookup_weather", got.Tools)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 64 {
		t.Errorf("MaxTokens = %v, want 64", got.MaxTokens)
	}
	if got.PresencePenalty == nil || *got.PresencePenalty != 0.1 {
		t.Errorf("PresencePenalty = %v, want 0.1", got.PresencePenalty)
	}
	if got.FrequencyPenalty == nil || *got.FrequencyPenalty != 0.2 {
		t.Errorf("FrequencyPenalty = %v, want 0.2", got.FrequencyPenalty)
	}
	if got.Seed == nil || *got.Seed != 7 {
		t.Errorf("Seed = %v, want 7", got.Seed)
	}

	var responseFormat map[string]interface{}
	if err := json.Unmarshal(got.ResponseFormat, &responseFormat); err != nil {
		t.Fatalf("unmarshal response_format: %v", err)
	}
	if responseFormat["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", responseFormat["type"])
	}
}

func TestDecodeGeminiGenerateContentRequestEdgeCases(t *testing.T) {
	t.Run("duplicate canonical field via snake case alias is rejected", func(t *testing.T) {
		body := []byte(`{
			"contents": [{"role":"user","parts":[{"text":"Hi"}]}],
			"generationConfig": {"maxOutputTokens": 10},
			"generation_config": {"max_output_tokens": 10}
		}`)

		_, err := decodeGeminiGenerateContentRequest(body)
		if err == nil {
			t.Fatal("decodeGeminiGenerateContentRequest() error = nil, want non-nil")
		}

		var geminiErr *geminiProtocolError
		if !errors.As(err, &geminiErr) {
			t.Fatalf("error type = %T, want *geminiProtocolError", err)
		}
		if geminiErr.statusCode != http.StatusBadRequest {
			t.Fatalf("statusCode = %d, want 400", geminiErr.statusCode)
		}
		if !strings.Contains(geminiErr.message, `request contains duplicate field "generationConfig"`) {
			t.Fatalf("message = %q, want duplicate generationConfig detail", geminiErr.message)
		}
	})

	t.Run("unknown nested snake case field is rejected with full path", func(t *testing.T) {
		body := []byte(`{
			"contents": [{"role":"user","parts":[{"text":"Hi"}]}],
			"generation_config": {"unexpected_option": true}
		}`)

		_, err := decodeGeminiGenerateContentRequest(body)
		if err == nil {
			t.Fatal("decodeGeminiGenerateContentRequest() error = nil, want non-nil")
		}

		var geminiErr *geminiProtocolError
		if !errors.As(err, &geminiErr) {
			t.Fatalf("error type = %T, want *geminiProtocolError", err)
		}
		if geminiErr.statusCode != http.StatusBadRequest {
			t.Fatalf("statusCode = %d, want 400", geminiErr.statusCode)
		}
		if !strings.Contains(geminiErr.message, "request.generationConfig.unexpected_option is not supported or unknown") {
			t.Fatalf("message = %q, want nested unknown-field detail", geminiErr.message)
		}
	})

	t.Run("null object aliases decode to nil pointers", func(t *testing.T) {
		body := []byte(`{
			"contents": [{"role":"user","parts":[{"text":"Hi"}]}],
			"generation_config": null,
			"tool_config": null,
			"system_instruction": null
		}`)

		req, err := decodeGeminiGenerateContentRequest(body)
		if err != nil {
			t.Fatalf("decodeGeminiGenerateContentRequest() error = %v", err)
		}
		if req.GenerationConfig != nil {
			t.Fatalf("GenerationConfig = %#v, want nil", req.GenerationConfig)
		}
		if req.ToolConfig != nil {
			t.Fatalf("ToolConfig = %#v, want nil", req.ToolConfig)
		}
		if req.SystemInstruction != nil {
			t.Fatalf("SystemInstruction = %#v, want nil", req.SystemInstruction)
		}
	})
}

func TestDecodeGeminiGenerateContentRequestErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{
			name: "duplicate camel and snake aliases are rejected",
			body: `{
				"systemInstruction": {"parts":[{"text":"one"}]},
				"system_instruction": {"parts":[{"text":"two"}]},
				"contents": [{"role":"user","parts":[{"text":"hi"}]}]
			}`,
			wantMsg: `request contains duplicate field "systemInstruction"`,
		},
		{
			name: "unknown nested generation config field is rejected",
			body: `{
				"contents": [{"role":"user","parts":[{"text":"hi"}]}],
				"generation_config": {"unexpected_option": true}
			}`,
			wantMsg: "request.generationConfig.unexpected_option is not supported or unknown",
		},
		{
			name: "unknown nested thinking config field is rejected",
			body: `{
				"contents": [{"role":"user","parts":[{"text":"hi"}]}],
				"generation_config": {"thinking_config": {"unexpected_option": true}}
			}`,
			wantMsg: "request.generationConfig.thinkingConfig.unexpected_option is not supported or unknown",
		},
		{
			name: "contents must be an array",
			body: `{
				"contents": {"role":"user","parts":[{"text":"hi"}]}
			}`,
			wantMsg: "request.contents must be a JSON array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeGeminiGenerateContentRequest([]byte(tt.body))
			if err == nil {
				t.Fatal("decodeGeminiGenerateContentRequest() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestNormalizeGeminiModelName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strips models prefix",
			input: "models/gemini-3.1-pro-preview",
			want:  "gemini-3.1-pro-preview",
		},
		{
			name:  "normalizes compression pro alias",
			input: "models/chat-compression-3-pro",
			want:  "gemini-3-pro-preview",
		},
		{
			name:  "normalizes compression flash alias",
			input: "chat-compression-3-flash",
			want:  "gemini-3-flash-preview",
		},
		{
			name:  "normalizes compression 2.5 pro alias",
			input: "chat-compression-2.5-pro",
			want:  "gemini-2.5-pro",
		},
		{
			name:  "normalizes compression 2.5 flash alias",
			input: "chat-compression-2.5-flash",
			want:  "gemini-3-flash-preview",
		},
		{
			name:  "normalizes compression 2.5 flash lite alias",
			input: "chat-compression-2.5-flash-lite",
			want:  "gemini-3-flash-preview",
		},
		{
			name:  "normalizes default compression alias",
			input: "chat-compression-default",
			want:  "gemini-3-pro-preview",
		},
		{
			name:  "normalizes customtools preview model",
			input: "models/gemini-3.1-pro-preview-customtools",
			want:  "gemini-3.1-pro-preview",
		},
		{
			name:  "normalizes unsupported 2.5 flash model",
			input: "gemini-2.5-flash",
			want:  "gemini-3-flash-preview",
		},
		{
			name:  "normalizes unsupported 2.5 flash lite model",
			input: "gemini-2.5-flash-lite",
			want:  "gemini-3-flash-preview",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeGeminiModelName(tt.input)
			if got != tt.want {
				t.Fatalf("normalizeGeminiModelName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTranslateGeminiToOpenAIUtilityModelNormalization(t *testing.T) {
	tests := []struct {
		name      string
		pathModel string
		bodyModel string
		wantModel string
	}{
		{
			name:      "customtools preview falls back to standard 3.1 preview",
			pathModel: "gemini-3.1-pro-preview-customtools",
			bodyModel: "models/gemini-3.1-pro-preview-customtools",
			wantModel: "gemini-3.1-pro-preview",
		},
		{
			name:      "2.5 flash falls back to 3 flash preview",
			pathModel: "gemini-2.5-flash",
			bodyModel: "models/gemini-2.5-flash",
			wantModel: "gemini-3-flash-preview",
		},
		{
			name:      "2.5 flash lite falls back to 3 flash preview",
			pathModel: "gemini-2.5-flash-lite",
			bodyModel: "models/gemini-2.5-flash-lite",
			wantModel: "gemini-3-flash-preview",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &models.GeminiGenerateContentRequest{
				Model: tt.bodyModel,
				Contents: []models.GeminiContent{{
					Role: "user",
					Parts: []models.GeminiPart{{
						Text: stringPtr("hi"),
					}},
				}},
			}

			got, err := TranslateGeminiToOpenAI(req, tt.pathModel, false)
			if err != nil {
				t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
			}
			if got.Model != tt.wantModel {
				t.Fatalf("TranslateGeminiToOpenAI() model = %q, want %q", got.Model, tt.wantModel)
			}
		})
	}
}

func TestParseGeminiPath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantModel  string
		wantAction string
		wantErr    string
	}{
		{
			name:       "v1beta generate content",
			path:       "/v1beta/models/gemini-2.5-pro:generateContent",
			wantModel:  "gemini-2.5-pro",
			wantAction: "generateContent",
		},
		{
			name:       "v1 count tokens",
			path:       "/v1/models/gemini-2.5-pro:countTokens",
			wantModel:  "gemini-2.5-pro",
			wantAction: "countTokens",
		},
		{
			name:       "bare models stream",
			path:       "/models/gemini-2.5-pro:streamGenerateContent",
			wantModel:  "gemini-2.5-pro",
			wantAction: "streamGenerateContent",
		},
		{
			name:    "missing model and action",
			path:    "/v1/models/",
			wantErr: "missing Gemini model and action in path",
		},
		{
			name:    "missing action separator",
			path:    "/v1/models/gemini-2.5-pro",
			wantErr: "Gemini routes must use /models/{model}:action",
		},
		{
			name:    "unsupported prefix",
			path:    "/v2/models/gemini-2.5-pro:generateContent",
			wantErr: "unsupported Gemini route",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotModel, gotAction, err := parseGeminiPath(tt.path)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseGeminiPath(%q) error = nil, want %q", tt.path, tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("parseGeminiPath(%q) error = %q, want %q", tt.path, err.Error(), tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("parseGeminiPath(%q) error = %v", tt.path, err)
			}
			if gotModel != tt.wantModel {
				t.Fatalf("parseGeminiPath(%q) model = %q, want %q", tt.path, gotModel, tt.wantModel)
			}
			if gotAction != tt.wantAction {
				t.Fatalf("parseGeminiPath(%q) action = %q, want %q", tt.path, gotAction, tt.wantAction)
			}
		})
	}
}

func TestTranslateGeminiToOpenAIErrors(t *testing.T) {
	tests := []struct {
		name       string
		req        *models.GeminiGenerateContentRequest
		pathModel  string
		wantCode   int
		wantStatus string
		wantMsg    string
	}{
		{
			name: "candidate count unsupported",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				GenerationConfig: &models.GeminiGenerationConfig{
					CandidateCount: intPtr(2),
				},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
		},
		{
			name: "response modalities unsupported",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				GenerationConfig: &models.GeminiGenerationConfig{
					ResponseModalities: json.RawMessage(`["AUDIO"]`),
				},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
			wantMsg:    "generationConfig.responseModalities is not supported",
		},
		{
			name: "conflicting response schema aliases rejected",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				GenerationConfig: &models.GeminiGenerationConfig{
					ResponseSchema:     json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
					ResponseJSONSchema: json.RawMessage(`{"type":"object","properties":{"b":{"type":"string"}}}`),
				},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusBadRequest,
			wantStatus: "INVALID_ARGUMENT",
			wantMsg:    "generationConfig.responseSchema and generationConfig.responseJsonSchema cannot differ",
		},
		{
			name: "google search unsupported",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				Tools: []models.GeminiTool{{
					GoogleSearch: json.RawMessage(`{}`),
				}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
			wantMsg:    "Gemini native web tools cannot be translated to Copilot function calls",
		},
		{
			name: "google maps unsupported",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				Tools: []models.GeminiTool{{
					GoogleMaps: json.RawMessage(`{}`),
				}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
			wantMsg:    "tools[0].googleMaps is not supported",
		},
		{
			name: "enterprise web search unsupported",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				Tools: []models.GeminiTool{{
					EnterpriseWebSearch: json.RawMessage(`{}`),
				}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
			wantMsg:    "tools[0].enterpriseWebSearch is not supported",
		},
		{
			name: "unmatched function response",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{
					Role: "user",
					Parts: []models.GeminiPart{{
						FunctionResponse: &models.GeminiFunctionResponse{Name: "search", Response: json.RawMessage(`{"ok":true}`)},
					}},
				}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusBadRequest,
			wantStatus: "INVALID_ARGUMENT",
		},
		{
			name: "unknown function response id with explicit history",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{
					{
						Role: "model",
						Parts: []models.GeminiPart{{
							FunctionCall: &models.GeminiFunctionCall{ID: "call-1", Name: "search", Args: json.RawMessage(`{"q":"hi"}`)},
						}},
					},
					{
						Role: "user",
						Parts: []models.GeminiPart{{
							FunctionResponse: &models.GeminiFunctionResponse{ID: "call-2", Name: "search", Response: json.RawMessage(`{"ok":true}`)},
						}},
					},
				},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusBadRequest,
			wantStatus: "INVALID_ARGUMENT",
		},
		{
			name: "unsupported inlineData mime type",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{
					Role: "user",
					Parts: []models.GeminiPart{{
						InlineData: json.RawMessage(`{"mimeType":"audio/wav","data":"AA=="}`),
					}},
				}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
			wantMsg:    "only image/* inlineData can be translated today",
		},
		{
			name: "invalid inlineData base64",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{
					Role: "user",
					Parts: []models.GeminiPart{{
						InlineData: json.RawMessage(`{"mimeType":"image/png","data":"***"}`),
					}},
				}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusBadRequest,
			wantStatus: "INVALID_ARGUMENT",
		},
		{
			name: "multimodal function response unsupported",
			req: &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{
					{
						Role: "model",
						Parts: []models.GeminiPart{{
							FunctionCall: &models.GeminiFunctionCall{Name: "search", Args: json.RawMessage(`{"q":"hi"}`)},
						}},
					},
					{
						Role: "user",
						Parts: []models.GeminiPart{{
							FunctionResponse: &models.GeminiFunctionResponse{
								Name:     "search",
								Response: json.RawMessage(`{"ok":true}`),
								Parts:    json.RawMessage(`[{"text":"supplemental"}]`),
							},
						}},
					},
				},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusNotImplemented,
			wantStatus: "UNIMPLEMENTED",
			wantMsg:    "functionResponse.parts is not supported",
		},
		{
			name: "body model mismatch",
			req: &models.GeminiGenerateContentRequest{
				Model:    "gemini-1.5-pro",
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
			},
			pathModel:  "gemini-2.5-pro",
			wantCode:   http.StatusBadRequest,
			wantStatus: "INVALID_ARGUMENT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TranslateGeminiToOpenAI(tt.req, tt.pathModel, false)
			if err == nil {
				t.Fatal("TranslateGeminiToOpenAI() error = nil, want non-nil")
			}

			var geminiErr *geminiProtocolError
			if !errors.As(err, &geminiErr) {
				t.Fatalf("error type = %T, want *geminiProtocolError", err)
			}
			if geminiErr.statusCode != tt.wantCode {
				t.Errorf("statusCode = %d, want %d", geminiErr.statusCode, tt.wantCode)
			}
			if geminiErr.status != tt.wantStatus {
				t.Errorf("status = %q, want %q", geminiErr.status, tt.wantStatus)
			}
			if tt.wantMsg != "" && !strings.Contains(geminiErr.message, tt.wantMsg) {
				t.Errorf("message = %q, want substring %q", geminiErr.message, tt.wantMsg)
			}
		})
	}
}

func TestTranslateGeminiToOpenAIAdditionalUnsupportedCases(t *testing.T) {
	configCases := []struct {
		name    string
		config  *models.GeminiGenerationConfig
		wantMsg string
	}{
		{
			name:    "speech config unsupported",
			config:  &models.GeminiGenerationConfig{SpeechConfig: json.RawMessage(`{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Aoede"}}}`)},
			wantMsg: "generationConfig.speechConfig is not supported",
		},
		{
			name:    "image config unsupported",
			config:  &models.GeminiGenerationConfig{ImageConfig: json.RawMessage(`{"aspectRatio":"1:1"}`)},
			wantMsg: "generationConfig.imageConfig is not supported",
		},
		{
			name:    "media resolution unsupported",
			config:  &models.GeminiGenerationConfig{MediaResolution: json.RawMessage(`"MEDIA_RESOLUTION_HIGH"`)},
			wantMsg: "generationConfig.mediaResolution is not supported",
		},
		{
			name:    "response logprobs unsupported",
			config:  &models.GeminiGenerationConfig{ResponseLogprobs: json.RawMessage(`true`)},
			wantMsg: "generationConfig.responseLogprobs is not supported",
		},
		{
			name:    "logprobs unsupported",
			config:  &models.GeminiGenerationConfig{Logprobs: json.RawMessage(`3`)},
			wantMsg: "generationConfig.logprobs is not supported",
		},
	}

	for _, tt := range configCases {
		t.Run(tt.name, func(t *testing.T) {
			req := &models.GeminiGenerateContentRequest{
				Contents:         []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				GenerationConfig: tt.config,
			}

			_, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
			if err == nil {
				t.Fatal("TranslateGeminiToOpenAI() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantMsg)
			}
		})
	}

	toolCases := []struct {
		name    string
		tool    models.GeminiTool
		wantMsg string
	}{
		{
			name:    "computer use unsupported",
			tool:    models.GeminiTool{ComputerUse: json.RawMessage(`{}`)},
			wantMsg: "tools[0].computerUse is not supported",
		},
	}

	for _, tt := range toolCases {
		t.Run(tt.name, func(t *testing.T) {
			req := &models.GeminiGenerateContentRequest{
				Contents: []models.GeminiContent{{Role: "user", Parts: []models.GeminiPart{{Text: stringPtr("hi")}}}},
				Tools:    []models.GeminiTool{tt.tool},
			}

			_, err := TranslateGeminiToOpenAI(req, "gemini-2.5-pro", false)
			if err == nil {
				t.Fatal("TranslateGeminiToOpenAI() error = nil, want non-nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestDecodeGeminiGenerateContentRequestAcceptsParametersJSONSchema(t *testing.T) {
	req, err := decodeGeminiGenerateContentRequest([]byte(`{
		"contents": [{"role":"user","parts":[{"text":"Use the tool"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name": "lookup_weather",
				"parametersJsonSchema": {
					"type": "object",
					"properties": {"city": {"type": "string"}},
					"required": ["city"]
				}
			}]
		}]
	}`))
	if err != nil {
		t.Fatalf("decodeGeminiGenerateContentRequest() error = %v", err)
	}

	if len(req.Tools) != 1 || len(req.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("decoded tools = %#v, want one function declaration", req.Tools)
	}
	if !hasRawJSON(req.Tools[0].FunctionDeclarations[0].ParametersJSONSchema) {
		t.Fatalf("ParametersJSONSchema = %s, want non-empty schema", string(req.Tools[0].FunctionDeclarations[0].ParametersJSONSchema))
	}
}

func TestDecodeGeminiGenerateContentRequestAcceptsThoughtMetadata(t *testing.T) {
	req, err := decodeGeminiGenerateContentRequest([]byte(`{
		"contents": [
			{
				"role": "model",
				"parts": [
					{
						"text": "reasoning",
						"thought": true,
						"thought_signature": "c2ln"
					},
					{
						"function_call": {
							"id": "call-1",
							"name": "read_file",
							"args": {"path": "README.md"}
						}
					}
				]
			},
			{
				"role": "user",
				"parts": [
					{
						"function_response": {
							"id": "call-1",
							"name": "read_file",
							"response": {"output": "ok"}
						}
					}
				]
			}
		]
	}`))
	if err != nil {
		t.Fatalf("decodeGeminiGenerateContentRequest() error = %v", err)
	}

	if len(req.Contents) != 2 || len(req.Contents[0].Parts) != 2 {
		t.Fatalf("decoded contents = %#v, want 2 content entries with 2 model parts", req.Contents)
	}
	if req.Contents[0].Parts[0].Thought == nil || !*req.Contents[0].Parts[0].Thought {
		t.Fatalf("Thought = %v, want true", req.Contents[0].Parts[0].Thought)
	}
	if req.Contents[0].Parts[0].ThoughtSignature != "c2ln" {
		t.Fatalf("ThoughtSignature = %q, want %q", req.Contents[0].Parts[0].ThoughtSignature, "c2ln")
	}

	got, err := TranslateGeminiToOpenAI(req, "models/gemini-3.1-pro-preview-customtools", false)
	if err != nil {
		t.Fatalf("TranslateGeminiToOpenAI() error = %v", err)
	}

	if got.Model != "gemini-3.1-pro-preview" {
		t.Fatalf("Model = %q, want gemini-3.1-pro-preview", got.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(got.Messages))
	}
	if len(got.Messages[0].ToolCalls) != 1 || got.Messages[0].ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("assistant tool calls = %#v, want read_file call", got.Messages[0].ToolCalls)
	}
	if got.Messages[0].Content != nil {
		t.Fatalf("assistant content = %s, want nil because thought text should be ignored", string(got.Messages[0].Content))
	}

	var toolResult string
	if err := json.Unmarshal(got.Messages[1].Content, &toolResult); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if toolResult != `{"output":"ok"}` {
		t.Fatalf("tool result = %q, want %q", toolResult, `{"output":"ok"}`)
	}
}

func TestTranslateOpenAIToGemini(t *testing.T) {
	finishReason := "tool_calls"
	resp := &models.OpenAIResponse{
		ID:      "chatcmpl-123",
		Created: 1234,
		Choices: []models.OpenAIChoice{{
			Index: 0,
			Message: models.OpenAIMessage{
				Role:    "assistant",
				Content: json.RawMessage(`"Hello"`),
				ToolCalls: []models.OpenAIToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: models.OpenAIFunctionCall{
						Name:      "lookup_weather",
						Arguments: `{"city":"Paris"}`,
					},
				}},
			},
			FinishReason: &finishReason,
		}},
		Usage: &models.OpenAIUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}

	got := TranslateOpenAIToGemini(resp)
	if got.UsageMetadata == nil {
		t.Fatal("UsageMetadata = nil, want non-nil")
	}
	if got.UsageMetadata.PromptTokenCount != 11 {
		t.Errorf("PromptTokenCount = %d, want 11", got.UsageMetadata.PromptTokenCount)
	}
	if len(got.Candidates) != 1 {
		t.Fatalf("len(Candidates) = %d, want 1", len(got.Candidates))
	}
	if got.Candidates[0].FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", got.Candidates[0].FinishReason)
	}
	if got.Candidates[0].Content == nil {
		t.Fatal("Content = nil, want non-nil")
	}
	if got.Candidates[0].Content.Role != "model" {
		t.Errorf("Role = %q, want model", got.Candidates[0].Content.Role)
	}
	if len(got.Candidates[0].Content.Parts) != 2 {
		t.Fatalf("len(Parts) = %d, want 2", len(got.Candidates[0].Content.Parts))
	}
	if got.Candidates[0].Content.Parts[0].Text == nil || *got.Candidates[0].Content.Parts[0].Text != "Hello" {
		t.Errorf("text part = %v, want Hello", got.Candidates[0].Content.Parts[0].Text)
	}
	if got.Candidates[0].Content.Parts[1].FunctionCall == nil {
		t.Fatal("functionCall part = nil, want non-nil")
	}
	if got.Candidates[0].Content.Parts[1].FunctionCall.ID != "call_1" {
		t.Errorf("functionCall.id = %q, want call_1", got.Candidates[0].Content.Parts[1].FunctionCall.ID)
	}
	if got.Candidates[0].Content.Parts[1].FunctionCall.Name != "lookup_weather" {
		t.Errorf("functionCall.name = %q, want lookup_weather", got.Candidates[0].Content.Parts[1].FunctionCall.Name)
	}

	var args map[string]string
	if err := json.Unmarshal(got.Candidates[0].Content.Parts[1].FunctionCall.Args, &args); err != nil {
		t.Fatalf("unmarshal functionCall.args: %v", err)
	}
	if args["city"] != "Paris" {
		t.Errorf("functionCall.args.city = %q, want Paris", args["city"])
	}
}

func TestHandleGeminiModelsGenerateContent(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Model != "gemini-2.5-pro" {
			t.Errorf("Model = %q, want gemini-2.5-pro", oaiReq.Model)
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Fatalf("Stream = %v, want true", oaiReq.Stream)
		}
		if len(oaiReq.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(oaiReq.Tools))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello from Gemini\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	reqBody := `{
		"model": "models/gemini-2.5-pro",
		"contents": [{"role":"user","parts":[{"text":"Hi"}]}],
		"tools": [{"functionDeclarations":[{"name":"lookup_weather","parameters":{"type":"object"}}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/models/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 {
		t.Fatalf("len(Candidates) = %d, want 1", len(geminiResp.Candidates))
	}
	if geminiResp.Candidates[0].Content == nil || len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("Content = %#v, want one text part", geminiResp.Candidates[0].Content)
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text == nil || *geminiResp.Candidates[0].Content.Parts[0].Text != "Hello from Gemini" {
		t.Errorf("text = %v, want Hello from Gemini", geminiResp.Candidates[0].Content.Parts[0].Text)
	}
	if geminiResp.Candidates[0].FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", geminiResp.Candidates[0].FinishReason)
	}
	if geminiResp.UsageMetadata == nil || geminiResp.UsageMetadata.TotalTokenCount != 13 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=13", geminiResp.UsageMetadata)
	}
}

func TestHandleGeminiModelsGenerateContent_ForcedStreamingUsesStreamingUpstreamTimeout(t *testing.T) {
	deadlineCh := make(chan time.Duration, 1)
	handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("expected upstream request deadline")
		}
		deadlineCh <- time.Until(deadline)

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Fatal("expected proxy to force upstream stream=true for Gemini tool calls")
		}

		return sseHTTPResponse("data: {\"id\":\"chatcmpl-gemini-deadline\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello from Gemini\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-gemini-deadline\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\ndata: [DONE]\n\n"), nil
	}))

	reqBody := `{
		"model": "models/gemini-2.5-pro",
		"contents": [{"role":"user","parts":[{"text":"Hi"}]}],
		"tools": [{"functionDeclarations":[{"name":"lookup_weather","parameters":{"type":"object"}}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/models/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	if resp := w.Result(); resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	assertDeadlineApprox(t, <-deadlineCh, streamingUpstreamTimeout)
}

func TestHandleGeminiModelsGenerateContentIgnoresTopKAndThinkingConfig(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Model != "gemini-2.5-pro" {
			t.Errorf("Model = %q, want gemini-2.5-pro", oaiReq.Model)
		}

		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-topk",
			Model:   "gemini-2.5-pro",
			Created: 1234,
			Choices: []models.OpenAIChoice{{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:    "assistant",
					Content: json.RawMessage(`"Ignored topK"`),
				},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		})
	})

	reqBody := `{
		"contents": [{"role":"user","parts":[{"text":"Reply briefly"}]}],
		"generationConfig": {
			"topK": 64,
			"thinkingConfig": {
				"includeThoughts": true,
				"thinkingBudget": 8192,
				"thinkingLevel": "HIGH"
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil || len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("unexpected response: %#v", geminiResp)
	}
	text := geminiResp.Candidates[0].Content.Parts[0].Text
	if text == nil || *text != "Ignored topK" {
		t.Errorf("text = %v, want Ignored topK", text)
	}
}

func TestHandleGeminiModelsGenerateContentGeminiCLIDefaults(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Model != "gemini-2.5-pro" {
			t.Errorf("Model = %q, want gemini-2.5-pro", oaiReq.Model)
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Fatalf("Stream = %v, want true", oaiReq.Stream)
		}
		if oaiReq.StreamOptions == nil || !oaiReq.StreamOptions.IncludeUsage {
			t.Fatalf("StreamOptions = %#v, want include_usage", oaiReq.StreamOptions)
		}
		if len(oaiReq.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(oaiReq.Tools))
		}
		if oaiReq.Tools[0].Function.Name != "lookup_weather" {
			t.Fatalf("tools[0].function.name = %q, want lookup_weather", oaiReq.Tools[0].Function.Name)
		}

		var parameters map[string]interface{}
		if err := json.Unmarshal(oaiReq.Tools[0].Function.Parameters, &parameters); err != nil {
			t.Fatalf("unmarshal tool parameters: %v", err)
		}
		if parameters["type"] != "object" {
			t.Fatalf("tool parameters type = %v, want object", parameters["type"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-cli\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"CLI defaults accepted\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-cli\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":3,\"total_tokens\":9}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	reqBody := `{
		"contents": [{"role":"user","parts":[{"text":"Look up the weather"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name": "lookup_weather",
				"parametersJsonSchema": {
					"type": "object",
					"properties": {"city": {"type": "string"}},
					"required": ["city"]
				}
			}]
		}],
		"generationConfig": {
			"topK": 64,
			"thinkingConfig": {
				"includeThoughts": true,
				"thinkingBudget": 8192,
				"thinkingLevel": "HIGH"
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil || len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("unexpected response: %#v", geminiResp)
	}
	text := geminiResp.Candidates[0].Content.Parts[0].Text
	if text == nil || *text != "CLI defaults accepted" {
		t.Errorf("text = %v, want CLI defaults accepted", text)
	}
	if geminiResp.UsageMetadata == nil || geminiResp.UsageMetadata.TotalTokenCount != 9 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=9", geminiResp.UsageMetadata)
	}
}

func TestHandleGeminiModelsGenerateContentGemini31Preview(t *testing.T) {
	const model = "gemini-3.1-pro-preview"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Model != model {
			t.Errorf("Model = %q, want %q", oaiReq.Model, model)
		}
		if len(oaiReq.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want 1", len(oaiReq.Messages))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-g31",
			Object:  "chat.completion",
			Created: 123,
			Model:   model,
			Choices: []models.OpenAIChoice{{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hi from Gemini 3.1"`)},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10},
		})
	})

	reqBody := `{
		"model": "models/gemini-3.1-pro-preview",
		"contents": [{"role":"user","parts":[{"text":"Hi"}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.1-pro-preview:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil {
		t.Fatalf("Candidates = %#v, want one candidate with content", geminiResp.Candidates)
	}
	if len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("len(Parts) = %d, want 1", len(geminiResp.Candidates[0].Content.Parts))
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text == nil || *geminiResp.Candidates[0].Content.Parts[0].Text != "Hi from Gemini 3.1" {
		t.Errorf("text = %v, want Gemini 3.1 assistant text", geminiResp.Candidates[0].Content.Parts[0].Text)
	}
}

func TestHandleGeminiModelsGenerateContentCompressionAlias(t *testing.T) {
	const upstreamModel = "gemini-3-pro-preview"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Model != upstreamModel {
			t.Errorf("Model = %q, want %q", oaiReq.Model, upstreamModel)
		}
		if len(oaiReq.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want 1", len(oaiReq.Messages))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-compress",
			Object:  "chat.completion",
			Created: 123,
			Model:   upstreamModel,
			Choices: []models.OpenAIChoice{{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Compressed ok"`)},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10},
		})
	})

	reqBody := `{
		"model": "models/chat-compression-3-pro",
		"contents": [{"role":"user","parts":[{"text":"Hi"}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/chat-compression-3-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil {
		t.Fatalf("Candidates = %#v, want one candidate with content", geminiResp.Candidates)
	}
	if len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("len(Parts) = %d, want 1", len(geminiResp.Candidates[0].Content.Parts))
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text == nil || *geminiResp.Candidates[0].Content.Parts[0].Text != "Compressed ok" {
		t.Errorf("text = %v, want alias-mapped assistant text", geminiResp.Candidates[0].Content.Parts[0].Text)
	}
}

func TestHandleGeminiModelsGenerateContentWithInlineImage(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if len(oaiReq.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want 1", len(oaiReq.Messages))
		}
		if oaiReq.Stream != nil && *oaiReq.Stream {
			t.Fatalf("Stream = %v, want false or nil", oaiReq.Stream)
		}

		var parts []models.OpenAIContentPart
		if err := json.Unmarshal(oaiReq.Messages[0].Content, &parts); err != nil {
			t.Fatalf("unmarshal forwarded content: %v", err)
		}
		if len(parts) != 2 {
			t.Fatalf("len(parts) = %d, want 2", len(parts))
		}
		if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "Describe this image." {
			t.Errorf("parts[0] = %#v, want text part", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
			t.Fatalf("parts[1] = %#v, want image_url part", parts[1])
		}
		if parts[1].ImageURL.URL != "data:image/png;base64,AQID" {
			t.Errorf("parts[1].ImageURL.URL = %q, want data URL", parts[1].ImageURL.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-img",
			Object:  "chat.completion",
			Created: 123,
			Model:   "gemini-2.5-pro",
			Choices: []models.OpenAIChoice{{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Looks like a tiny sample image."`)},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 14, CompletionTokens: 7, TotalTokens: 21},
		})
	})

	reqBody := `{
		"contents": [{
			"role": "user",
			"parts": [
				{"text":"Describe this image."},
				{"inlineData":{"mimeType":"image/png","data":"AQID"}}
			]
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 || geminiResp.Candidates[0].Content == nil {
		t.Fatalf("Candidates = %#v, want one candidate with content", geminiResp.Candidates)
	}
	if len(geminiResp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("len(Parts) = %d, want 1", len(geminiResp.Candidates[0].Content.Parts))
	}
	if geminiResp.Candidates[0].Content.Parts[0].Text == nil || *geminiResp.Candidates[0].Content.Parts[0].Text != "Looks like a tiny sample image." {
		t.Errorf("text = %v, want translated assistant text", geminiResp.Candidates[0].Content.Parts[0].Text)
	}
}

func TestHandleGeminiModelsGenerateContentAggregatesMultipleToolCalls(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Fatalf("Stream = %v, want true", oaiReq.Stream)
		}
		if oaiReq.StreamOptions == nil || !oaiReq.StreamOptions.IncludeUsage {
			t.Fatalf("StreamOptions = %#v, want include_usage", oaiReq.StreamOptions)
		}
		if len(oaiReq.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(oaiReq.Tools))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-agg\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I'll check both.\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-agg\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_left\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-agg\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"file_path\\\":\\\"left.txt\\\"}\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-agg\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_right\",\"index\":1,\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-agg\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"file_path\\\":\\\"right.txt\\\"}\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-agg\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":9,\"total_tokens\":30}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	reqBody := `{
		"contents": [{"role":"user","parts":[{"text":"Read left.txt and right.txt"}]}],
		"tools": [{"functionDeclarations":[{"name":"read_file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var geminiResp models.GeminiGenerateContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(geminiResp.Candidates) != 1 {
		t.Fatalf("len(Candidates) = %d, want 1", len(geminiResp.Candidates))
	}
	candidate := geminiResp.Candidates[0]
	if candidate.Content == nil {
		t.Fatal("Content = nil, want non-nil")
	}
	if candidate.Content.Role != "model" {
		t.Fatalf("Role = %q, want model", candidate.Content.Role)
	}
	if len(candidate.Content.Parts) != 3 {
		t.Fatalf("len(Parts) = %d, want 3", len(candidate.Content.Parts))
	}
	if candidate.Content.Parts[0].Text == nil || *candidate.Content.Parts[0].Text != "I'll check both." {
		t.Fatalf("text part = %v, want I'll check both.", candidate.Content.Parts[0].Text)
	}
	if candidate.Content.Parts[1].FunctionCall == nil || candidate.Content.Parts[1].FunctionCall.Name != "read_file" {
		t.Fatalf("first functionCall = %#v, want read_file", candidate.Content.Parts[1].FunctionCall)
	}
	if candidate.Content.Parts[1].FunctionCall.ID != "call_left" {
		t.Fatalf("first functionCall.id = %q, want call_left", candidate.Content.Parts[1].FunctionCall.ID)
	}
	if candidate.Content.Parts[2].FunctionCall == nil || candidate.Content.Parts[2].FunctionCall.Name != "read_file" {
		t.Fatalf("second functionCall = %#v, want read_file", candidate.Content.Parts[2].FunctionCall)
	}
	if candidate.Content.Parts[2].FunctionCall.ID != "call_right" {
		t.Fatalf("second functionCall.id = %q, want call_right", candidate.Content.Parts[2].FunctionCall.ID)
	}

	var leftArgs map[string]string
	if err := json.Unmarshal(candidate.Content.Parts[1].FunctionCall.Args, &leftArgs); err != nil {
		t.Fatalf("unmarshal first functionCall args: %v", err)
	}
	if leftArgs["file_path"] != "left.txt" {
		t.Errorf("left file_path = %q, want left.txt", leftArgs["file_path"])
	}

	var rightArgs map[string]string
	if err := json.Unmarshal(candidate.Content.Parts[2].FunctionCall.Args, &rightArgs); err != nil {
		t.Fatalf("unmarshal second functionCall args: %v", err)
	}
	if rightArgs["file_path"] != "right.txt" {
		t.Errorf("right file_path = %q, want right.txt", rightArgs["file_path"])
	}

	if candidate.FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", candidate.FinishReason)
	}
	if geminiResp.UsageMetadata == nil || geminiResp.UsageMetadata.TotalTokenCount != 30 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=30", geminiResp.UsageMetadata)
	}
}

func TestHandleGeminiModelsStreamGenerateContent(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Fatalf("Stream = %v, want true", oaiReq.Stream)
		}
		if oaiReq.StreamOptions == nil || !oaiReq.StreamOptions.IncludeUsage {
			t.Fatalf("StreamOptions = %#v, want include_usage", oaiReq.StreamOptions)
		}
		if len(oaiReq.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(oaiReq.Tools))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stream\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_secret\",\"index\":0,\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stream\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"file_path\\\":\\\"secret.txt\\\"}\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-stream\",\"model\":\"gemini-2.5-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":4,\"total_tokens\":16}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	reqBody := `{
		"contents": [{"role":"user","parts":[{"text":"Read secret.txt"}]}],
		"tools": [{"functionDeclarations":[{"name":"read_file","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	frames := parseGeminiSSEFrames(string(body))
	if len(frames) != 2 {
		t.Fatalf("len(frames) = %d, want 2\nraw:\n%s", len(frames), string(body))
	}

	var functionFrame models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[0]), &functionFrame); err != nil {
		t.Fatalf("unmarshal function frame: %v", err)
	}
	if len(functionFrame.Candidates) != 1 || functionFrame.Candidates[0].Content == nil {
		t.Fatalf("function frame = %#v, want one candidate with content", functionFrame)
	}
	if len(functionFrame.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("len(functionFrame parts) = %d, want 1", len(functionFrame.Candidates[0].Content.Parts))
	}
	if functionFrame.Candidates[0].Content.Parts[0].FunctionCall == nil {
		t.Fatalf("function part = %#v, want functionCall", functionFrame.Candidates[0].Content.Parts[0])
	}
	if functionFrame.Candidates[0].Content.Parts[0].FunctionCall.ID != "call_secret" {
		t.Errorf("functionCall.ID = %q, want call_secret", functionFrame.Candidates[0].Content.Parts[0].FunctionCall.ID)
	}
	if functionFrame.Candidates[0].Content.Parts[0].FunctionCall.Name != "read_file" {
		t.Errorf("functionCall.Name = %q, want read_file", functionFrame.Candidates[0].Content.Parts[0].FunctionCall.Name)
	}

	var args map[string]string
	if err := json.Unmarshal(functionFrame.Candidates[0].Content.Parts[0].FunctionCall.Args, &args); err != nil {
		t.Fatalf("unmarshal functionCall args: %v", err)
	}
	if args["file_path"] != "secret.txt" {
		t.Errorf("file_path = %q, want secret.txt", args["file_path"])
	}

	var tail models.GeminiGenerateContentResponse
	if err := json.Unmarshal([]byte(frames[1]), &tail); err != nil {
		t.Fatalf("unmarshal tail frame: %v", err)
	}
	if tail.Candidates[0].FinishReason != "STOP" {
		t.Errorf("FinishReason = %q, want STOP", tail.Candidates[0].FinishReason)
	}
	if tail.UsageMetadata == nil || tail.UsageMetadata.TotalTokenCount != 16 {
		t.Errorf("UsageMetadata = %#v, want totalTokenCount=16", tail.UsageMetadata)
	}
}

func TestHandleGeminiModelsCountTokensFallbackAndCache(t *testing.T) {
	callCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if oaiReq.Stream == nil || *oaiReq.Stream {
			t.Fatalf("Stream = %v, want false", oaiReq.Stream)
		}
		if oaiReq.Temperature == nil || *oaiReq.Temperature != 0 {
			t.Fatalf("Temperature = %v, want 0", oaiReq.Temperature)
		}
		if len(oaiReq.Tools) != 1 {
			t.Fatalf("len(Tools) = %d, want 1", len(oaiReq.Tools))
		}

		switch callCount {
		case 1:
			if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 {
				t.Fatalf("MaxCompletionTokens = %v, want 1", oaiReq.MaxCompletionTokens)
			}
			if oaiReq.MaxTokens != nil {
				t.Fatalf("MaxTokens = %v, want nil", oaiReq.MaxTokens)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"max_completion_tokens unsupported"}`))
		case 2:
			if oaiReq.MaxCompletionTokens != nil {
				t.Fatalf("MaxCompletionTokens = %v, want nil", oaiReq.MaxCompletionTokens)
			}
			if oaiReq.MaxTokens == nil || *oaiReq.MaxTokens != 1 {
				t.Fatalf("MaxTokens = %v, want 1", oaiReq.MaxTokens)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
				ID:      "chatcmpl-456",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gemini-2.5-pro",
				Choices: []models.OpenAIChoice{{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
					FinishReason: strPtr("stop"),
				}},
				Usage: &models.OpenAIUsage{PromptTokens: 17, CompletionTokens: 1, TotalTokens: 18},
			})
		default:
			t.Fatalf("unexpected upstream call %d", callCount)
		}
	})

	reqBody := `{
		"contents": [{"role":"user","parts":[{"text":"Count this"}]}],
		"tools": [{"functionDeclarations":[{"name":"lookup_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]
	}`

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("iteration %d StatusCode = %d, want 200: %s", i, resp.StatusCode, string(body))
		}

		var countResp models.GeminiCountTokensResponse
		if err := json.NewDecoder(resp.Body).Decode(&countResp); err != nil {
			t.Fatalf("decode countTokens response: %v", err)
		}
		if countResp.TotalTokens != 17 {
			t.Errorf("iteration %d TotalTokens = %d, want 17", i, countResp.TotalTokens)
		}
		if len(countResp.PromptTokensDetails) != 1 || countResp.PromptTokensDetails[0].Modality != "TEXT" {
			t.Errorf("iteration %d PromptTokensDetails = %#v, want TEXT detail", i, countResp.PromptTokensDetails)
		}
	}

	if callCount != 2 {
		t.Fatalf("upstream callCount = %d, want 2", callCount)
	}
}

func TestHandleGeminiModelsCountTokensFallbackFiltersExpected400Metrics(t *testing.T) {
	callCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		switch callCount {
		case 1:
			if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 {
				t.Fatalf("MaxCompletionTokens = %v, want 1", oaiReq.MaxCompletionTokens)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"max_completion_tokens unsupported"}`))
		case 2:
			if oaiReq.MaxTokens == nil || *oaiReq.MaxTokens != 1 {
				t.Fatalf("MaxTokens = %v, want 1", oaiReq.MaxTokens)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
				ID:      "chatcmpl-fallback-metrics",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gemini-2.5-pro",
				Choices: []models.OpenAIChoice{{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
					FinishReason: strPtr("stop"),
				}},
				Usage: &models.OpenAIUsage{PromptTokens: 17, CompletionTokens: 1, TotalTokens: 18},
			})
		default:
			t.Fatalf("unexpected upstream call %d", callCount)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"Count this"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}
	if callCount != 2 {
		t.Fatalf("upstream callCount = %d, want 2", callCount)
	}

	if got := testutil.ToFloat64(handler.metrics.upstreamErrorsTotal.WithLabelValues("copilot", "gemini-2.5-pro", metricEndpointGeminiCountTokens, "400")); got != 0 {
		t.Fatalf("upstream_errors_total{code=400} = %v, want 0 for expected fallback", got)
	}
	if got := testutil.ToFloat64(handler.metrics.requestsTotal.WithLabelValues("copilot", "gemini-2.5-pro", metricEndpointGeminiCountTokens, "200")); got != 1 {
		t.Fatalf("requests_total{status=200} = %v, want 1", got)
	}
}

func TestHandleGeminiModelsCountTokensProbeBadRequestDoesNotFallbackWithoutMaxCompletionTokensSignal(t *testing.T) {
	callCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}
		if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 {
			t.Fatalf("MaxCompletionTokens = %v, want 1", oaiReq.MaxCompletionTokens)
		}

		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"some other bad request"}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"Count this"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 400: %s", resp.StatusCode, string(body))
	}
	if callCount != 1 {
		t.Fatalf("upstream callCount = %d, want 1", callCount)
	}
	if got := testutil.ToFloat64(handler.metrics.upstreamErrorsTotal.WithLabelValues("copilot", "gemini-2.5-pro", metricEndpointGeminiCountTokens, "400")); got != 1 {
		t.Fatalf("upstream_errors_total{code=400} = %v, want 1", got)
	}
}

func TestHandleGeminiModelsCountTokensRetriesProbeBeforeSuccess(t *testing.T) {
	callCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}
		if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 {
			t.Fatalf("MaxCompletionTokens = %v, want 1", oaiReq.MaxCompletionTokens)
		}
		if oaiReq.MaxTokens != nil {
			t.Fatalf("MaxTokens = %v, want nil", oaiReq.MaxTokens)
		}

		switch callCount {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
				ID:      "chatcmpl-retry-metrics",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gemini-2.5-pro",
				Choices: []models.OpenAIChoice{{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
					FinishReason: strPtr("stop"),
				}},
				Usage: &models.OpenAIUsage{PromptTokens: 29, CompletionTokens: 1, TotalTokens: 30},
			})
		default:
			t.Fatalf("unexpected upstream call %d", callCount)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"Count this"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}
	if callCount != 2 {
		t.Fatalf("upstream callCount = %d, want 2", callCount)
	}

	if got := testutil.ToFloat64(handler.metrics.retriesTotal.WithLabelValues("copilot", "gemini-2.5-pro", metricEndpointGeminiCountTokens, "429")); got != 1 {
		t.Fatalf("retries_total{reason=429} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(handler.metrics.upstreamErrorsTotal.WithLabelValues("copilot", "gemini-2.5-pro", metricEndpointGeminiCountTokens, "429")); got != 1 {
		t.Fatalf("upstream_errors_total{code=429} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(handler.metrics.requestsTotal.WithLabelValues("copilot", "gemini-2.5-pro", metricEndpointGeminiCountTokens, "200")); got != 1 {
		t.Fatalf("requests_total{status=200} = %v, want 1", got)
	}
}

func TestHandleGeminiModelsCountTokensWithInlineImageOmitsPromptDetails(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		if len(oaiReq.Messages) != 1 {
			t.Fatalf("len(Messages) = %d, want 1", len(oaiReq.Messages))
		}
		var parts []models.OpenAIContentPart
		if err := json.Unmarshal(oaiReq.Messages[0].Content, &parts); err != nil {
			t.Fatalf("unmarshal forwarded content: %v", err)
		}
		if len(parts) != 2 {
			t.Fatalf("len(parts) = %d, want 2", len(parts))
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
			t.Fatalf("parts[1] = %#v, want image_url part", parts[1])
		}
		if oaiReq.Stream == nil || *oaiReq.Stream {
			t.Fatalf("Stream = %v, want false", oaiReq.Stream)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-789",
			Object:  "chat.completion",
			Created: 123,
			Model:   "gemini-2.5-pro",
			Choices: []models.OpenAIChoice{{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 23, CompletionTokens: 1, TotalTokens: 24},
		})
	})

	reqBody := `{
		"contents": [{
			"role": "user",
			"parts": [
				{"text":"Count this image."},
				{"inlineData":{"mimeType":"image/png","data":"AQID"}}
			]
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleGeminiModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var countResp models.GeminiCountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&countResp); err != nil {
		t.Fatalf("decode countTokens response: %v", err)
	}
	if countResp.TotalTokens != 23 {
		t.Errorf("TotalTokens = %d, want 23", countResp.TotalTokens)
	}
	if len(countResp.PromptTokensDetails) != 0 {
		t.Errorf("PromptTokensDetails = %#v, want omitted details for multimodal prompt", countResp.PromptTokensDetails)
	}
}

func TestHandleGeminiModelsCountTokensCacheNormalizesSnakeCase(t *testing.T) {
	callCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}

		switch callCount {
		case 1:
			if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 {
				t.Fatalf("MaxCompletionTokens = %v, want 1", oaiReq.MaxCompletionTokens)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"max_completion_tokens unsupported"}`))
		case 2:
			if oaiReq.MaxTokens == nil || *oaiReq.MaxTokens != 1 {
				t.Fatalf("MaxTokens = %v, want 1", oaiReq.MaxTokens)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
				ID:      "chatcmpl-cache",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gemini-2.5-pro",
				Choices: []models.OpenAIChoice{{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
					FinishReason: strPtr("stop"),
				}},
				Usage: &models.OpenAIUsage{PromptTokens: 19, CompletionTokens: 1, TotalTokens: 20},
			})
		default:
			t.Fatalf("unexpected upstream call %d", callCount)
		}
	})

	bodies := []string{
		`{
			"contents": [{"role":"user","parts":[{"text":"Count this"}]}],
			"tools": [{"functionDeclarations":[{"name":"lookup_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]
		}`,
		`{
			"contents": [{"role":"user","parts":[{"text":"Count this"}]}],
			"tools": [{"function_declarations":[{"name":"lookup_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]
		}`,
	}

	for i, reqBody := range bodies {
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("iteration %d StatusCode = %d, want 200: %s", i, resp.StatusCode, string(body))
		}

		var countResp models.GeminiCountTokensResponse
		if err := json.NewDecoder(resp.Body).Decode(&countResp); err != nil {
			t.Fatalf("decode countTokens response: %v", err)
		}
		if countResp.TotalTokens != 19 {
			t.Errorf("iteration %d TotalTokens = %d, want 19", i, countResp.TotalTokens)
		}
	}

	if callCount != 2 {
		t.Fatalf("upstream callCount = %d, want 2", callCount)
	}
}

func TestHandleGeminiModelsErrors(t *testing.T) {
	t.Run("invalid action", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "INVALID_ARGUMENT" {
			t.Errorf("status = %q, want INVALID_ARGUMENT", errResp.Error.Status)
		}
	})

	t.Run("upstream bad request maps to invalid argument", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad upstream request"}}`))
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}]
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "INVALID_ARGUMENT" {
			t.Errorf("status = %q, want INVALID_ARGUMENT", errResp.Error.Status)
		}
		if !strings.Contains(errResp.Error.Message, "upstream error (400)") {
			t.Errorf("message = %q, want upstream 400 detail", errResp.Error.Message)
		}
	})

	t.Run("unsupported feature returns Google error envelope", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig": {"candidateCount": 2}
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("StatusCode = %d, want 501", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "UNIMPLEMENTED" {
			t.Errorf("status = %q, want UNIMPLEMENTED", errResp.Error.Status)
		}
	})

	t.Run("snake case code execution tool returns unimplemented", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}],
			"tools": [{"code_execution": {}}]
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("StatusCode = %d, want 501", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "UNIMPLEMENTED" {
			t.Errorf("status = %q, want UNIMPLEMENTED", errResp.Error.Status)
		}
		if !strings.Contains(errResp.Error.Message, "tools[0].codeExecution is not supported") {
			t.Errorf("message = %q, want codeExecution unsupported detail", errResp.Error.Message)
		}
	})

	t.Run("snake case response modalities returns unimplemented", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}],
			"generation_config": {"response_modalities": ["AUDIO"]}
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("StatusCode = %d, want 501", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "UNIMPLEMENTED" {
			t.Errorf("status = %q, want UNIMPLEMENTED", errResp.Error.Status)
		}
		if !strings.Contains(errResp.Error.Message, "generationConfig.responseModalities is not supported") {
			t.Errorf("message = %q, want responseModalities unsupported detail", errResp.Error.Message)
		}
	})

	t.Run("duplicate snake case alias returns invalid argument", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig": {"maxOutputTokens": 1},
			"generation_config": {"max_output_tokens": 1}
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "INVALID_ARGUMENT" {
			t.Errorf("status = %q, want INVALID_ARGUMENT", errResp.Error.Status)
		}
		if !strings.Contains(errResp.Error.Message, `request contains duplicate field "generationConfig"`) {
			t.Errorf("message = %q, want duplicate generationConfig detail", errResp.Error.Message)
		}
	})

	t.Run("unknown snake case field returns invalid argument", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}],
			"unexpected_field": true
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "INVALID_ARGUMENT" {
			t.Errorf("status = %q, want INVALID_ARGUMENT", errResp.Error.Status)
		}
		if !strings.Contains(errResp.Error.Message, "request.unexpected_field is not supported or unknown") {
			t.Errorf("message = %q, want unexpected_field detail", errResp.Error.Message)
		}
	})

	t.Run("unknown nested snake case field returns invalid argument", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		reqBody := `{
			"contents": [{"role":"user","parts":[{"text":"hi"}]}],
			"generation_config": {"unexpected_option": true}
		}`
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleGeminiModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
		}

		var errResp models.GeminiErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error.Status != "INVALID_ARGUMENT" {
			t.Errorf("status = %q, want INVALID_ARGUMENT", errResp.Error.Status)
		}
		if !strings.Contains(errResp.Error.Message, "request.generationConfig.unexpected_option is not supported or unknown") {
			t.Errorf("message = %q, want nested unexpected_option detail", errResp.Error.Message)
		}
	})
}

func floatPtr(v float64) *float64 {
	return &v
}
