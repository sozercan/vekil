package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

func (s *responsesWebSocketSession) postConversationCreateRequest(h *ProxyHandler, ctx context.Context, request *responsesWebSocketCreateRequest, plan responsesWebSocketRequestPlan) (*http.Response, error) {
	operation := routeOperationFromContext(ctx)
	input := [][]json.RawMessage{request.Input}
	previousID := request.PreviousResponseID
	if strings.HasPrefix(request.PreviousResponseID, "vekil-ws-") {
		// generate:false staging remains connection-local and does not claim a
		// saved generation. Its full input is available on this connection.
		previousID = ""
		input = plan.fullReplaySegments
		if plan.conversationSourceID != "" {
			source, err := h.conversationHistory.lookupResponse(operation.route.public.routeID, plan.conversationSourceID)
			if err != nil {
				return nil, conversationRequestError(err)
			}
			var full []json.RawMessage
			for _, segment := range input {
				full = append(full, segment...)
			}
			catalogs, history := partitionResponsesAdditionalToolsInputItems(full)
			if !conversationHasPrefix(history, source.Input) {
				return nil, conversationRequestError(errConversationHistoryPartial)
			}
			input = [][]json.RawMessage{catalogs, history[len(source.Input):]}
			previousID = source.ResponseID
		}
	}
	body, err := request.upstreamBody(input...)
	if err != nil {
		return nil, err
	}
	body, err = responsesWebSocketStateValidationBody(body, previousID)
	if err != nil {
		return nil, err
	}
	headers := s.requestHeaders(request, false)
	if plan.conversationComplete && headerGetCI(headers, "X-Vekil-History-Complete") == "" {
		// A staged independent import keeps its assertion until generation.
		// The request planner clears it when the client starts a new chain.
		headers.Set("X-Vekil-History-Complete", "true")
	}
	body, headers, err = h.prepareConversationTurn(operation, body, headers)
	if err != nil {
		return nil, err
	}
	if err := h.applyExplicitRequestStateBinding(operation, body, headers); err != nil {
		return nil, err
	}
	// Unsupported hosted state leaves the turn unprotected without a conversation.
	if turn := operation.conversation; turn != nil {
		turn.toolContexts, turn.toolScope = s.toolContexts, s.toolScope
	}
	body = h.rewriteResponsesRequestBodyWithToolOptimizersForModel(ctx, body, request.Model, "responses/websocket", true, s.toolContexts, s.toolScope)
	resp, err := h.postResponsesWithHeadersForModel(ctx, body, headers, request.Model)
	attachResponsesWebSocketOperationID(resp, operation)
	return resp, err
}
