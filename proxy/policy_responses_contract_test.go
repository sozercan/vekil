package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func testResponsesChatTranslationContract() responsesChatTranslationContract {
	return responsesChatTranslationContract{
		label:              "test",
		subject:            "test models",
		maxRequestBytes:    4 << 20,
		maxTranslatedBytes: 8 << 20,
		maxInputItems:      4,
	}
}

func requireResponsesTranslationError(t *testing.T, err error, param, message string) {
	t.Helper()
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("error = %T %v, want *chatExecutionError", err, err)
	}
	if executionErr.Param != param || executionErr.Message != message {
		t.Fatalf("error param/message = %q/%q, want %q/%q", executionErr.Param, executionErr.Message, param, message)
	}
}

func TestPolicyResponsesTranslationContractPreservesPolicyMessages(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		param   string
		message string
	}{
		{
			name:    "store",
			body:    `{"model":"m","input":"hi","store":true}`,
			param:   "store",
			message: "store must be false or omitted for policy models",
		},
		{
			name:    "previous response",
			body:    `{"model":"m","input":"hi","previous_response_id":"resp_1"}`,
			param:   "previous_response_id",
			message: "previous_response_id is not supported for policy models",
		},
		{
			name:    "image content",
			body:    `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:,"}]}]}`,
			param:   "input[0].content[0].type",
			message: "image content is not supported for policy models",
		},
		{
			name:    "reasoning item",
			body:    `{"model":"m","input":[{"type":"reasoning","summary":[]}]}`,
			param:   "input[0].type",
			message: `Responses input item type "reasoning" is not supported for policy models`,
		},
		{
			name:    "hosted tool",
			body:    `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`,
			param:   "tools[0].type",
			message: `hosted or custom tool type "web_search" is not supported for policy models`,
		},
		{
			name:    "deferred tool",
			body:    `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{},"defer_loading":true}]}`,
			param:   "tools[0].defer_loading",
			message: "deferred tool loading is not supported for policy models",
		},
		{
			name:    "minimum output tokens",
			body:    `{"model":"m","input":"hi","max_output_tokens":1}`,
			param:   "max_output_tokens",
			message: fmt.Sprintf("max_output_tokens must be at least %d for policy models", responsesChatMinimumOutputTokens),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := translatePolicyResponsesRequestToChat([]byte(tc.body))
			requireResponsesTranslationError(t, err, tc.param, tc.message)
		})
	}
}

func TestPolicyResponsesTranslationContractKeepsPolicyLimits(t *testing.T) {
	oversized := `{"model":"m","input":"` + strings.Repeat("a", policyResponsesMaxRequestBytes) + `"}`
	_, err := translatePolicyResponsesRequestToChat([]byte(oversized))
	requireResponsesTranslationError(t, err, "", fmt.Sprintf("policy Responses request exceeds %d bytes", policyResponsesMaxRequestBytes))

	items := make([]string, policyResponsesMaxInputItems+1)
	for i := range items {
		items[i] = `{"type":"message","role":"user","content":"x"}`
	}
	_, err = translatePolicyResponsesRequestToChat([]byte(`{"model":"m","input":[` + strings.Join(items, ",") + `]}`))
	requireResponsesTranslationError(t, err, "input", fmt.Sprintf("input may contain at most %d items", policyResponsesMaxInputItems))
}

func TestResponsesChatTranslationContractOwnsLimitsAndMessages(t *testing.T) {
	contract := testResponsesChatTranslationContract()

	large := strings.Repeat("b", policyResponsesMaxRequestBytes+1024)
	translated, err := translateResponsesRequestToChat([]byte(`{"model":"m","input":"`+large+`"}`), contract)
	if err != nil {
		t.Fatalf("translateResponsesRequestToChat() error = %v", err)
	}
	var chat policyResponsesCanonicalChatRequest
	if err := json.Unmarshal(translated.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || string(chat.Messages[0].Content) != `"`+large+`"` {
		t.Fatalf("translated message was not preserved intact")
	}

	_, err = translateResponsesRequestToChat([]byte(`{"model":"m","input":"hi","store":true}`), contract)
	requireResponsesTranslationError(t, err, "store", "store must be false or omitted for test models")

	_, err = translateResponsesRequestToChat([]byte(`{"model":"m","input":"hi","tools":[{"type":"custom","name":"apply_patch"}]}`), contract)
	requireResponsesTranslationError(t, err, "tools[0].type", `hosted or custom tool type "custom" is not supported for test models`)

	items := make([]string, contract.maxInputItems+1)
	for i := range items {
		items[i] = `{"type":"message","role":"user","content":"x"}`
	}
	_, err = translateResponsesRequestToChat([]byte(`{"model":"m","input":[`+strings.Join(items, ",")+`]}`), contract)
	requireResponsesTranslationError(t, err, "input", fmt.Sprintf("input may contain at most %d items", contract.maxInputItems))

	oversized := `{"model":"m","input":"` + strings.Repeat("c", contract.maxRequestBytes) + `"}`
	_, err = translateResponsesRequestToChat([]byte(oversized), contract)
	requireResponsesTranslationError(t, err, "", fmt.Sprintf("test Responses request exceeds %d bytes", contract.maxRequestBytes))
}

func TestResponsesChatTranslationContractOwnsTranslatedByteLimits(t *testing.T) {
	contract := testResponsesChatTranslationContract()
	contract.maxTranslatedBytes = 100

	t.Run("canonical Chat body", func(t *testing.T) {
		body := `{"model":"m","input":"` + strings.Repeat("x", 72) + `"}`
		_, err := translateResponsesRequestToChat([]byte(body), contract)
		requireResponsesTranslationError(t, err, "", "translated test Chat request exceeds 100 bytes")
	})

	t.Run("flattened namespace descriptions", func(t *testing.T) {
		tools := json.RawMessage(`[{"type":"namespace","name":"ns","description":"` + strings.Repeat("x", 60) + `","tools":[` +
			`{"type":"function","name":"first","parameters":{"type":"object"}},` +
			`{"type":"function","name":"second","parameters":{"type":"object"}}]}]`)
		_, err := parseResponsesToolsWithContract(tools, contract)
		requireResponsesTranslationError(t, err, "tools[0].tools[1].description", "flattened tool descriptions exceed 100 encoded bytes")
	})
}

func TestPolicyResponsesToolParserWrapperUsesPolicyContract(t *testing.T) {
	_, err := parsePolicyResponsesTools(json.RawMessage(`[{"type":"image_generation"}]`))
	requireResponsesTranslationError(t, err, "tools[0].type", `hosted or custom tool type "image_generation" is not supported for policy models`)

	_, err = parseResponsesToolsWithContract(json.RawMessage(`[{"type":"image_generation"}]`), testResponsesChatTranslationContract())
	requireResponsesTranslationError(t, err, "tools[0].type", `hosted or custom tool type "image_generation" is not supported for test models`)
}
