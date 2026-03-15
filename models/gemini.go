package models

import "encoding/json"

// GeminiGenerateContentRequest is the supported subset of the Gemini
// generateContent request shape.
type GeminiGenerateContentRequest struct {
	Model             string                  `json:"model,omitempty"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	Contents          []GeminiContent         `json:"contents,omitempty"`
	Tools             []GeminiTool            `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
	CachedContent     string                  `json:"cachedContent,omitempty"`
	SafetySettings    json.RawMessage         `json:"safetySettings,omitempty"`
}

// GeminiCountTokensRequest reuses the supported generateContent subset.
type GeminiCountTokensRequest = GeminiGenerateContentRequest

// GeminiContent is a role-tagged list of content parts.
type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts,omitempty"`
}

// GeminiPart is the supported subset of Gemini content parts.
type GeminiPart struct {
	Text                *string                 `json:"text,omitempty"`
	FunctionCall        *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse    *GeminiFunctionResponse `json:"functionResponse,omitempty"`
	InlineData          json.RawMessage         `json:"inlineData,omitempty"`
	FileData            json.RawMessage         `json:"fileData,omitempty"`
	ExecutableCode      json.RawMessage         `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage         `json:"codeExecutionResult,omitempty"`
}

// GeminiInlineData is a base64-encoded inline media blob.
type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// GeminiFunctionCall is a model-issued function call.
type GeminiFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// GeminiFunctionResponse is a user-supplied tool result.
type GeminiFunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
	Parts    json.RawMessage `json:"parts,omitempty"`
}

// GeminiTool is the supported subset of Gemini tool declarations.
type GeminiTool struct {
	FunctionDeclarations  []GeminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
	GoogleSearch          json.RawMessage             `json:"googleSearch,omitempty"`
	GoogleSearchRetrieval json.RawMessage             `json:"googleSearchRetrieval,omitempty"`
	URLContext            json.RawMessage             `json:"urlContext,omitempty"`
	CodeExecution         json.RawMessage             `json:"codeExecution,omitempty"`
	GoogleMaps            json.RawMessage             `json:"googleMaps,omitempty"`
	ComputerUse           json.RawMessage             `json:"computerUse,omitempty"`
	EnterpriseWebSearch   json.RawMessage             `json:"enterpriseWebSearch,omitempty"`
}

// GeminiFunctionDeclaration is a tool schema declaration.
type GeminiFunctionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

// GeminiToolConfig configures tool selection.
type GeminiToolConfig struct {
	FunctionCallingConfig *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// GeminiFunctionCallingConfig is the supported function-calling subset.
type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// GeminiGenerationConfig controls sampling and structured output.
type GeminiGenerationConfig struct {
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	TopK               *int            `json:"topK,omitempty"`
	MaxOutputTokens    *int            `json:"maxOutputTokens,omitempty"`
	StopSequences      []string        `json:"stopSequences,omitempty"`
	ResponseMimeType   string          `json:"responseMimeType,omitempty"`
	ResponseSchema     json.RawMessage `json:"responseSchema,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
	ThinkingConfig     json.RawMessage `json:"thinkingConfig,omitempty"`
	CandidateCount     *int            `json:"candidateCount,omitempty"`
	PresencePenalty    *float64        `json:"presencePenalty,omitempty"`
	FrequencyPenalty   *float64        `json:"frequencyPenalty,omitempty"`
	Seed               *int            `json:"seed,omitempty"`
	ResponseModalities json.RawMessage `json:"responseModalities,omitempty"`
	SpeechConfig       json.RawMessage `json:"speechConfig,omitempty"`
	ImageConfig        json.RawMessage `json:"imageConfig,omitempty"`
	MediaResolution    json.RawMessage `json:"mediaResolution,omitempty"`
	ResponseLogprobs   json.RawMessage `json:"responseLogprobs,omitempty"`
	Logprobs           json.RawMessage `json:"logprobs,omitempty"`
}

// GeminiGenerateContentResponse is the supported Gemini response envelope.
type GeminiGenerateContentResponse struct {
	Candidates    []GeminiCandidate    `json:"candidates,omitempty"`
	UsageMetadata *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
}

// GeminiCandidate is a single candidate in a Gemini response.
type GeminiCandidate struct {
	Content      *GeminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        *int           `json:"index,omitempty"`
}

// GeminiUsageMetadata holds token usage for Gemini responses.
type GeminiUsageMetadata struct {
	PromptTokenCount     int                       `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int                       `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int                       `json:"totalTokenCount,omitempty"`
	PromptTokensDetails  []GeminiTokenCountDetails `json:"promptTokensDetails,omitempty"`
}

// GeminiTokenCountDetails is a modality-specific token count.
type GeminiTokenCountDetails struct {
	Modality   string `json:"modality"`
	TokenCount int    `json:"tokenCount"`
}

// GeminiCountTokensResponse is the supported countTokens response shape.
type GeminiCountTokensResponse struct {
	TotalTokens         int                       `json:"totalTokens"`
	PromptTokensDetails []GeminiTokenCountDetails `json:"promptTokensDetails,omitempty"`
}

// GeminiErrorResponse is the Google-style error envelope.
type GeminiErrorResponse struct {
	Error GeminiError `json:"error"`
}

// GeminiError contains the Gemini error body.
type GeminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}
