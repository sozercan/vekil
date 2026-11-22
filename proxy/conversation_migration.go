package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/logger"
)

type conversationTurn struct {
	mu                  sync.Mutex
	h                   *ProxyHandler
	store               *conversationHistoryStore
	operation           *routeOperation
	source              *conversationSnapshot
	sourceKey           []byte
	trustedClient       bool
	root                string
	scope               string
	input               []json.RawMessage
	fields              map[string]json.RawMessage
	instructions        json.RawMessage
	tools               json.RawMessage
	additionalTools     []json.RawMessage
	toolContexts        *ToolExecutionContextStore
	toolScope           string
	hostedTools         map[string]bool
	pending             bool
	dispatched          bool
	executable          bool
	openItems           map[int]string
	unstaged            bool
	failed              bool
	streamUncertain     bool
	handoff             bool
	handoffOffset       int64
	handoffReason       string
	handoffTargetID     string
	bridged             bool
	streamWritten       int64
	streamRead          int64
	deliveryEvents      int
	staged              []conversationStagedItem
	stagedIndexes       map[int]bool
	stagedBytes         int
	deliveredResponseID string
	deliveredInvalid    bool
	deliveryInfo        explicitRouteResponseInfo
	haveDeliveryInfo    bool
	saved               bool
	closed              bool
	migrated            bool
	attempted           bool
	blocked             bool
	unprotected         bool
}

var minResendingCodexVersion = [3]uint64{0, 157, 0}

var resendingCodexProducts = map[string]bool{"codex_cli_rs": true, "codex-tui": true, "codex_exec": true, "codex_vscode": true}

func conversationClientResendsDelivered(userAgent string) bool {
	product, _, _ := strings.Cut(strings.TrimSpace(userAgent), " ")
	name, version, ok := strings.Cut(product, "/")
	if !ok || !resendingCodexProducts[name] {
		return false
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return false
		}
		if n != minResendingCodexVersion[i] {
			return n > minResendingCodexVersion[i]
		}
	}
	return true
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
	if _, err := extractResponsesRequestState(body, headers, true); err != nil {
		return nil, nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return fail(errConversationHistoryPartial)
	}
	unprotected := func(err error) ([]byte, http.Header, error) {
		if !conversationUnsupportedState(err) {
			return fail(err)
		}
		source, owned, ownedHeaders, ownerErr := h.resolveMixedOwnerRequest(operation, fields, body, headers)
		if ownerErr != nil {
			return nil, nil, ownerErr
		}
		targetID := ""
		if source != nil {
			targetID = source.TargetID
		}
		h.logConversationRecovery(operation, "unprotected", targetID, conversationFailureReason(err))
		return owned, ownedHeaders, nil
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
	} else if previousID != "" {
		source, err = store.lookupResponse(routeID, previousID)
		if errors.Is(err, errConversationHistoryMissing) && store.turnActive("", store.responseKey(routeID, previousID)) {
			return fail(errConversationHistoryBusy)
		}
		if err != nil {
			return fail(err)
		}
	} else {
		source, err = store.lookupFullHistory(routeID, headers, input)
		if err != nil {
			return fail(err)
		}
	}
	fullInput := input.items
	trusted := conversationClientResendsDelivered(operation.clientUserAgent)
	resent := false
	partial := func() ([]byte, http.Header, error) {
		if store.turnActive(source.Root, nil) {
			return fail(errConversationHistoryBusy)
		}
		return fail(errConversationHistoryPartial)
	}
	if source != nil {
		if previousID != "" {
			if !conversationDeltaInput(input.items) || input.private {
				resent = true
			}
			fullInput = append(cloneRawMessages(source.Input), input.items...)
		} else {
			if !conversationHasPrefix(input.items, source.Input) {
				return partial()
			}
			resent = !conversationDeltaInput(input.items[len(source.Input):])
		}
		if resent && !trusted {
			return partial()
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
		turn.sourceKey = store.responseKey(source.RouteID, source.ResponseID)
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
		if err := h.pinConversationOwner(operation, source); err != nil {
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
	turn.trustedClient = trusted
	root, overrode, err := store.acquireFrom(turn.root, headerGetCI(headers, "thread-id"), turn.sourceKey, turn.trustedClient)
	if err != nil {
		return fail(err)
	}
	if root != turn.root {
		turn.root = root
		h.logConversationRecovery(operation, "branched", "", "concurrent_turn")
	}
	if overrode {
		h.logConversationRecovery(operation, "released", "", "client_resends_delivered")
	}
	operation.conversation = turn
	if turn.migrated && source != nil && previousID == "" {
		body, headers, err = turn.currentOwnerHistoryRequest(headers)
	} else if (source != nil && !source.Stored && previousID != "") || (source == nil && assertedComplete) {
		body, headers, err = turn.reconstructedRequest(headers)
	} else {
		body, err = json.Marshal(turn.fields)
		headers = headers.Clone()
		deleteConversationHeader(headers, "X-Vekil-History-Complete")
		if turn.migrated {
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

func (t *conversationTurn) currentOwnerHistoryRequest(headers http.Header) ([]byte, http.Header, error) {
	var rawItems []json.RawMessage
	if json.Unmarshal(t.fields["input"], &rawItems) != nil {
		return nil, nil, errConversationHistoryPartial
	}
	owner := t.store.snapshotOwner(t.source)
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
			owned, err := t.h.ownsConversationReasoning(owner, item.Encrypted)
			if err != nil {
				return nil, nil, err
			}
			if owned {
				items = append(items, raw)
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

func (h *ProxyHandler) pinConversationOwner(operation *routeOperation, source *conversationSnapshot) error {
	if _, exists := operation.route.targetByID(source.TargetID); !exists || operation.forcePinnedTarget(source.TargetID) != nil {
		h.logConversationRecovery(operation, "blocked", "", conversationFailureReason(errDurableStateIdentity))
		return conversationRequestError(errDurableStateIdentity)
	}
	operation.mu.Lock()
	operation.stateOwnerIdentity = source.Identity
	operation.mu.Unlock()
	if err := h.applyDurableRequestStateBinding(h.stateBindings, operation, []stateBindingToken{{stateBindingTypeResponseID, source.ResponseID}}); err != nil {
		h.logConversationRecovery(operation, "blocked", source.TargetID, "owner_unverified")
		return err
	}
	return nil
}

func (s *conversationHistoryStore) snapshotOwner(source *conversationSnapshot) stateBindingOwner {
	return s.d.encodeOwner(stateBindingOwner{routeID: source.RouteID, targetID: source.TargetID, identity: source.Identity})
}

func (h *ProxyHandler) ownsConversationReasoning(owner stateBindingOwner, encrypted string) (bool, error) {
	if encrypted == "" {
		return false, nil
	}
	binding := h.stateBindings.lookup(stateBindingTypeEncryptedContent, encrypted)
	if binding.err != nil {
		return false, binding.err
	}
	return binding.outcome == stateBindingLookupKnown && binding.owner == owner, nil
}

func (h *ProxyHandler) resolveMixedOwnerRequest(operation *routeOperation, fields map[string]json.RawMessage, body []byte, headers http.Header) (*conversationSnapshot, []byte, http.Header, error) {
	source := h.mixedOwnerConversationSource(operation, fields, body, headers)
	if source == nil {
		return nil, body, headers, nil
	}
	if err := h.pinConversationOwner(operation, source); err != nil {
		return nil, nil, nil, err
	}
	owned, ownedHeaders, err := h.ownerReasoningRequest(fields, headers, source)
	if err != nil {
		h.logConversationRecovery(operation, "blocked", source.TargetID, conversationFailureReason(err))
		return nil, nil, nil, conversationRequestError(err)
	}
	return source, owned, ownedHeaders, nil
}

func (h *ProxyHandler) prepareCompactOwnerRequest(operation *routeOperation, body []byte, headers http.Header) ([]byte, http.Header, bool, error) {
	if operation == nil || !h.conversationMigrationEnabled(operation.route) {
		return body, headers, false, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return body, headers, false, nil
	}
	source, owned, ownedHeaders, err := h.resolveMixedOwnerRequest(operation, fields, body, headers)
	if err != nil || source == nil {
		return owned, ownedHeaders, false, err
	}
	h.logConversationRecovery(operation, "unprotected", source.TargetID, conversationFailureReason(errConversationCompaction))
	return owned, ownedHeaders, true, nil
}

func (h *ProxyHandler) mixedOwnerConversationSource(operation *routeOperation, fields map[string]json.RawMessage, body []byte, headers http.Header) *conversationSnapshot {
	if rawJSONHasNonEmptyValue(fields["previous_response_id"]) || rawJSONHasNonEmptyValue(fields["conversation"]) ||
		headerGetCI(headers, "X-Vekil-History-Complete") != "" {
		return nil
	}
	routeID := operation.route.public.routeID
	tokens, err := extractResponsesRequestState(body, headers, true)
	if err != nil || len(tokens) == 0 {
		return nil
	}
	if result := h.stateBindings.resolveForRoute(routeID, operation.pinnedTarget(), tokens); result.err != nil || result.outcome != stateBindingLookupConflict {
		return nil
	}
	readable := readableConversationHistory(fields["input"])
	source, err := h.conversationHistory.lookupFullHistory(routeID, headers, readable)
	if err != nil || source == nil || !source.Migrated || !conversationHasPrefix(readable.items, source.Input) {
		return nil
	}
	return source
}

func (s *conversationHistoryStore) lookupFullHistory(routeID string, headers http.Header, input conversationInput) (*conversationSnapshot, error) {
	source, err := s.lookupIndexes(s.anchorIndexes(routeID, input.anchors))
	if err != nil {
		return nil, err
	}
	prefix, err := s.lookupIndexes(s.prefixIndexes(routeID, s.clientScope(routeID, headers), input.items))
	if err != nil {
		return nil, err
	}
	if prefix != nil && (source == nil || (prefix.Root == source.Root &&
		len(prefix.Input) > len(source.Input) && conversationHasPrefix(prefix.Input, source.Input))) {
		source = prefix
	}
	return source, nil
}

func (h *ProxyHandler) ownerReasoningRequest(fields map[string]json.RawMessage, headers http.Header, source *conversationSnapshot) ([]byte, http.Header, error) {
	var rawItems []json.RawMessage
	if json.Unmarshal(fields["input"], &rawItems) != nil {
		return nil, nil, errConversationHistoryPartial
	}
	owner := h.conversationHistory.snapshotOwner(source)
	items := make([]json.RawMessage, 0, len(rawItems))
	for _, raw := range rawItems {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil && rawJSONString(item["type"]) == "reasoning" {
			owned, err := h.ownsConversationReasoning(owner, rawJSONString(item["encrypted_content"]))
			if err != nil {
				return nil, nil, err
			}
			if owned {
				items = append(items, raw)
			}
			continue
		}
		if canonical, err := canonicalConversationItem(raw); err == nil && len(canonical.items) == 1 {
			raw = canonical.items[0]
		}
		items = append(items, raw)
	}
	body, err := json.Marshal(copyResponsesRequestFieldsWithInput(fields, items))
	if err != nil {
		return nil, nil, errConversationHistoryPartial
	}
	if len(body) > maxLargeRequestBodySize {
		return nil, nil, errConversationHistoryCapacity
	}
	headers = headers.Clone()
	deleteConversationHeader(headers, "X-Codex-Turn-State")
	return body, headers, nil
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
	if err := t.store.beginAttemptFrom(t.root, t.operation.operationID(), t.sourceKey, t.trustedClient); err != nil {
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
	if t.pending && !t.saved && t.dispatched && !t.blocked && !t.streamUncertain && t.clientEnded() {
		t.releaseInterruptedAttempt()
	} else if t.pending && !t.saved {
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
		} else if !t.blocked && !t.streamUncertain {
			if err := t.releaseEndedAttempt(t.deliveryInfo.targetID, "proxy_ended", false); err != nil && !t.blocked {
				t.h.logConversationRecovery(t.operation, "blocked", "", "execution_uncertain")
			}
		} else if !t.blocked {
			t.h.logConversationRecovery(t.operation, "blocked", "", "execution_uncertain")
		}
	}
	t.store.release(t.root)
}

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

func (t *conversationTurn) releaseFailedAttempt(envelope map[string]json.RawMessage, targetID, reason string) error {
	if conversationEventHasExecutableOutput(envelope) {
		t.executable, t.unstaged = true, true
		return conversationRequestError(errConversationIncomplete)
	}
	if t.saved {
		return conversationRequestError(errConversationIncomplete)
	}
	if t.executable {
		return t.deferEnd(targetID, reason, t.streamWritten)
	}
	return t.releaseEndedAttempt(targetID, reason, true)
}

func (t *conversationTurn) streamWrote(n int) {
	t.mu.Lock()
	t.streamWritten += int64(n)
	t.mu.Unlock()
}

func (t *conversationTurn) deferEnd(targetID, reason string, offset int64) error {
	if t.handoff {
		return nil
	}
	if t.executable {
		if _, err := t.endedHistory(true); err != nil {
			return err
		}
	}
	t.handoff, t.handoffOffset, t.handoffReason, t.handoffTargetID = true, offset, reason, targetID
	return nil
}

func (t *conversationTurn) streamReadStarting() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streamRead <= t.handoffOffset {
		return nil
	}
	return t.settleHandoff()
}

func (t *conversationTurn) streamDelivered() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.settleHandoff()
}

func (t *conversationTurn) settleHandoff() error {
	if !t.handoff {
		return nil
	}
	t.handoff = false
	return t.releaseEndedAttempt(t.handoffTargetID, t.handoffReason, true)
}

func (t *conversationTurn) streamReadDone(n int) {
	t.mu.Lock()
	t.streamRead += int64(n)
	t.mu.Unlock()
}

func (t *conversationTurn) releaseEndedAttempt(targetID, reason string, ended bool) error {
	if t.executable {
		if err := t.saveEndedHistory(targetID, ended); err != nil {
			return err
		}
		reason = "delivered_history_saved"
	} else if err := t.saveEndedInertHistory(ended); err != nil {
		t.blocked = true
		t.h.logConversationRecovery(t.operation, "blocked", targetID, conversationFailureReason(err))
		return conversationRequestError(err)
	}
	if t.pending {
		if err := t.store.clearAttempt(t.root); err != nil {
			t.blocked = true
			t.h.logConversationRecovery(t.operation, "blocked", targetID, conversationFailureReason(err))
			return conversationRequestError(err)
		}
		t.pending = false
	}
	if !t.failed {
		t.failed = true
		t.h.logConversationRecovery(t.operation, "failed", targetID, reason)
	}
	return nil
}

func conversationEventHasExecutableOutput(envelope map[string]json.RawMessage) bool {
	raw, ok := envelope["response"]
	if !ok || rawJSONIsNullOrEmpty(raw) {
		return false
	}
	var response struct {
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return true
	}
	if !responsesOutputHasProgress(response.Output) {
		return false
	}
	var items []json.RawMessage
	if json.Unmarshal(response.Output, &items) != nil {
		return true
	}
	for _, item := range items {
		if !conversationItemInert(item) {
			return true
		}
	}
	return false
}

func conversationItemInert(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	return item.Type == "reasoning" || item.Type == "message"
}

func conversationEventInert(eventType string, envelope map[string]json.RawMessage) bool {
	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		return conversationItemInert(envelope["item"])
	case "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added",
		"response.refusal.delta", "response.refusal.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		return true
	}
	return false
}

func (t *conversationTurn) trackExecutableItem(eventType string, envelope map[string]json.RawMessage) {
	switch eventType {
	case "response.output_item.added", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		return
	}
	t.executable = true
	var index *int
	complete := eventType == "response.output_item.done" || eventType == "response.function_call_arguments.done" ||
		eventType == "response.custom_tool_call_input.done"
	if !complete || json.Unmarshal(envelope["output_index"], &index) != nil || index == nil {
		t.unstaged = true
		return
	}
	if eventType == "response.output_item.done" {
		if id, open := t.openItems[*index]; open {
			var item struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(envelope["item"], &item) != nil || item.ID != id {
				t.unstaged = true
				return
			}
			delete(t.openItems, *index)
		}
		return
	}
	id := rawJSONString(envelope["item_id"])
	if open, ok := t.openItems[*index]; id == "" || ok && open != id {
		t.unstaged = true
		return
	}
	if t.openItems == nil {
		t.openItems = make(map[int]string)
	}
	t.openItems[*index] = id
}

func (t *conversationTurn) saveEndedHistory(targetID string, ended bool) error {
	delivered, err := t.endedHistory(ended)
	if err != nil {
		return err
	}
	snapshot, err := t.historySnapshot(t.deliveredResponseID, t.deliveryInfo, delivered, false)
	if err == nil {
		err = t.store.saveDelivered(snapshot)
	}
	if errors.Is(err, errConversationHistoryStorage) || errors.Is(err, errConversationHistoryCapacity) || errors.Is(err, errConversationHistoryUncertain) {
		t.blocked = true
		t.h.logConversationRecovery(t.operation, "blocked", targetID, conversationFailureReason(err))
		return conversationRequestError(err)
	}
	if err != nil {
		return conversationRequestError(errConversationIncomplete)
	}
	t.pending = false
	return nil
}

func (t *conversationTurn) saveEndedInertHistory(ended bool) error {
	delivered := t.deliveredOutput(ended)
	if len(delivered.items) == 0 && len(delivered.anchors) == 0 || t.deliveredInvalid || !t.haveDeliveryInfo || t.deliveredResponseID == "" {
		return nil
	}
	snapshot, err := t.historySnapshot(t.deliveredResponseID, t.deliveryInfo, delivered, false)
	if err == nil {
		err = t.store.save(snapshot)
	}
	if errors.Is(err, errConversationHistoryUncertain) {
		return err
	}
	if err == nil {
		t.pending = false
	}
	return nil
}

func (t *conversationTurn) endedHistory(ended bool) (conversationInput, error) {
	if !ended {
		for _, staged := range t.staged {
			if staged.event <= t.deliveryEvents-2 {
				continue
			}
			for _, item := range staged.output.items {
				if !conversationItemInert(item) {
					return conversationInput{}, conversationRequestError(errConversationIncomplete)
				}
			}
		}
	}
	delivered := t.deliveredOutput(ended)
	if t.unstaged || len(t.openItems) > 0 || t.deliveredInvalid || !t.haveDeliveryInfo || t.deliveredResponseID == "" ||
		len(delivered.items) == 0 && len(delivered.anchors) == 0 {
		return conversationInput{}, conversationRequestError(errConversationIncomplete)
	}
	return delivered, nil
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
	if (!attempted && operation.stateOwnerIdentity == [32]byte{}) || operation.route.policy.mode != routeModePriorityFailover {
		return targetBinding{}, false
	}
	for _, target := range operation.route.targets {
		_, attempted := operation.attemptedTargets[target.id]
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
		if failure.progress == upstreamProgressTerminalFailure && failure.commitment == downstreamCommitmentNone && failure.cleanupDone {
			t.mu.Lock()
			err := t.releaseFailedAttempt(nil, "", "failure_event")
			t.mu.Unlock()
			if err != nil {
				return nil, true, err
			}
			return nil, false, nil
		}
		if t.clientEnded() && (errors.Is(failure.err, context.Canceled) || errors.Is(failure.err, context.DeadlineExceeded)) {
			return nil, false, nil
		}
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
		if t.saved || t.failed || t.handoff {
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
			switch eventType {
			case "response.incomplete", "response.failed", "response.cancelled", "response.canceled", "error":
				if err := t.releaseFailedAttempt(envelope, info.targetID, "failure_event"); err != nil {
					return nil, err
				}
				return data, nil
			case "response.queued", "response.created", "response.in_progress", "keepalive":
				if conversationEventHasExecutableOutput(envelope) {
					t.executable, t.unstaged = true, true
				}
			default:
				if !conversationEventInert(eventType, envelope) {
					t.trackExecutableItem(eventType, envelope)
				}
			}
			t.observeDelivery(eventType, envelope, info)
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
	stored := !bytes.Equal(bytes.TrimSpace(t.fields["store"]), []byte("false"))
	snapshot, err := t.historySnapshot(rawJSONString(response["id"]), info, output, stored)
	if err != nil {
		return nil, conversationRequestError(err)
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

func (t *conversationTurn) historySnapshot(responseID string, info explicitRouteResponseInfo, output conversationInput, stored bool) (*conversationSnapshot, error) {
	if len(t.input)+len(t.additionalTools)+len(output.items) > maxConversationHistoryItems {
		return nil, errConversationHistoryCapacity
	}
	snapshot := &conversationSnapshot{
		ResponseID: responseID, RouteID: info.routeID, TargetID: info.targetID, Identity: info.stateIdentity,
		Root: t.root, Scope: t.scope, Created: t.store.d.now().Unix(),
		Input: append(cloneRawMessages(t.input), output.items...), Instructions: t.instructions, Tools: t.tools, Migrated: t.migrated,
		AdditionalTools: cloneRawMessages(t.additionalTools),
		Stored:          stored,
	}
	if target, ok := t.operation.route.targetByID(info.targetID); ok && target.provider != nil && target.provider.kind == providerTypeCopilot {
		snapshot.Stored = false
	}
	if err := validateConversationToolSequence(snapshot.Input, true); err != nil {
		return nil, err
	}
	snapshot.Indexes = t.store.anchorIndexes(info.routeID, output.anchors)
	if prefixes := t.store.prefixIndexes(info.routeID, snapshot.Scope, snapshot.Input); len(prefixes) > 0 {
		snapshot.Indexes = append(snapshot.Indexes, prefixes[len(prefixes)-1])
	}
	return snapshot, nil
}

type conversationStagedItem struct {
	event, index int
	output       conversationInput
}

func (t *conversationTurn) observeDelivery(eventType string, envelope map[string]json.RawMessage, info explicitRouteResponseInfo) {
	t.deliveryEvents++
	if !t.haveDeliveryInfo {
		t.deliveryInfo, t.haveDeliveryInfo = info, true
	} else if info.targetID != t.deliveryInfo.targetID || info.routeID != t.deliveryInfo.routeID {
		t.invalidateDelivery()
	}
	switch eventType {
	case "response.queued", "response.created", "response.in_progress":
		var response struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(envelope["response"], &response)
		if t.deliveredResponseID == "" {
			t.deliveredResponseID = response.ID
			if response.ID != "" {
				t.store.markStreaming(t.root, t.store.responseKey(info.routeID, response.ID))
			}
		} else if response.ID != "" && response.ID != t.deliveredResponseID {
			t.invalidateDelivery()
		}
	case "response.output_item.done":
		if t.deliveredInvalid {
			return
		}
		item, ok := envelope["item"]
		if !ok {
			t.invalidateDelivery()
			return
		}
		var index *int
		output, err := canonicalConversationInput(append(append([]byte("["), item...), ']'), true)
		if err != nil || json.Unmarshal(envelope["output_index"], &index) != nil || index == nil || *index < 0 || t.stagedIndexes[*index] {
			t.invalidateDelivery()
			return
		}
		size := rawMessagesSize(output.items)
		for _, anchor := range output.anchors {
			size += len(anchor.value)
		}
		if len(t.staged) >= maxConversationHistoryItems || t.stagedBytes+size > t.store.config.MaxHistoryBytes {
			t.invalidateDelivery()
			return
		}
		if t.stagedIndexes == nil {
			t.stagedIndexes = make(map[int]bool)
		}
		t.stagedIndexes[*index] = true
		t.stagedBytes += size
		t.staged = append(t.staged, conversationStagedItem{event: t.deliveryEvents, index: *index, output: output})
	}
}

func (t *conversationTurn) invalidateDelivery() {
	t.deliveredInvalid = true
	t.staged, t.stagedIndexes, t.stagedBytes = nil, nil, 0
}

func (t *conversationTurn) deliveredOutput(ended bool) conversationInput {
	var delivered []conversationStagedItem
	for _, staged := range t.staged {
		if ended || staged.event <= t.deliveryEvents-2 {
			delivered = append(delivered, staged)
		}
	}
	sort.Slice(delivered, func(i, j int) bool { return delivered[i].index < delivered[j].index })
	var output conversationInput
	for _, staged := range delivered {
		output.items = append(output.items, staged.output.items...)
		output.anchors = append(output.anchors, staged.output.anchors...)
	}
	return output
}

func (t *conversationTurn) releaseInterruptedAttempt() {
	targetID := t.deliveryInfo.targetID
	delivered := t.deliveredOutput(false)
	if (len(delivered.items) > 0 || len(delivered.anchors) > 0) && !t.deliveredInvalid && t.deliveredResponseID != "" && t.haveDeliveryInfo {
		snapshot, err := t.historySnapshot(t.deliveredResponseID, t.deliveryInfo, delivered, false)
		if err == nil {
			err = t.store.save(snapshot)
		}
		if err == nil {
			t.pending = false
			t.h.logConversationRecovery(t.operation, "interrupted", targetID, "delivered_history_saved")
			return
		}
		if errors.Is(err, errConversationHistoryStorage) || errors.Is(err, errConversationHistoryUncertain) {
			t.h.logConversationRecovery(t.operation, "blocked", targetID, conversationFailureReason(err))
			return
		}
	}
	if err := t.store.clearAttempt(t.root); err != nil {
		t.h.logConversationRecovery(t.operation, "blocked", targetID, "storage_unavailable")
		return
	}
	t.pending = false
	reason := "no_delivered_items"
	if len(delivered.items) > 0 || len(delivered.anchors) > 0 || t.deliveredInvalid {
		reason = "delivered_history_unavailable"
	}
	t.h.logConversationRecovery(t.operation, "interrupted", targetID, reason)
}

func (t *conversationTurn) clientEnded() bool {
	inbound := t.operation.inbound
	return inbound != nil && inbound.Err() != nil && !t.h.ShuttingDown()
}

type upstreamBodyEnd struct {
	mu  sync.Mutex
	err error
}

func (e *upstreamBodyEnd) observe(ctx context.Context, err error) {
	if e == nil || err == nil || ctx == nil || ctx.Err() != nil || errors.Is(err, http.ErrBodyReadAfterClose) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		e.err = err
	}
}

func (e *upstreamBodyEnd) endedBy(err error) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	ended := e.err
	e.mu.Unlock()
	if ended == io.EOF {
		return err == io.EOF
	}
	return ended != nil && errors.Is(err, ended)
}

type conversationCompletionBody struct {
	io.ReadCloser
	turn        *conversationTurn
	targetID    string
	upstreamEnd *upstreamBodyEnd
}

func (b *conversationCompletionBody) Read(p []byte) (int, error) {
	if releaseErr := b.turn.streamReadStarting(); releaseErr != nil {
		return 0, releaseErr
	}
	n, err := b.ReadCloser.Read(p)
	b.turn.streamReadDone(n)
	if err != nil && !errors.Is(err, context.Canceled) {
		b.turn.mu.Lock()
		known := b.turn.saved || b.turn.unprotected || b.turn.failed
		released := false
		var releaseErr error
		if !known && !b.turn.clientEnded() {
			if !b.turn.blocked {
				reason := "proxy_ended"
				if b.upstreamEnd.endedBy(err) {
					reason = "stream_ended"
				}
				if b.turn.bridged {
					releaseErr = b.turn.deferEnd(b.targetID, reason, math.MaxInt64)
				} else {
					releaseErr = b.turn.releaseEndedAttempt(b.targetID, reason, true)
				}
				known, released = releaseErr == nil, releaseErr == nil
			}
			if !known {
				b.turn.streamUncertain = true
			}
		}
		b.turn.mu.Unlock()
		if releaseErr != nil {
			return n, releaseErr
		}
		if released && providerRequestErrorCode(err) != "" {
			return n, errConversationStreamEnded
		}
		_, _, storageFailure := durableStateFailureDetails(err)
		if !known && !storageFailure && providerRequestErrorCode(err) == "" {
			return n, conversationRequestError(errConversationIncomplete)
		}
	}
	return n, err
}
