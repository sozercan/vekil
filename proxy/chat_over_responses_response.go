package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/models"
)

const responsesChatMaxJSONBodyBytes = 16 << 20

type responsesChatResponseOptions struct {
	PublicModel string
	ReplayStore *responsesChatReplayStore
	ReplayRoute responsesChatReplayRoute
	UsageOnly   bool
}

type responsesChatJSONResult struct {
	Response *models.OpenAIResponse
	Body     []byte
	Usage    *models.OpenAIUsage
}

type responsesChatJSONEnvelope struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	Error     *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []json.RawMessage `json:"output"`
	Usage  *responsesUsage   `json:"usage"`
}

func translateResponsesJSONToChat(body []byte, options responsesChatResponseOptions) (result responsesChatJSONResult, err error) {
	if len(body) > responsesChatMaxJSONBodyBytes {
		return responsesChatJSONResult{}, newChatServerError("responses_body_too_large", "upstream Responses body exceeds the converted JSON limit")
	}
	var envelope responsesChatJSONEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return responsesChatJSONResult{}, newChatServerError("invalid_responses_body", "upstream returned malformed Responses JSON")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return responsesChatJSONResult{}, newChatServerError("invalid_responses_body", "upstream returned trailing or malformed Responses JSON")
	}

	var usage *models.OpenAIUsage
	if envelope.Usage != nil {
		usage = envelope.Usage.toOpenAIUsage()
	}
	defer func() {
		attachChatExecutionErrorUsage(err, usage)
	}()
	if strings.TrimSpace(envelope.Status) == "failed" {
		return responsesChatJSONResult{}, responsesChatFailedExecutionError(envelope.Error, usage)
	}
	if envelope.Error != nil {
		return responsesChatJSONResult{}, newChatServerError("invalid_responses_body", "successful or incomplete Responses body unexpectedly contains an error")
	}

	content := strings.Builder{}
	refusal := strings.Builder{}
	functionCalls := make([]responsesChatParsedFunctionCall, 0)
	sawIncompleteFunctionCall := false
	if !options.UsageOnly {
		for index, rawItem := range envelope.Output {
			var header struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(rawItem, &header); err != nil {
				return responsesChatJSONResult{}, newChatServerError("unsupported_responses_output", fmt.Sprintf("upstream output item %d is malformed", index))
			}
			switch strings.TrimSpace(header.Type) {
			case "reasoning":
				if err := validateResponsesChatReasoningStatus(envelope.Status, header.Status); err != nil {
					return responsesChatJSONResult{}, err
				}
				// Hidden from Chat. Exact bytes are retained only when a later
				// function-call group is published for replay.
			case "message":
				if err := validateResponsesChatMessageStatus(envelope.Status, header.Status); err != nil {
					return responsesChatJSONResult{}, err
				}
				text, refusalText, err := responsesChatMessageContent(rawItem)
				if err != nil {
					return responsesChatJSONResult{}, err
				}
				content.WriteString(text)
				refusal.WriteString(refusalText)
			case "function_call":
				call, err := parseResponsesChatFunctionCall(rawItem, index)
				if err != nil {
					return responsesChatJSONResult{}, err
				}
				completed, err := responsesChatFunctionCallCompleted(envelope.Status, call.Status)
				if err != nil {
					return responsesChatJSONResult{}, err
				}
				if !completed {
					sawIncompleteFunctionCall = true
					continue
				}
				if call.UpstreamCallID == "" || call.Name == "" || !call.ArgumentsPresent {
					return responsesChatJSONResult{}, newChatServerError("unsupported_responses_output", "completed function-call output item is incomplete")
				}
				functionCalls = append(functionCalls, call)
			case "":
				return responsesChatJSONResult{}, newChatServerError("unsupported_responses_output", fmt.Sprintf("upstream output item %d has no type", index))
			default:
				return responsesChatJSONResult{}, newChatServerError("unsupported_responses_output", fmt.Sprintf("upstream output item type %q is not supported", header.Type))
			}
		}
	}
	if sawIncompleteFunctionCall {
		functionCalls = nil
	}

	contentRaw, _ := json.Marshal(content.String())
	if len(functionCalls) > 0 && refusal.Len() > 0 {
		return responsesChatJSONResult{}, newChatServerError("unsupported_responses_output", "Responses tool-call turns with refusal content are not supported")
	}
	finishReason, err := responsesChatFinishReason(envelope.Status, envelope.IncompleteDetails, len(functionCalls) > 0)
	if err != nil {
		return responsesChatJSONResult{}, err
	}
	chatToolCalls := make([]models.OpenAIToolCall, 0, len(functionCalls))
	if len(functionCalls) > 0 {
		if options.ReplayStore == nil {
			replayErr := newChatServerError("responses_replay_unavailable", "Responses replay storage is unavailable")
			attachChatExecutionErrorUsage(replayErr, usage)
			return responsesChatJSONResult{}, replayErr
		}
		// Chat-style surfaces intentionally do not run command_rewrite; they preserve
		// upstream arguments and only capture tool context for later output reduction.
		publishCalls := make([]responsesChatReplayPublishCall, len(functionCalls))
		for i, call := range functionCalls {
			publishCalls[i] = responsesChatReplayPublishCall{
				UpstreamCallID:   call.UpstreamCallID,
				Name:             call.Name,
				VisibleArguments: call.Arguments,
				OutputItemIndex:  call.OutputItemIndex,
			}
		}
		published, err := options.ReplayStore.Publish(responsesChatReplayPublishRequest{
			Route:            options.ReplayRoute,
			AssistantContent: contentRaw,
			OutputItems:      envelope.Output,
			Calls:            publishCalls,
		})
		if err != nil {
			replayErr := mapResponsesChatReplayPublishError(err)
			attachChatExecutionErrorUsage(replayErr, usage)
			return responsesChatJSONResult{}, replayErr
		}
		for _, call := range published.Projection.Calls {
			chatToolCalls = append(chatToolCalls, models.OpenAIToolCall{
				ID:       call.ID,
				Type:     "function",
				Function: models.OpenAIFunctionCall{Name: call.Name, Arguments: call.Arguments},
			})
		}
	}
	var refusalRaw json.RawMessage
	if refusal.Len() > 0 {
		refusalRaw, _ = json.Marshal(refusal.String())
	}
	response := &models.OpenAIResponse{
		ID:      responsesChatCompletionID(envelope.ID),
		Object:  openAIChatCompletionObject,
		Created: envelope.CreatedAt,
		Model:   strings.TrimSpace(options.PublicModel),
		Choices: []models.OpenAIChoice{{
			Index: 0,
			Message: models.OpenAIMessage{
				Role:      "assistant",
				Content:   contentRaw,
				Refusal:   refusalRaw,
				ToolCalls: chatToolCalls,
			},
			FinishReason: &finishReason,
		}},
	}
	response.Usage = usage
	normalizeOpenAIChatCompletionStruct(response, options.PublicModel)
	encoded, err := json.Marshal(response)
	if err != nil {
		return responsesChatJSONResult{}, fmt.Errorf("marshal canonical Chat response: %w", err)
	}
	if len(encoded) > responsesChatMaxJSONBodyBytes {
		return responsesChatJSONResult{}, newChatServerError("chat_body_too_large", "converted Chat response exceeds the JSON limit")
	}
	return responsesChatJSONResult{Response: response, Body: encoded, Usage: usage}, nil
}

func validateResponsesChatMessageStatus(responseStatus, messageStatus string) error {
	responseStatus = strings.TrimSpace(responseStatus)
	messageStatus = strings.TrimSpace(messageStatus)
	switch responseStatus {
	case "completed", "incomplete":
		if messageStatus != responseStatus {
			return newChatServerError(
				"unsupported_responses_output",
				fmt.Sprintf("assistant message status %q is incompatible with Responses status %q", messageStatus, responseStatus),
			)
		}
	}
	return nil
}

func validateResponsesChatReasoningStatus(responseStatus, reasoningStatus string) error {
	responseStatus = strings.TrimSpace(responseStatus)
	reasoningStatus = strings.TrimSpace(reasoningStatus)
	if reasoningStatus == "" {
		return nil
	}
	if reasoningStatus != "completed" && reasoningStatus != "incomplete" && reasoningStatus != "in_progress" {
		return newChatServerError(
			"unsupported_responses_output",
			fmt.Sprintf("reasoning item status %q is not supported", reasoningStatus),
		)
	}
	switch responseStatus {
	case "completed":
		if reasoningStatus != "completed" {
			return newChatServerError(
				"unsupported_responses_output",
				fmt.Sprintf("reasoning item status %q is incompatible with Responses status %q", reasoningStatus, responseStatus),
			)
		}
	case "incomplete":
		if reasoningStatus == "in_progress" {
			return newChatServerError(
				"unsupported_responses_output",
				fmt.Sprintf("reasoning item status %q is incompatible with terminal Responses status %q", reasoningStatus, responseStatus),
			)
		}
	}
	return nil
}

func normalizeResponsesChatTerminalFunctionCallStatus(status string) (string, error) {
	status = strings.TrimSpace(status)
	switch status {
	case "", "completed", "incomplete":
		return status, nil
	default:
		return "", newChatServerError("unsupported_responses_output", fmt.Sprintf("terminal function-call status %q is not supported", status))
	}
}

func responsesChatFunctionCallCompleted(responseStatus, callStatus string) (bool, error) {
	callStatus, err := normalizeResponsesChatTerminalFunctionCallStatus(callStatus)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(responseStatus) {
	case "completed":
		if callStatus == "" || callStatus == "completed" {
			return true, nil
		}
		return false, newChatServerError("unsupported_responses_output", "incomplete function call appeared in a completed Responses result")
	case "incomplete":
		return callStatus == "completed", nil
	default:
		return callStatus == "completed", nil
	}
}

func responsesChatMessageContent(raw json.RawMessage) (string, string, error) {
	var item struct {
		Role    string `json:"role"`
		Content []struct {
			Type    string  `json:"type"`
			Text    *string `json:"text"`
			Refusal *string `json:"refusal"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", "", newChatServerError("unsupported_responses_output", "assistant message item is malformed")
	}
	if strings.TrimSpace(item.Role) != "assistant" {
		return "", "", newChatServerError("unsupported_responses_output", "Responses message item is not from the assistant")
	}
	var text strings.Builder
	var refusal strings.Builder
	for _, part := range item.Content {
		switch part.Type {
		case "output_text":
			if part.Text == nil {
				return "", "", newChatServerError("unsupported_responses_output", "Responses output_text content is missing a string text field")
			}
			text.WriteString(*part.Text)
		case "refusal":
			if part.Refusal == nil {
				return "", "", newChatServerError("unsupported_responses_output", "Responses refusal content is missing a string refusal field")
			}
			refusal.WriteString(*part.Refusal)
		default:
			return "", "", newChatServerError("unsupported_responses_output", fmt.Sprintf("Responses content type %q is not supported", part.Type))
		}
	}
	return text.String(), refusal.String(), nil
}

func responsesChatFinishReason(status string, details *struct {
	Reason string `json:"reason"`
}, hasCalls bool) (string, error) {
	switch strings.TrimSpace(status) {
	case "completed":
		if hasCalls {
			return "tool_calls", nil
		}
		return "stop", nil
	case "incomplete":
		if details != nil {
			switch strings.TrimSpace(details.Reason) {
			case "max_output_tokens":
				if hasCalls {
					return "tool_calls", nil
				}
				return "length", nil
			case "content_filter":
				if hasCalls {
					return "tool_calls", nil
				}
				return "content_filter", nil
			}
		}
		return "", newChatServerError("response_incomplete", "upstream Responses generation was incomplete")
	case "failed":
		return "", newChatServerError("response_failed", "upstream Responses generation failed")
	default:
		return "", newChatServerError("unsupported_response_status", fmt.Sprintf("upstream Responses status %q is not supported", status))
	}
}

func responsesChatCompletionID(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	responseID = strings.TrimPrefix(responseID, "resp_")
	if responseID == "" {
		responseID = uuid.NewString()
	}
	return "chatcmpl-" + responseID
}

func newChatServerError(code, message string) *chatExecutionError {
	return &chatExecutionError{
		StatusCode: http.StatusBadGateway,
		Type:       "server_error",
		Code:       code,
		Message:    message,
	}
}

type responsesChatParsedFunctionCall struct {
	UpstreamCallID   string
	Name             string
	Arguments        string
	ArgumentsPresent bool
	Status           string
	OutputItemIndex  int
}

func parseResponsesChatFunctionCall(raw json.RawMessage, outputIndex int) (responsesChatParsedFunctionCall, error) {
	var call struct {
		Type      string  `json:"type"`
		CallID    string  `json:"call_id"`
		Name      string  `json:"name"`
		Arguments *string `json:"arguments"`
		Status    string  `json:"status"`
	}
	if err := json.Unmarshal(raw, &call); err != nil || call.Type != "function_call" {
		return responsesChatParsedFunctionCall{}, newChatServerError("unsupported_responses_output", "function-call output item is malformed")
	}
	call.CallID = strings.TrimSpace(call.CallID)
	call.Name = strings.TrimSpace(call.Name)
	call.Status = strings.TrimSpace(call.Status)
	if call.Status == "completed" && (call.CallID == "" || call.Name == "" || call.Arguments == nil) {
		return responsesChatParsedFunctionCall{}, newChatServerError("unsupported_responses_output", "completed function-call output item is incomplete")
	}
	arguments := ""
	if call.Arguments != nil {
		arguments = *call.Arguments
	}
	return responsesChatParsedFunctionCall{
		UpstreamCallID:   call.CallID,
		Name:             call.Name,
		Arguments:        arguments,
		ArgumentsPresent: call.Arguments != nil,
		Status:           call.Status,
		OutputItemIndex:  outputIndex,
	}, nil
}

func mapResponsesChatReplayPublishError(err error) error {
	switch err.(type) {
	case *responsesChatReplayTooLargeError:
		return &chatExecutionError{StatusCode: http.StatusBadGateway, Type: "server_error", Code: "responses_replay_state_too_large", Message: "Responses-backed tool replay state exceeds configured limits."}
	case *responsesChatReplayClosedError:
		return &chatExecutionError{StatusCode: http.StatusServiceUnavailable, Type: "server_error", Code: responsesChatReplayClosedCode, Message: responsesChatReplayClosedMessage}
	default:
		return &chatExecutionError{StatusCode: http.StatusBadGateway, Type: "server_error", Code: "responses_replay_state_invalid", Message: "Upstream returned invalid Responses tool replay state."}
	}
}

func responsesChatFailedExecutionError(failure *struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}, usage *models.OpenAIUsage) *chatExecutionError {
	errorType, code, message := "server_error", "response_failed", "upstream Responses generation failed"
	if failure != nil {
		if strings.TrimSpace(failure.Type) != "" {
			errorType = strings.TrimSpace(failure.Type)
		}
		if strings.TrimSpace(failure.Code) != "" {
			code = strings.TrimSpace(failure.Code)
		}
		if strings.TrimSpace(failure.Message) != "" {
			message = strings.TrimSpace(failure.Message)
		}
	}
	if failure == nil || strings.TrimSpace(failure.Type) == "" {
		errorType = responsesChatErrorTypeForCode(code)
	}
	status := responsesChatFailureStatus(errorType, code)
	return &chatExecutionError{StatusCode: status, Type: errorType, Code: code, Message: message, Usage: usage}
}

func responsesChatErrorTypeForCode(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "too_many_requests", "rate_limit_exceeded", "rate_limit_error":
		return "rate_limit_error"
	case "invalid_prompt", "bio_policy", "invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url",
		"image_too_large", "image_too_small", "image_parse_error", "image_content_policy_violation", "invalid_image_mode",
		"image_file_too_large", "unsupported_image_media_type", "empty_image_file", "failed_to_download_image", "image_file_not_found":
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

func responsesChatFailureStatus(errorType, code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "too_many_requests", "rate_limit_exceeded", "rate_limit_error":
		return http.StatusTooManyRequests
	case "model_overloaded", "engine_overloaded", "overloaded_error", "service_unavailable":
		return http.StatusServiceUnavailable
	case "timeout", "gateway_timeout", "vector_store_timeout":
		return http.StatusGatewayTimeout
	case "bad_gateway":
		return http.StatusBadGateway
	case "invalid_prompt", "bio_policy", "invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url",
		"image_too_large", "image_too_small", "image_parse_error", "image_content_policy_violation", "invalid_image_mode",
		"image_file_too_large", "unsupported_image_media_type", "empty_image_file", "failed_to_download_image", "image_file_not_found":
		return http.StatusBadRequest
	}
	switch strings.ToLower(strings.TrimSpace(errorType)) {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "conflict_error":
		return http.StatusConflict
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}
