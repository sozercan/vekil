package proxy

import (
	"encoding/json"
	"strings"
)

// Only effort changes on one physical upstream can reuse completed tool history.
// Provider/model changes and routes with failover retain the exact replay binding.
func policyCompletedReplayRoutes(profile *compiledPolicyProfile) []responsesChatReplayRoute {
	if profile == nil || profile.entry == nil || !policyProfileControlsReasoning(profile.config) {
		return nil
	}
	routes := []*modelRoute{profile.lightweight, profile.powerful}
	tiers := []policyTier{policyTierLightweight, policyTierPowerful}
	completed := make([]responsesChatReplayRoute, 0, len(routes))
	for index, route := range routes {
		if route == nil || len(route.targets) != 1 || !route.supportsEndpoint(providerEndpointResponses) {
			return nil
		}
		target := route.targets[0]
		if target.provider == nil || !target.provider.supportsEndpoint(providerEndpointResponses) {
			return nil
		}
		upstreamModel := strings.TrimSpace(target.upstreamModel)
		if upstreamModel == "" {
			upstreamModel = profile.entry.id
		}
		completed = append(completed, responsesChatReplayRoute{
			ProviderID:    target.provider.id,
			PublicModel:   profile.entry.id,
			UpstreamModel: upstreamModel,
			RouteID:       route.public.routeID,
			PolicyTier:    tiers[index].String(),
		})
	}
	if !sameResponsesChatReplayUpstream(completed[0], completed[1]) {
		return nil
	}
	return completed
}

func cloneResponsesChatReplayRoutes(routes []responsesChatReplayRoute) []responsesChatReplayRoute {
	return append([]responsesChatReplayRoute(nil), routes...)
}

func sameResponsesChatReplayUpstream(left, right responsesChatReplayRoute) bool {
	return left.ProviderID != "" && left.PublicModel != "" && left.UpstreamModel != "" &&
		left.ProviderID == right.ProviderID && left.PublicModel == right.PublicModel && left.UpstreamModel == right.UpstreamModel
}

// A fully completed prefix ends before a new user task. Every tool call in it
// must have a result before an assistant reply with no tools. A later unfinished
// turn keeps its exact binding without undoing an earlier completed prefix.
func completedResponsesChatPolicyReplayEnd(messages []json.RawMessage, resultIndices map[string]int) int {
	type historyMessage struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Refusal   json.RawMessage `json:"refusal"`
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	completedEnd := 0
	lastReply := -1
	lastToolResult := -1
	allCallsHaveResults := true
	for index, raw := range messages {
		var message historyMessage
		if json.Unmarshal(raw, &message) != nil {
			return 0
		}
		message.Role = strings.TrimSpace(message.Role)
		switch message.Role {
		case "user":
			content, err := translateChatMessageContent(message.Content, "user", index)
			if err != nil {
				return 0
			}
			if strings.TrimSpace(assistantHistoryText(content)) != "" && allCallsHaveResults && lastReply >= 0 && lastToolResult < lastReply {
				completedEnd = index
			}
		case "assistant":
			if len(message.ToolCalls) > 0 {
				for _, call := range message.ToolCalls {
					resultIndex, ok := resultIndices[strings.TrimSpace(call.ID)]
					if !ok || resultIndex <= index {
						allCallsHaveResults = false
					} else if resultIndex > lastToolResult {
						lastToolResult = resultIndex
					}
				}
				continue
			}
			content, err := translateChatMessageContent(message.Content, "assistant", index)
			if err != nil {
				return 0
			}
			refusal, err := translateChatAssistantRefusal(message.Refusal, index)
			if err != nil {
				return 0
			}
			if strings.TrimSpace(assistantHistoryText(content)+refusal) != "" {
				lastReply = index
			}
		}
	}
	return completedEnd
}

func chatRequestHasActiveResponsesPolicyReplay(body []byte) bool {
	request, err := decodeChatJSONObject(body, "")
	if err != nil {
		return true
	}
	var messages []json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil {
		return true
	}
	resultIndices, err := chatToolResultIndices(messages)
	if err != nil {
		return true
	}
	end := completedResponsesChatPolicyReplayEnd(messages, resultIndices)
	if end == 0 {
		return true
	}
	remaining, err := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
	}{Messages: messages[end:]})
	return err != nil || chatRequestContainsResponsesReplayID(remaining)
}

func restoreCompletedResponsesChatPolicyCalls(options responsesChatRequestOptions, projected []responsesChatReplayProjectedCall, content []map[string]any, tally *responsesChatRestoreTally) (responsesChatRestoredCalls, error) {
	// A completed-history exception still needs authoritative replay state. Probe
	// silently and never recover it from IDs or a degraded visible transcript.
	probe := options
	probe.Log = nil
	probe.DegradeUnrestorableReplay = false
	restored, err := restoreResponsesChatCalls(probe, projected, content, tally)
	if err == nil || !isMissingResponsesChatReplayError(err) {
		return restored, err
	}
	for _, route := range options.CompletedPolicyReplayRoutes {
		if route.equal(options.ReplayRoute) || !sameResponsesChatReplayUpstream(route, options.ReplayRoute) {
			continue
		}
		probe.ReplayRoute = route
		restored, err = restoreResponsesChatCalls(probe, projected, content, tally)
		if err == nil || !isMissingResponsesChatReplayError(err) {
			return restored, err
		}
	}
	return responsesChatRestoredCalls{}, err
}
