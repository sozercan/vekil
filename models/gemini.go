package models

import "encoding/json"

type GeminiGenerateContentRequest struct {
	Model             string                  `json:"model,omitempty"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	Contents          []GeminiContent         `json:"contents,omitempty"`
	Tools             []GeminiTool            `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
	CachedContent     string                  `json:"cachedContent,omitempty"`
	SafetySettings    json.RawMessage         `json:"safetySettings,omitempty"`
	Labels            map[string]string       `json:"labels,omitempty"`
}

type GeminiCountTokensRequest = GeminiGenerateContentRequest

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts,omitempty"`
}

type GeminiPart struct {
	Text                *string                 `json:"text,omitempty"`
	Thought             *bool                   `json:"thought,omitempty"`
	ThoughtSignature    string                  `json:"thoughtSignature,omitempty"`
	FunctionCall        *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse    *GeminiFunctionResponse `json:"functionResponse,omitempty"`
	InlineData          json.RawMessage         `json:"inlineData,omitempty"`
	FileData            json.RawMessage         `json:"fileData,omitempty"`
	ExecutableCode      json.RawMessage         `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage         `json:"codeExecutionResult,omitempty"`
}

type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type GeminiFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type GeminiFunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
	Parts    json.RawMessage `json:"parts,omitempty"`
}

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

type GeminiFunctionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type GeminiToolConfig struct {
	FunctionCallingConfig *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

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

type GeminiGenerateContentResponse struct {
	Candidates    []GeminiCandidate    `json:"candidates,omitempty"`
	UsageMetadata *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
}

type GeminiCandidate struct {
	Content      *GeminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        *int           `json:"index,omitempty"`
}

type GeminiUsageMetadata struct {
	PromptTokenCount     int                       `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int                       `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int                       `json:"totalTokenCount,omitempty"`
	PromptTokensDetails  []GeminiTokenCountDetails `json:"promptTokensDetails,omitempty"`
}

type GeminiTokenCountDetails struct {
	Modality   string `json:"modality"`
	TokenCount int    `json:"tokenCount"`
}

type GeminiCountTokensResponse struct {
	TotalTokens         int                       `json:"totalTokens"`
	PromptTokensDetails []GeminiTokenCountDetails `json:"promptTokensDetails,omitempty"`
}

type GeminiErrorResponse struct {
	Error GeminiError `json:"error"`
}

type GeminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}
