package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
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
	sourceKey       []byte
	trustedClient   bool
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
	// A handed-off event may have given the client something it can run, such
	// as a completed tool call. Reasoning, assistant messages and items handed
	// off only in part cannot run anything.
	executable bool
	// Item IDs whose complete arguments were handed off without the finished
	// item, by output_index, and whether a handed-off event carried executable
	// content that staging cannot save.
	openItems       map[int]string
	unstaged        bool
	failed          bool
	streamUncertain bool
	// A terminal failure event that followed executable output settles its
	// attempt once the client's consumer reads past failureOffset, the stream
	// offset where the event starts: by then every earlier event was handed off.
	failureHandoff  bool
	failureOffset   int64
	failureTargetID string
	streamWritten   int64
	streamRead      int64
	// Completed output items staged from the committed response. An item is
	// delivered once a later event has been handed downstream: HTTP and
	// websocket consumers read the next event only after writing the previous.
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

// minResendingCodexVersion is the first Codex release verified to record every
// output item it completes, to run a completed tool call even when the stream
// then fails, and to resend both, with the tool's output, in its next request
// (codex-rs core/src/session/turn.rs run_sampling_request and drain_in_flight,
// stream_events_utils.rs handle_output_item_done).
var minResendingCodexVersion = [3]uint64{0, 157, 0}

// resendingCodexProducts are the Codex front ends, named by their originator,
// that share that verified turn loop (codex-rs login default_client.rs).
var resendingCodexProducts = map[string]bool{"codex_cli_rs": true, "codex-tui": true, "codex_exec": true, "codex_vscode": true}

// conversationClientResendsDelivered reports whether a User-Agent names a Codex
// front end at or after minResendingCodexVersion, such as "codex_exec/0.157.1
// (Mac OS 27.0.0; arm64)". Pre-release and development versions do not count.
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
		// ParseUint accepts digits only, without a sign.
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
		// Imports are explicitly independent. Matching common visible text must
		// not import another client's hidden instructions or tool definitions.
	} else if previousID != "" {
		source, err = store.lookupResponse(routeID, previousID)
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
	// A verified Codex client resends every item it completed, with its tool
	// outputs. Its request reached here only by extending saved history with
	// client input, so nothing it received from an unsettled attempt is missing:
	// an unresolved marker cannot hide a tool call the model might repeat.
	turn.trustedClient = conversationClientResendsDelivered(operation.clientUserAgent)
	overrode, err := store.acquireFrom(turn.root, turn.sourceKey, turn.trustedClient)
	if err != nil {
		return fail(err)
	}
	if overrode {
		h.logConversationRecovery(operation, "released", "", "client_resends_delivered")
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

// pinConversationOwner selects a saved conversation's exact owner. A readable
// snapshot does not override missing or conflicting ownership proof, so check
// the original response before a store:false reconstruction removes its ID
// from the upstream request.
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

// ownsConversationReasoning reports whether encrypted reasoning is bound to the
// saved owner. After a switch, any other reasoning belongs to an earlier owner.
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

// resolveMixedOwnerRequest handles a request that history cannot save. After
// a switch, a full-history client still replays the earlier owner's encrypted
// reasoning, which ownership validation rejects as mixed state. The request
// then runs only on the saved owner with only its reasoning; source is nil and
// the request is unchanged when nothing conflicts.
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

// prepareCompactOwnerRequest resolves mixed replayed state for
// /responses/compact on a migration-enabled route. Compaction output is never
// saved as history, so it follows the unprotected-turn rules. It reports
// whether it rewrote the request.
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

// mixedOwnerConversationSource returns the saved owner of a switched
// conversation whose unprotected full-history request would otherwise fail
// ownership validation with mixed state. Every other request, including one
// whose lookup fails, keeps ordinary routing and validation. Response-ID,
// provider-conversation and import requests carry no earlier owner's items.
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
	// The saved lineage must be a prefix of the readable history, as it must
	// for a protected continuation.
	readable := readableConversationHistory(fields["input"])
	source, err := h.conversationHistory.lookupFullHistory(routeID, headers, readable)
	if err != nil || source == nil || !source.Migrated || !conversationHasPrefix(readable.items, source.Input) {
		return nil
	}
	return source
}

// lookupFullHistory finds the saved snapshot that full input continues, by the
// provider anchors it replays or by its visible prefix within the client scope.
func (s *conversationHistoryStore) lookupFullHistory(routeID string, headers http.Header, input conversationInput) (*conversationSnapshot, error) {
	source, err := s.lookupIndexes(s.anchorIndexes(routeID, input.anchors))
	if err != nil {
		return nil, err
	}
	prefix, err := s.lookupIndexes(s.prefixIndexes(routeID, s.clientScope(routeID, headers), input.items))
	if err != nil {
		return nil, err
	}
	// A client may replay streamed item IDs absent from the terminal
	// response. An older anchor must not hide a newer complete snapshot,
	// but a matching prefix cannot override a different anchored lineage.
	if prefix != nil && (source == nil || (prefix.Root == source.Root &&
		len(prefix.Input) > len(source.Input) && conversationHasPrefix(prefix.Input, source.Input))) {
		source = prefix
	}
	return source, nil
}

// ownerReasoningRequest forwards the client's own items unsaved, keeping only
// reasoning proven to belong to the saved owner and no earlier provider IDs.
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
		// As in a protected continuation, readable items lose earlier owners'
		// provider IDs. Items that history cannot save are forwarded as sent.
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
	// A turn-state header from before the switch would name the earlier owner.
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
			// Vekil ended the attempt before the stream reported its end, for
			// example in a shutdown or at its streaming deadline. As after an
			// interrupt, only items followed by another event count as delivered.
			if err := t.releaseEndedAttempt(t.deliveryInfo.targetID, "proxy_ended", false); err != nil && !t.blocked {
				t.h.logConversationRecovery(t.operation, "blocked", "", "execution_uncertain")
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

// releaseFailedAttempt settles an attempt the upstream ended with a terminal
// failure event. Executable output inside that event, which staging cannot
// save, leaves execution uncertain. The caller holds t.mu.
func (t *conversationTurn) releaseFailedAttempt(envelope map[string]json.RawMessage, targetID, reason string) error {
	if conversationEventHasExecutableOutput(envelope) {
		// Staging cannot save it, and the turn stays uncertain when the
		// stream ends after this event too.
		t.executable, t.unstaged = true, true
		return conversationRequestError(errConversationIncomplete)
	}
	if t.saved {
		return conversationRequestError(errConversationIncomplete)
	}
	if t.executable {
		// The event before this one may still be on its way to the client.
		// Settle once the client reads this event (conversationCompletionBody.Read);
		// a turn that could not be saved anyway reports that now.
		if _, err := t.endedHistory(true); err != nil {
			return err
		}
		t.failureHandoff, t.failureOffset, t.failureTargetID = true, t.streamWritten, targetID
		return nil
	}
	return t.releaseEndedAttempt(targetID, reason, true)
}

// streamWrote counts the bytes written to the client's stream pipe.
func (t *conversationTurn) streamWrote(n int) {
	t.mu.Lock()
	t.streamWritten += int64(n)
	t.mu.Unlock()
}

// readStream counts bytes the client's consumer read. Consumers read the stream
// in order, so reading into a terminal failure event means every earlier event
// was handed off, and the attempt that event ended can settle.
func (t *conversationTurn) readStream(n int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streamRead += int64(n)
	if !t.failureHandoff || t.streamRead <= t.failureOffset {
		return nil
	}
	t.failureHandoff = false
	return t.releaseEndedAttempt(t.failureTargetID, "failure_event", true)
}

// An attempt that ends while the client stays connected leaves a known outcome:
// the client received exactly what Vekil handed off, whether the upstream ended
// it (reasons failure_event and stream_ended) or Vekil did, through a
// processing error, its streaming deadline or a shutdown (proxy_ended).
// Reasoning and assistant messages cannot run anything, so the attempt marker
// is cleared. A completed tool call may have run, so the items delivered are
// saved and the marker admits only a continuation that includes the call and
// its output. The failure reaches the client unchanged, so it can apply its own
// retry policy. Executable output that cannot be saved leaves execution
// uncertain. ended reports whether the client read past every handed-off
// event. The caller holds t.mu.
func (t *conversationTurn) releaseEndedAttempt(targetID, reason string, ended bool) error {
	if t.executable {
		if err := t.saveEndedHistory(targetID, ended); err != nil {
			return err
		}
		reason = "delivered_history_saved"
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

// conversationEventHasExecutableOutput reports whether a lifecycle or terminal
// event carries response output a client could execute. A malformed response
// object counts as executable.
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

// conversationItemInert reports whether an output item is reasoning or an
// assistant message: content a client displays or keeps, but cannot execute.
func conversationItemInert(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	return item.Type == "reasoning" || item.Type == "message"
}

// conversationEventInert reports whether a streamed output event can only
// belong to reasoning or an assistant message. Any other event, including an
// unrecognized one, may be part of a tool call or hosted tool execution.
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

// trackExecutableItem records a handed-off event of an item that is neither
// reasoning nor an assistant message. Clients run a tool call only once it is
// complete, so an item's first event, its argument deltas and hosted search
// progress make nothing runnable. Complete arguments without the finished item
// could be run but not saved, and an event of no item staging recognizes cannot
// be saved either. The caller holds t.mu.
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
		// Only the same item closes complete arguments seen at its index.
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

// saveEndedHistory saves the items an ended attempt delivered, after executable
// output reached the client. When the client read the end of the stream, every
// handed-off item counts as delivered. Codex runs a delivered tool call and
// resends it with its output; that continuation is admitted. The marker stays
// for any turn that branches from earlier history, which could repeat the call.
// Complete arguments without the finished item, content staging cannot save,
// a completed tool call among items not confirmed as delivered, or a failed
// save leaves execution uncertain. The caller holds t.mu.
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

// endedHistory returns the delivered items saveEndedHistory saves, or an error
// when execution stays uncertain. The caller holds t.mu.
func (t *conversationTurn) endedHistory(ended bool) (conversationInput, error) {
	if !ended {
		// A completed tool call the conservative rule leaves out may still have
		// reached the client; a snapshot without it could let a continuation
		// repeat it.
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
		if failure.progress == upstreamProgressTerminalFailure && failure.commitment == downstreamCommitmentNone && failure.cleanupDone {
			// Only an output-free terminal event is translated before commitment,
			// so like a committed one its outcome is known. Release the marker
			// and return the translated failure to the client.
			t.mu.Lock()
			err := t.releaseFailedAttempt(nil, "", "failure_event")
			t.mu.Unlock()
			if err != nil {
				return nil, true, err
			}
			return nil, false, nil
		}
		if t.clientEnded() && (errors.Is(failure.err, context.Canceled) || errors.Is(failure.err, context.DeadlineExceeded)) {
			// The client's disconnect ended this attempt, so its outcome is not
			// uncertain. An upstream failure that preceded the disconnect is.
			// Report the attempt's own failure; finish records the interrupt.
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
		if t.saved || t.failed || t.failureHandoff {
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
				// Upstreams emit keepalives while a long generation is quiet.
				// Output inside them is never staged.
				if conversationEventHasExecutableOutput(envelope) {
					t.executable, t.unstaged = true, true
				}
			default:
				// Unrecognized events count as executable so they cannot hide a
				// tool call.
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

// historySnapshot builds the immutable snapshot of this turn's history followed
// by output from one upstream response.
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
		// Copilot continuations use our durable snapshots, including when
		// store was omitted. Keep its IDs as local history anchors.
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

// observeDelivery stages the response and completed output items being sent
// to the client, so a client interrupt can save exactly the delivered history.
// Items that cannot be represented, or output from another response, disable
// the delivered snapshot. The caller holds t.mu.
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
		// Stop staging once the output could not fit a snapshot anyway.
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

// invalidateDelivery disables the delivered snapshot and drops staged items.
// The caller holds t.mu.
func (t *conversationTurn) invalidateDelivery() {
	t.deliveredInvalid = true
	t.staged, t.stagedIndexes, t.stagedBytes = nil, nil, 0
}

// deliveredOutput returns staged items whose event was followed by another
// handed-off event, or every staged item once the upstream ended the stream,
// in output_index order. The caller holds t.mu.
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

// A client that ends its own request owns the outcome: it received exactly the
// events delivered so far and decides whether to act on them. Save completed
// delivered items as a snapshot, so a continuation that includes them matches
// verified history, and release the attempt either way. Only the target's own
// delivered output is saved; the client cannot add items it did not receive.
// The caller holds t.mu.
func (t *conversationTurn) releaseInterruptedAttempt() {
	targetID := t.deliveryInfo.targetID
	delivered := t.deliveredOutput(false)
	if (len(delivered.items) > 0 || len(delivered.anchors) > 0) && !t.deliveredInvalid && t.deliveredResponseID != "" && t.haveDeliveryInfo {
		// Recover interrupted response-ID continuations from what the client
		// received, not from an upstream copy that may have continued.
		snapshot, err := t.historySnapshot(t.deliveredResponseID, t.deliveryInfo, delivered, false)
		if err == nil {
			err = t.store.save(snapshot)
		}
		if err == nil {
			t.pending = false
			t.h.logConversationRecovery(t.operation, "interrupted", targetID, "delivered_history_saved")
			return
		}
		// A reused response ID keeps the marker, like a completed turn does.
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

// clientEnded reports whether the client, rather than Vekil or its upstream,
// ended this turn's request. A shutdown is never the client's decision.
func (t *conversationTurn) clientEnded() bool {
	inbound := t.operation.inbound
	return inbound != nil && inbound.Err() != nil && !t.h.ShuttingDown()
}

// upstreamBodyEnd records how one attempt's upstream response body ended when
// the upstream, rather than Vekil, ended it: a clean EOF or a read error while
// the attempt's request was still live. Vekil's own close, cancellation or
// deadline is not recorded, even when the body then reports EOF.
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

// endedBy reports whether err, as the normalized stream surfaced it, is the
// upstream's own end of the body rather than an error Vekil raised while
// processing the stream.
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
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if releaseErr := b.turn.readStream(n); releaseErr != nil {
			return n, releaseErr
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		b.turn.mu.Lock()
		known := b.turn.saved || b.turn.unprotected || b.turn.failed
		released := false
		var releaseErr error
		if !known && !b.turn.clientEnded() {
			// The stream ended while the client was still connected, so it read
			// past every handed-off event: the outcome is known. A block recorded
			// earlier, such as a failed save, keeps execution uncertain, and a
			// later disconnect must not release it.
			if !b.turn.blocked {
				reason := "proxy_ended"
				if b.upstreamEnd.endedBy(err) {
					reason = "stream_ended"
				}
				releaseErr = b.turn.releaseEndedAttempt(b.targetID, reason, true)
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
			// Report the failed stream, not the conversation error Vekil raised
			// while it still looked uncertain.
			return n, errConversationStreamEnded
		}
		_, _, storageFailure := durableStateFailureDetails(err)
		if !known && !storageFailure && providerRequestErrorCode(err) == "" {
			return n, conversationRequestError(errConversationIncomplete)
		}
	}
	return n, err
}
