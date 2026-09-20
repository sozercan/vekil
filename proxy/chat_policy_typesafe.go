package proxy

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const policyTypeSafePromptGenerationVersion = "coding-agent-typesafe-choices-v1"

type policyTypeSafeQuestion struct {
	Type         string             `json:"type"`
	Instructions string             `json:"instructions"`
	Criteria     map[string]*string `json:"criteria"`
}

// TypeSafe-compatible endpoints use typed evaluations instead of Chat tools.
// Keep transport failures, admission, deadlines, and fallbacks in the shared
// HTTP classifier. Only the request and response formats differ.
func newPolicyTypeSafeClassifier(options policyHTTPClassifierOptions, send policyClassifierSendFunc) (*policyHTTPClassifier, error) {
	if options.ReasoningEffort != "" {
		return nil, fmt.Errorf("TypeSafe classification does not support reasoning_effort")
	}
	if options.MaxCompletionTokens != 0 {
		return nil, fmt.Errorf("TypeSafe classification does not support max_completion_tokens")
	}
	classifier, err := newPolicyHTTPClassifier(options, send)
	if err != nil {
		return nil, err
	}
	classifier.buildRequest = buildPolicyTypeSafeRequest
	classifier.parseResponse = parsePolicyTypeSafeResponse
	return classifier, nil
}

func buildPolicyTypeSafeRequest(options policyHTTPClassifierOptions, facts policyClassifierFacts) ([]byte, error) {
	factJSON, err := facts.marshal()
	if err != nil {
		return nil, err
	}
	if len(factJSON) > options.MaxFactsBytes {
		return nil, fmt.Errorf("serialized policy facts exceed max_request_bytes")
	}
	request := struct {
		Model     string                            `json:"model"`
		State     json.RawMessage                   `json:"state"`
		Questions map[string]policyTypeSafeQuestion `json:"questions"`
	}{options.Model, factJSON, policyTypeSafeQuestions()}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	// State is embedded JSON. The fixed questions have a separate wire bound,
	// including the task instructions repeated for each independent question.
	if len(body) > options.MaxFactsBytes+(32<<10) {
		return nil, fmt.Errorf("serialized TypeSafe classifier request exceeds wire bound")
	}
	return body, nil
}

func policyTypeSafeQuestions() map[string]policyTypeSafeQuestion {
	instructions := map[string]string{
		"abstain":                            "Choose true if the supplied facts do not support a reliable classification of the current task; otherwise choose false.",
		"turn_type":                          "Choose the primary intent of the current task for turn_type.",
		"code_scope":                         "Choose the code_scope affected by the current task. Use unknown when its scope cannot be determined.",
		"tool_call_count_estimate":           "Estimate the total number of tool calls needed for the current task, including reads and verification. Choose an integer from 0 to 128, capped at 128.",
		"modifying_tool_call_count_estimate": "Estimate the number of tool calls that modify files or external state for the current task. Exclude reads and verification. Choose an integer from 0 to 128, capped at 128.",
		"requires_codebase_context":          "Choose true if the current task requires broad codebase context beyond its explicit target; otherwise choose false.",
		"risk_level":                         "Choose the risk_level of the current task: low, medium, or high.",
	}
	properties := policyClassifierSignalSchema()["properties"].(map[string]any)
	questions := make(map[string]policyTypeSafeQuestion, len(properties))
	for name, rawProperty := range properties {
		property := rawProperty.(map[string]any)
		criteria := make(map[string]*string)
		switch property["type"] {
		case "boolean":
			criteria["false"], criteria["true"] = nil, nil
		case "integer":
			for value := property["minimum"].(int); value <= property["maximum"].(int); value++ {
				criteria[strconv.Itoa(value)] = nil
			}
		case "string":
			for _, value := range property["enum"].([]string) {
				criteria[value] = nil
			}
		}
		// Choices preserve the mapper's discrete inputs without treating
		// probabilities or confidence as a new routing policy.
		questions[name] = policyTypeSafeQuestion{
			Type: "choice", Instructions: policyClassifierTaskInstruction + " " + instructions[name], Criteria: criteria,
		}
	}
	return questions
}

func parsePolicyTypeSafeResponse(body []byte) (policyClassifierSignals, error) {
	root, err := decodePolicyClassifierObject(body)
	if err != nil {
		return policyClassifierSignals{}, invalidPolicyClassifierOutput(err)
	}
	answers, err := decodePolicyClassifierObject(root["answers"])
	if err != nil {
		return policyClassifierSignals{}, invalidPolicyClassifierOutput(err)
	}
	properties := policyClassifierSignalSchema()["properties"].(map[string]any)
	if len(answers) != len(properties) {
		return policyClassifierSignals{}, invalidPolicyClassifierOutput(fmt.Errorf("expected exactly one answer for each policy signal"))
	}
	signals := make(map[string]json.RawMessage, len(answers))
	for name, rawAnswer := range answers {
		rawProperty, ok := properties[name]
		if !ok {
			return policyClassifierSignals{}, invalidPolicyClassifierOutput(fmt.Errorf("unexpected policy signal"))
		}
		answer, err := decodePolicyClassifierObject(rawAnswer)
		if err != nil {
			return policyClassifierSignals{}, invalidPolicyClassifierOutput(err)
		}
		for field := range answer {
			switch field {
			case "type", "choice", "probabilities", "confidence":
			default:
				return policyClassifierSignals{}, invalidPolicyClassifierOutput(fmt.Errorf("unexpected choice answer field"))
			}
		}
		var answerType, choice string
		if json.Unmarshal(answer["type"], &answerType) != nil || answerType != "choice" ||
			json.Unmarshal(answer["choice"], &choice) != nil {
			return policyClassifierSignals{}, invalidPolicyClassifierOutput(fmt.Errorf("expected a choice answer"))
		}
		switch rawProperty.(map[string]any)["type"] {
		case "string":
			signals[name], _ = json.Marshal(choice)
		case "boolean":
			if choice != "true" && choice != "false" {
				return policyClassifierSignals{}, invalidPolicyClassifierOutput(fmt.Errorf("expected a boolean choice"))
			}
			signals[name] = json.RawMessage(choice)
		case "integer":
			value, err := parsePolicyClassifierBoundedInteger([]byte(choice))
			if err != nil || strconv.Itoa(value) != choice {
				return policyClassifierSignals{}, invalidPolicyClassifierOutput(fmt.Errorf("expected an integer choice in 0..128"))
			}
			signals[name] = json.RawMessage(choice)
		}
	}
	arguments, err := json.Marshal(signals)
	if err != nil {
		return policyClassifierSignals{}, invalidPolicyClassifierOutput(err)
	}
	return parsePolicyClassifierSignals(arguments)
}

func readPolicyTypeSafeUsage(body []byte) policyStatsTokenUsage {
	var response struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return policyStatsTokenUsage{}
	}
	return policyStatsTokenUsage{
		InputTokens: response.Usage.InputTokens, OutputTokens: response.Usage.OutputTokens,
	}.normalized()
}
