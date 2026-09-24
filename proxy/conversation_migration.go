package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/logger"
)

// A turn owns one conversation admission until its response body has finished.
// The durable pending marker survives a crash in the dispatch/exposure gap.
type conversationTurn struct {
	mu              sync.Mutex
	h               *ProxyHandler
	store           *conversationHistoryStore
	operation       *routeOperation
	source          *conversationSnapshot
	root            string
	scope           string
	input           []json.RawMessage
	fields          map[string]json.RawMessage
	instructions    json.RawMessage
	tools           json.RawMessage
	additionalTools []json.RawMessage
	toolContexts    *ToolExecutionContextStore
	toolScope       string
	hostedTools     map[string]bool
	pending         bool
	dispatched      bool
	saved           bool
	closed          bool
	migrated        bool
	attempted       bool
	blocked         bool
	unprotected     bool
}

func (h *ProxyHandler) initializeConversationHistory() error {
	if h.providersConfig.ConversationMigration == nil {
		return nil
	}
	store, err := newConversationHistoryStore(h.stateBindings.durable, *h.providersConfig.ConversationMigration)
	if err != nil {
		return err
	}
	h.conversationHistory = store
	return nil
}

func (h *ProxyHandler) conversationMigrationEnabled(route *modelRoute) bool {
	if h == nil || h.conversationHistory == nil || route == nil || route.legacy {
		return false
	}
	for _, routeID := range h.conversationHistory.config.Routes {
		if routeID == route.public.routeID {
			return true
		}
	}
	return false
}

func conversationTurnFromContext(ctx context.Context) *conversationTurn {
	operation := routeOperationFromContext(ctx)
	if operation == nil {
		return nil
	}
	return operation.conversation
}

func (h *ProxyHandler) prepareConversationTurn(operation *routeOperation, body []byte, headers http.Header) ([]byte, http.Header, error) {
	if operation == nil || !h.conversationMigrationEnabled(operation.route) || operation.conversation != nil {
		return body, headers, nil
	}
	fail := func(err error) ([]byte, http.Header, error) {
		h.logConversationRecovery(operation, "blocked", "", conversationFailureReason(err))
		return nil, nil, conversationRequestError(err)
	}
	if err := validateUnambiguousResponsesJSON(body); err != nil {
		return fail(errConversationHistoryPartial)
	}
	// Validate the original state fields before any reconstruction removes them.
	if _, err := extractResponsesRequestState(body, headers, true); err != nil {
		return nil, nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return fail(errConversationHistoryPartial)
	}
	// Hosted state that this history format cannot replay does not block the
	// turn. It runs on the normal route without a snapshot, so later failover
	// cannot reconstruct it; contract violations of readable history still fail.
	unprotected := func(err error) ([]byte, http.Header, error) {
		if !conversationUnsupportedState(err) {
			return fail(err)
		}
		h.logConversationRecovery(operation, "unprotected", "", conversationFailureReason(err))
		return body, headers, nil
	}
	if err := validateConversationRequestFields(fields); err != nil {
		return unprotected(err)
	}
	input, err := canonicalConversationInput(fields["input"], false)
	if err != nil {
		return unprotected(err)
	}
	store := h.conversationHistory
	routeID := operation.route.public.routeID
	previousID := rawJSONString(fields["previous_response_id"])
	assertedComplete := headerGetCI(headers, "X-Vekil-History-Complete") == "true"
	if value := headerGetCI(headers, "X-Vekil-History-Complete"); value != "" && value != "true" {
		return fail(errConversationHistoryPartial)
	}
	var source *conversationSnapshot
	if assertedComplete {
		// Imports are explicitly independent. Matching common visible text must
		// not import another client's hidden instructions or tool definitions.
	} else if previousID != "" {
		source, err = store.lookupResponse(routeID, previousID)
		if err != nil {
			return fail(err)
		}
	} else {
		source, err = store.lookupIndexes(store.anchorIndexes(routeID, input.anchors))
		if err != nil {
			return fail(err)
		}
		prefix, err := store.lookupIndexes(store.prefixIndexes(routeID, store.clientScope(routeID, headers), input.items))
		if err != nil {
			return fail(err)
		}
		// A client may replay streamed item IDs absent from the terminal
		// response. An older anchor must not hide a newer complete snapshot,
		// but a matching prefix cannot override a different anchored lineage.
		if prefix != nil && (source == nil || (prefix.Root == source.Root &&
			len(prefix.Input) > len(source.Input) && conversationHasPrefix(prefix.Input, source.Input))) {
			source = prefix
		}
	}
	fullInput := input.items
	if source != nil {
		if previousID != "" {
			if !conversationDeltaInput(input.items) || input.private {
				return fail(errConversationHistoryPartial)
			}
			fullInput = append(cloneRawMessages(source.Input), input.items...)
		} else {
			if !conversationHasPrefix(input.items, source.Input) || !conversationDeltaInput(input.items[len(source.Input):]) {
				return fail(errConversationHistoryPartial)
			}
		}
	} else if !assertedComplete && (input.private || previousID != "" || headerGetCI(headers, "X-Codex-Turn-State") != "" || !conversationNewInput(input.items)) {
		return fail(errConversationHistoryMissing)
	}
	if len(fullInput) > maxConversationHistoryItems {
		return fail(errConversationHistoryCapacity)
	}
	if err := validateConversationToolSequence(fullInput, false); err != nil {
		return fail(err)
	}
	turn := &conversationTurn{
		h: h, store: store, operation: operation, source: source,
		root: uuid.NewString(), input: cloneRawMessages(fullInput), fields: copyResponsesRequestFields(fields),
		scope:        store.clientScope(routeID, headers),
		instructions: cloneRawMessage(fields["instructions"]), tools: cloneRawMessage(fields["tools"]),
		additionalTools: cloneRawMessages(input.additionalTools),
	}
	if source != nil {
		turn.root, turn.migrated = source.Root, source.Migrated
		if turn.scope == "" {
			turn.scope = source.Scope
		}
		if _, present := fields["instructions"]; !present {
			turn.instructions = cloneRawMessage(source.Instructions)
		}
		if _, present := fields["tools"]; !present {
			turn.tools = cloneRawMessage(source.Tools)
		}
		if len(input.additionalTools) == 0 {
			turn.additionalTools = cloneRawMessages(source.AdditionalTools)
		}
		if _, exists := operation.route.targetByID(source.TargetID); !exists {
			return fail(errDurableStateIdentity)
		}
		if err := operation.forcePinnedTarget(source.TargetID); err != nil {
			return fail(errDurableStateIdentity)
		}
		operation.mu.Lock()
		operation.stateOwnerIdentity = source.Identity
		operation.mu.Unlock()
		// A readable snapshot does not override missing or conflicting ownership
		// proof. Check the original response before a store:false reconstruction
		// removes its ID from the upstream request.
		if err := h.applyDurableRequestStateBinding(h.stateBindings, operation, []stateBindingToken{{stateBindingTypeResponseID, source.ResponseID}}); err != nil {
			h.logConversationRecovery(operation, "blocked", source.TargetID, "owner_unverified")
			return nil, nil, err
		}
	}
	if len(turn.instructions) > 0 {
		turn.fields["instructions"] = turn.instructions
	}
	if len(turn.tools) > 0 {
		turn.fields["tools"] = turn.tools
	}
	if len(input.additionalTools) == 0 && len(turn.additionalTools) > 0 {
		// Inherit omitted catalogs on ordinary same-owner continuations too,
		// without changing any raw reasoning items supplied by the client.
		var rawInput []json.RawMessage
		if json.Unmarshal(fields["input"], &rawInput) != nil {
			rawInput = input.items
		}
		turn.fields["input"], _ = json.Marshal(append(cloneRawMessages(turn.additionalTools), rawInput...))
	}
	turn.hostedTools = conversationHostedTools(turn.tools, turn.additionalTools, turn.input)
	if len(turn.input)+len(turn.additionalTools) > maxConversationHistoryItems ||
		rawMessagesSize(turn.input)+rawMessagesSize(turn.additionalTools)+len(turn.instructions)+len(turn.tools) > store.config.MaxHistoryBytes {
		return fail(errConversationHistoryCapacity)
	}
	if err := store.acquire(turn.root); err != nil {
		return fail(err)
	}
	operation.conversation = turn
	// After a migration, a full-history client may still carry east's older
	// encrypted items. The verified snapshot supplies the missing readable
	// context, and the saved owner selects west without relabeling those items.
	// An explicit complete-history import starts a separate, fresh conversation.
	if turn.migrated && source != nil && previousID == "" {
		body, headers, err = turn.currentOwnerHistoryRequest(headers)
	} else if (source != nil && !source.Stored && previousID != "") || (source == nil && assertedComplete) {
		body, headers, err = turn.reconstructedRequest(headers)
	} else {
		body, err = json.Marshal(turn.fields)
		headers = headers.Clone()
		deleteConversationHeader(headers, "X-Vekil-History-Complete")
		if turn.migrated {
			// Keep the new provider's response ID. An old connection header is
			// redundant once the saved response unambiguously selects its owner.
			deleteConversationHeader(headers, "X-Codex-Turn-State")
		}
	}
	if err != nil {
		turn.finish()
		return fail(err)
	}
	return body, headers, nil
}

func deleteConversationHeader(headers http.Header, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}

func (s *conversationHistoryStore) clientScope(routeID string, headers http.Header) string {
	for _, name := range []string{"session_id", "session-id", "thread-id"} {
		if value := headerGetCI(headers, name); value != "" {
			digest := s.d.digest("conversation-client-v1", routeID, name, value)
			return hex.EncodeToString(digest[:])
		}
	}
	return ""
}

func (t *conversationTurn) reconstructedRequest(headers http.Header) ([]byte, http.Header, error) {
	return t.historyRequest(t.input, headers)
}

// Full-history clients keep both regions' encrypted items after a switch. Keep
// only reasoning proven to belong to the saved owner. Visible items are already
// verified against the immutable snapshot and lose their old provider IDs.
func (t *conversationTurn) currentOwnerHistoryRequest(headers http.Header) ([]byte, http.Header, error) {
	var rawItems []json.RawMessage
	if json.Unmarshal(t.fields["input"], &rawItems) != nil {
		return nil, nil, errConversationHistoryPartial
	}
	owner := t.store.d.encodeOwner(stateBindingOwner{routeID: t.source.RouteID, targetID: t.source.TargetID, identity: t.source.Identity})
	items := make([]json.RawMessage, 0, len(rawItems))
	visible := 0
	for _, raw := range rawItems {
		var item struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return nil, nil, errConversationHistoryPartial
		}
		if item.Type == responsesAdditionalToolsType {
			continue
		}
		if item.Type == "reasoning" {
			if item.Encrypted != "" {
				binding := t.h.stateBindings.lookup(stateBindingTypeEncryptedContent, item.Encrypted)
				if binding.err != nil {
					return nil, nil, binding.err
				}
				if binding.outcome == stateBindingLookupKnown && binding.owner == owner {
					items = append(items, raw)
				}
			}
			continue
		}
		if visible >= len(t.input) {
			return nil, nil, errConversationHistoryPartial
		}
		items = append(items, t.input[visible])
		visible++
	}
	if visible != len(t.input) {
		return nil, nil, errConversationHistoryPartial
	}
	return t.historyRequest(items, headers)
}

func (t *conversationTurn) historyRequest(input []json.RawMessage, headers http.Header) ([]byte, http.Header, error) {
	fields := copyResponsesRequestFields(t.fields)
	delete(fields, "previous_response_id")
	delete(fields, "conversation")
	fields["input"], _ = json.Marshal(append(cloneRawMessages(t.additionalTools), input...))
	if len(t.instructions) > 0 {
		fields["instructions"] = t.instructions
	}
	if len(t.tools) > 0 {
		fields["tools"] = t.tools
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, errConversationHistoryPartial
	}
	if len(body) > maxLargeRequestBodySize {
		return nil, nil, errConversationHistoryCapacity
	}
	headers = headers.Clone()
	deleteConversationHeader(headers, "X-Codex-Turn-State")
	deleteConversationHeader(headers, "X-Vekil-History-Complete")
	return body, headers, nil
}

func (t *conversationTurn) prepareTargetBody(body []byte, target targetBinding) ([]byte, error) {
	if t == nil || target.provider == nil || target.provider.kind != providerTypeCopilot ||
		!bytes.Equal(bytes.TrimSpace(t.fields["store"]), []byte("true")) {
		return body, nil
	}
	// Copilot rejects store:true. Protected turns already save their complete
	// history locally, so response-ID continuations can reconstruct it instead.
	rewritten, ok := replaceSingleTopLevelRawJSONField(body, "store", json.RawMessage("false"))
	if !ok {
		return nil, conversationRequestError(errConversationHistoryPartial)
	}
	return rewritten, nil
}

func (t *conversationTurn) persistIntent() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return context.Canceled
	}
	if t.pending {
		return nil
	}
	if err := t.store.beginAttempt(t.root, t.operation.operationID()); err != nil {
		return conversationRequestError(err)
	}
	t.pending = true
	return nil
}

func (t *conversationTurn) dispatching() {
	if t != nil {
		t.mu.Lock()
		t.dispatched = true
		t.mu.Unlock()
	}
}

func (t *conversationTurn) finish() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.pending && !t.saved {
		safe := !t.dispatched
		if t.dispatched {
			_, _, traces := t.operation.snapshot()
			safe = len(traces) > 0
			for _, trace := range traces {
				safe = safe && trace.CleanupDone && trace.Commitment == downstreamCommitmentNone &&
					upstreamProgressAllowsTargetSwitch(trace.Progress) &&
					(trace.Delivery == requestDefinitelyNotDelivered || trace.Delivery == requestExplicitlyRejected)
			}
		}
		if safe {
			if err := t.store.clearAttempt(t.root); err != nil {
				t.h.logConversationRecovery(t.operation, "blocked", "", "storage_unavailable")
			}
		} else if !t.blocked {
			t.h.logConversationRecovery(t.operation, "blocked", "", "execution_uncertain")
		}
	}
	t.store.release(t.root)
}

// An unsupported completion already executed upstream. Clear the attempt
// marker because the outcome is known, then expose the completion unchanged
// without a snapshot. A marker that cannot be cleared withholds the completion
// like a failed snapshot save does. The caller holds t.mu.
func (t *conversationTurn) unprotect(targetID, reason string) error {
	if t.pending {
		if err := t.store.clearAttempt(t.root); err != nil {
			t.blocked = true
			t.h.logConversationRecovery(t.operation, "blocked", targetID, conversationFailureReason(err))
			return conversationRequestError(err)
		}
		t.pending = false
	}
	t.unprotected, t.blocked = true, true
	t.h.logConversationRecovery(t.operation, "unprotected", targetID, reason)
	return nil
}

func (t *conversationTurn) recoveryHeader() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.unprotected {
		return "unprotected"
	}
	return "saved"
}

func conversationUnsupportedState(err error) bool {
	return errors.Is(err, errConversationHostedState) || errors.Is(err, errConversationCompaction)
}

func (h *ProxyHandler) logConversationRecovery(operation *routeOperation, outcome, targetID, reason string) {
	if h == nil || h.log == nil || operation == nil || operation.route == nil {
		return
	}
	h.log.Info("conversation recovery",
		logger.F("route", operation.route.public.routeID),
		logger.F("outcome", outcome), logger.F("target", targetID), logger.F("reason", reason))
}

func (h *ProxyHandler) conversationMigrationTarget(ctx context.Context, operation *routeOperation, endpoint string, kind routeAttemptKind) (targetBinding, bool) {
	if operation == nil || operation.conversation == nil || endpoint != providerEndpointResponses || kind != routeAttemptNormal ||
		!operation.retryAdmissionOpen(ctx, h.ShuttingDown()) {
		return targetBinding{}, false
	}
	t := operation.conversation
	t.mu.Lock()
	attempted, closed := t.attempted, t.closed
	t.mu.Unlock()
	if closed || operation.pinnedTarget() == "" {
		return targetBinding{}, false
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()
	// The first reconstruction requires verified original ownership. Later
	// candidates use the same complete history after a proven safe failure.
	if (!attempted && operation.stateOwnerIdentity == [32]byte{}) || operation.route.policy.mode != routeModePriorityFailover {
		return targetBinding{}, false
	}
	for _, target := range operation.route.targets {
		_, attempted := operation.attemptedTargets[target.id]
		// A target that has not declared the conversation's hosted tools cannot
		// replay their call items, so it is skipped rather than tried.
		if target.id != operation.pinnedTargetID && !attempted && target.provider != nil &&
			conversationMigrationProviderSupported(target.provider.kind) && target.provider.supportsEndpoint(endpoint) &&
			target.provider.supportsHostedTools(t.hostedTools) {
			return target, true
		}
	}
	return targetBinding{}, false
}

func safeConversationMigrationFailure(failure routeAttemptFailure) bool {
	if !failure.cleanupDone || failure.commitment != downstreamCommitmentNone || !upstreamProgressAllowsTargetSwitch(failure.progress) {
		return false
	}
	return (failure.delivery == requestDefinitelyNotDelivered && failure.outcome == routeAttemptOutcomeTransportError) ||
		((failure.delivery == requestExplicitlyRejected || failure.delivery == requestDefinitelyNotDelivered) && failure.outcome == routeAttemptOutcomeRejected)
}

func (h *ProxyHandler) tryConversationMigration(ctx context.Context, operation *routeOperation, endpoint, dispatchPath string, headers http.Header, requestedModel string, stream bool, failure routeAttemptFailure) (*http.Response, bool, error) {
	if operation == nil || operation.conversation == nil || endpoint != providerEndpointResponses {
		return nil, false, nil
	}
	t := operation.conversation
	if _, _, storageFailure := durableStateFailureDetails(failure.err); storageFailure {
		return nil, false, nil
	}
	if !safeConversationMigrationFailure(failure) {
		if t.dispatched && failure.delivery == requestDeliveredOrAmbiguous {
			t.mu.Lock()
			t.blocked = true
			t.mu.Unlock()
			h.logConversationRecovery(operation, "blocked", "", "execution_uncertain")
			return nil, true, conversationRequestError(errConversationHistoryUncertain)
		}
		return nil, false, nil
	}
	target, ok := h.conversationMigrationTarget(ctx, operation, endpoint, routeAttemptKindFromContext(ctx))
	if !ok {
		return nil, false, nil
	}
	body, headers, err := t.reconstructedRequest(headers)
	if err != nil {
		return nil, true, conversationRequestError(err)
	}
	body = h.rewriteResponsesRequestBodyWithToolOptimizersForModel(ctx, body, requestedModel, "responses/migration", true, t.toolContexts, t.toolScope)
	t.mu.Lock()
	t.attempted, t.migrated = true, true
	t.mu.Unlock()
	operation.mu.Lock()
	// Reconstruction is the sole exception to the original owner pin. Saved
	// ownership never changes, and attemptedTargets prevents revisiting a target.
	operation.pinnedTargetID, operation.hardPinned = target.id, true
	operation.stateOwnerIdentity = [32]byte{}
	operation.bootstrapConversation = nil
	operation.mu.Unlock()
	h.logConversationRecovery(operation, "attempted", target.id, "")
	resp, err := h.executeExplicitRouteRequestPath(ctx, operation.route, endpoint, dispatchPath, body, headers, requestedModel, stream)
	if err != nil || (resp != nil && resp.StatusCode >= http.StatusBadRequest) {
		h.logConversationRecovery(operation, "blocked", target.id, "backup_failed")
	}
	return resp, true, err
}

func (t *conversationTurn) saveResponse(data []byte, info explicitRouteResponseInfo) ([]byte, error) {
	if t == nil {
		return data, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, context.Canceled
	}
	if t.unprotected {
		return data, nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if t.saved {
			return data, nil
		}
		return nil, conversationRequestError(errConversationIncomplete)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return nil, conversationRequestError(errConversationIncomplete)
	}
	response := envelope
	eventType := rawJSONString(envelope["type"])
	if eventType != "" {
		if eventType != "response.completed" {
			if eventType == "response.incomplete" || eventType == "response.failed" || eventType == "response.cancelled" || eventType == "response.canceled" || eventType == "error" {
				return nil, conversationRequestError(errConversationIncomplete)
			}
			return data, nil
		}
		response = nil
		if json.Unmarshal(envelope["response"], &response) != nil || response == nil {
			return nil, conversationRequestError(errConversationIncomplete)
		}
	}
	if t.saved {
		return nil, conversationRequestError(errConversationIncomplete)
	}
	if rawJSONString(response["status"]) != "completed" || rawJSONString(response["id"]) == "" || response["output"] == nil {
		return nil, conversationRequestError(errConversationIncomplete)
	}
	output, err := canonicalConversationInput(response["output"], true)
	if err != nil {
		if conversationUnsupportedState(err) {
			if err := t.unprotect(info.targetID, conversationFailureReason(err)); err != nil {
				return nil, err
			}
			return data, nil
		}
		return nil, conversationRequestError(err)
	}
	if len(t.input)+len(t.additionalTools)+len(output.items) > maxConversationHistoryItems {
		return nil, conversationRequestError(errConversationHistoryCapacity)
	}
	snapshot := &conversationSnapshot{
		ResponseID: rawJSONString(response["id"]), RouteID: info.routeID, TargetID: info.targetID, Identity: info.stateIdentity,
		Root: t.root, Scope: t.scope, Created: t.store.d.now().Unix(),
		Input: append(cloneRawMessages(t.input), output.items...), Instructions: t.instructions, Tools: t.tools, Migrated: t.migrated,
		AdditionalTools: cloneRawMessages(t.additionalTools),
		Stored:          !bytes.Equal(bytes.TrimSpace(t.fields["store"]), []byte("false")),
	}
	if target, ok := t.operation.route.targetByID(info.targetID); ok && target.provider != nil && target.provider.kind == providerTypeCopilot {
		// Copilot continuations use our durable snapshots, including when
		// store was omitted. Keep its IDs as local history anchors.
		snapshot.Stored = false
	}
	if err := validateConversationToolSequence(snapshot.Input, true); err != nil {
		return nil, conversationRequestError(err)
	}
	snapshot.Indexes = t.store.anchorIndexes(info.routeID, output.anchors)
	if prefixes := t.store.prefixIndexes(info.routeID, snapshot.Scope, snapshot.Input); len(prefixes) > 0 {
		snapshot.Indexes = append(snapshot.Indexes, prefixes[len(prefixes)-1])
	}
	if !t.saved {
		if err := t.store.save(snapshot); err != nil {
			t.blocked = true
			t.h.logConversationRecovery(t.operation, "blocked", info.targetID, conversationFailureReason(err))
			return nil, conversationRequestError(err)
		}
		t.saved = true
		outcome := "saved"
		if t.attempted {
			outcome = "completed"
		}
		t.h.logConversationRecovery(t.operation, outcome, info.targetID, "")
	}
	diagnostic := map[string]any{"history": "saved", "target": info.targetID}
	if t.attempted {
		diagnostic["migration"] = "completed"
	}
	response["vekil"], _ = json.Marshal(diagnostic)
	if eventType != "" {
		envelope["response"], _ = json.Marshal(response)
	} else {
		envelope = response
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

type conversationCompletionBody struct {
	io.ReadCloser
	turn *conversationTurn
}

func (b *conversationCompletionBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, context.Canceled) {
		b.turn.mu.Lock()
		saved := b.turn.saved || b.turn.unprotected
		b.turn.mu.Unlock()
		_, _, storageFailure := durableStateFailureDetails(err)
		if !saved && !storageFailure && providerRequestErrorCode(err) == "" {
			return n, conversationRequestError(errConversationIncomplete)
		}
	}
	return n, err
}
