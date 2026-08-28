package proxy

import (
	"bytes"
	"compress/flate"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

const reasoningCarrierPrefix = "vekil1."

// A signature is client input, so a compression bomb is reachable, and one body holds many.
// The per-carrier cap must cover a group the store itself accepts, or our own decoder
// rejects a carrier we minted; the request budget is what actually bounds a bomb.
const (
	reasoningCarrierMaxDecodedBytes = responsesChatReplayMaxGroupBytes + (1 << 20)
	reasoningCarrierRequestBudget   = 8 << 20
	carriedDigestBytes              = 16
)

// Emit-side cap. Measured on session 4e6e968c: carriers were 78% of a 9.81 MB body at
// 880 tool turns and 8.9 KB per turn, so the 10 MiB ingress gate closes near turn 940.
// This is a cumulative wire-byte ceiling: once the client already carries the budget,
// or the next signature would cross it, no new carrier is emitted. The visible-transcript
// fallback preserves progress without exporting provider-owned replay mappings.
const reasoningCarrierInboundBudget = 2 << 20

// What the client already replays. Counted off the wire signatures, so it is the weight
// that returns on every later request no matter what vekil emits now.
type carrierInbound struct {
	Carriers int
	Bytes    int
	Starved  bool
}

func (i carrierInbound) saturated() bool { return i.Bytes >= reasoningCarrierInboundBudget }

// Where the cap is decided and where to say so; the response path has no logger of its own.
type carrierEmit struct {
	Inbound carrierInbound
	Log     *logger.Logger
}

// Carried, not inferred from item order: positional binding misattaches same-name parallel calls.
type carriedCall struct {
	ProxyID string `json:"proxy_id"`
	// Legacy decode only. Newly emitted carriers clear this field, and restoration never
	// trusts it: provider-owned call IDs stay in the process-local replay store.
	UpstreamID               string                              `json:"upstream_id,omitempty"`
	Name                     string                              `json:"name"`
	ItemIndex                int                                 `json:"item_index"`
	VisibleArgumentDigest    string                              `json:"visible_argument_digest,omitempty"`
	OriginalArgumentDigest   string                              `json:"original_argument_digest,omitempty"`
	VisibleOptionalDefaults  responsesChatReplayOptionalDefaults `json:"visible_optional_defaults,omitempty"`
	OriginalOptionalDefaults responsesChatReplayOptionalDefaults `json:"original_optional_defaults,omitempty"`
}

type reasoningCarrierPayload struct {
	Items         []json.RawMessage `json:"items"`
	Calls         []carriedCall     `json:"calls,omitempty"`
	TextItemIndex *int              `json:"text_item_index,omitempty"`
	// Binds a carrier to the route that minted it: Copilot's ciphertext is model-bound,
	// and on the policy path this re-picks the tier. RouteTag keys that second role;
	// the digest stays unkeyed so a restart can still restore items.
	RouteDigest              string `json:"route_digest,omitempty"`
	RouteTag                 string `json:"route_tag,omitempty"`
	ProjectionDigest         string `json:"projection_digest,omitempty"`
	OriginalProjectionDigest string `json:"original_projection_digest,omitempty"`
}

// A turn's Responses output and bindings, encoded into an Anthropic thinking block's
// signature so the CLIENT holds it rather than the replay store, whose TTL, eviction
// and restart each wedge a conversation. Chat has no such field, so it keeps the store.
type carriedTurn struct {
	Items              []json.RawMessage
	Calls              []carriedCall
	TextItemIndex      *int
	Route              responsesChatReplayRoute
	Projection         string
	OriginalProjection string
	Emit               *carrierEmit
}

// A carrier needs at least one ordering/reasoning item or visible-call binding.
func (t carriedTurn) present() bool { return len(t.Items) > 0 || len(t.Calls) > 0 }

type carriedReplay struct {
	Items                    []json.RawMessage
	Calls                    map[string]carriedCall
	TextItemIndex            *int
	RouteDigest              string
	RouteTagValid            bool
	ProjectionDigest         string
	OriginalProjectionDigest string
}

func (r carriedReplay) present() bool { return len(r.Items) > 0 || len(r.Calls) > 0 }

func carriedRouteDigest(route responsesChatReplayRoute) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		route.ProviderID, route.PublicModel, route.UpstreamModel, route.RouteID, route.PolicyTier,
	}, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// A carrier is client input, so its route claim is authority only if this process
// minted the tag. Losing the key on restart fails closed, which is the old 400.
var reasoningCarrierKey = sync.OnceValue(func() []byte {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil
	}
	return key
})

// Per-process key, so a restart invalidates every tag. That costs tier pinning and
// nothing else: routeSelectingCarriers is the only reader, and it is reached only
// from the policy path, so the tag is inert unless policy routing is configured.
//
// The whole replay payload is authenticated, not just the route digest. Otherwise a
// client can copy a valid tier tag onto rewritten calls and projection metadata, then
// use the forged replay shape to make that tier validate without classification.
func reasoningCarrierRouteTag(payload reasoningCarrierPayload) string {
	key := reasoningCarrierKey()
	if key == nil || payload.RouteDigest == "" {
		return ""
	}
	payload.RouteTag = ""
	authenticated, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(authenticated)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func reasoningCarrierRouteTagValid(payload reasoningCarrierPayload) bool {
	expected := reasoningCarrierRouteTag(payload)
	return expected != "" && hmac.Equal([]byte(expected), []byte(payload.RouteTag))
}

// A tier is authority the store used to hold, so only a carrier this process tagged
// may pick one; an untagged carrier still restores items under a route already decided.
func routeSelectingCarriers(carried map[string]carriedReplay) map[string]carriedReplay {
	selecting := make(map[string]carriedReplay, len(carried))
	for id, replay := range carried {
		if replay.RouteTagValid {
			selecting[id] = replay
		}
	}
	if len(selecting) == 0 {
		return nil
	}
	return selecting
}

// Mirrors responsesChatReplayGroup.matchesProjection: same canonical assistant text,
// same calls, same order, and the same canonical arguments.
func carriedProjectionDigest(content []byte, calls []responsesChatReplayProjectedCall) string {
	argumentDigests := make([][sha256.Size]byte, len(calls))
	for i, call := range calls {
		canonical, err := canonicalReplayArguments(call.Arguments)
		if err != nil {
			canonical = []byte(call.Arguments)
		}
		argumentDigests[i] = sha256.Sum256(canonical)
	}
	return carriedProjectionDigestWithArguments(content, calls, argumentDigests)
}

func carriedProjectionDigestWithArguments(content []byte, calls []responsesChatReplayProjectedCall, argumentDigests [][sha256.Size]byte) string {
	if len(calls) != len(argumentDigests) {
		return ""
	}
	sum := sha256.New()
	sum.Write(content)
	for i, call := range calls {
		sum.Write([]byte{0})
		sum.Write([]byte(call.ID))
		sum.Write([]byte{0})
		sum.Write([]byte(call.Name))
		sum.Write([]byte{0})
		sum.Write(argumentDigests[i][:])
	}
	return hex.EncodeToString(sum.Sum(nil)[:carriedDigestBytes])
}

// A digest is client input that keys a request-scoped map, so drop anything not ours.
func carriedDigest(value string) string {
	if len(value) != hex.EncodedLen(carriedDigestBytes) {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

func carriedTurnFromPublished(route responsesChatReplayRoute, outputItems []json.RawMessage, published responsesChatReplayPublished, emit carrierEmit) carriedTurn {
	calls := make([]carriedCall, len(published.Calls))
	for i, call := range published.Calls {
		calls[i] = carriedCall{
			ProxyID:                  call.ProxyCallID,
			Name:                     call.Name,
			ItemIndex:                call.OutputItemIndex,
			VisibleArgumentDigest:    hex.EncodeToString(call.visibleArgumentHash[:]),
			OriginalArgumentDigest:   hex.EncodeToString(call.originalArgumentHash[:]),
			VisibleOptionalDefaults:  cloneReplayOptionalDefaults(call.visibleOptionalDefaults),
			OriginalOptionalDefaults: cloneReplayOptionalDefaults(call.originalOptionalDefaults),
		}
	}
	turn := carriedTurn{
		Items:              outputItems,
		Calls:              calls,
		TextItemIndex:      carriedMessageItemIndex(outputItems),
		Route:              route,
		Projection:         carriedProjectionDigest(published.Projection.Content, published.Projection.Calls),
		OriginalProjection: carriedProjectionDigest(published.OriginalProjection.Content, published.OriginalProjection.Calls),
		Emit:               &emit,
	}
	return turn
}

// Debug, not warn: past the budget this is the steady state and fires every tool turn.
// Counts and sizes only -- a reasoning item is Copilot ciphertext and never logged.
func logCarrierSuppressed(emit carrierEmit, route responsesChatReplayRoute, dropped []json.RawMessage, calls, candidateBytes int) {
	if emit.Log == nil {
		return
	}
	items, itemBytes := 0, 0
	for _, item := range dropped {
		if itemType, _ := carriedItemHeader(item); itemType == "reasoning" {
			items++
			itemBytes += len(item)
		}
	}
	emit.Log.Debug("reasoning carrier omitted; the client's replay reached its byte budget",
		logger.F("model", route.PublicModel),
		logger.F("inbound_carriers", emit.Inbound.Carriers),
		logger.F("inbound_carrier_bytes", emit.Inbound.Bytes),
		logger.F("budget_bytes", reasoningCarrierInboundBudget),
		logger.F("dropped_reasoning_items", items),
		logger.F("dropped_reasoning_bytes", itemBytes),
		logger.F("dropped_calls", calls),
		logger.F("candidate_carrier_bytes", candidateBytes),
	)
}

// The carrier is visible client state, unlike the process-local replay store. Keep only
// the opaque reasoning fields and ordering placeholders needed to resume the turn;
// upstream ids, arguments, content, status and future fields must not become output.
func clientSafeCarrierItems(items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	safe := make([]json.RawMessage, len(items))
	for i, item := range items {
		var fields struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			EncryptedContent string `json:"encrypted_content"`
			Role             string `json:"role"`
		}
		if json.Unmarshal(item, &fields) != nil {
			safe[i] = json.RawMessage(`{}`)
			continue
		}
		var narrowed []byte
		var err error
		switch strings.TrimSpace(fields.Type) {
		case "reasoning":
			narrowed, err = json.Marshal(struct {
				Type             string `json:"type"`
				ID               string `json:"id,omitempty"`
				EncryptedContent string `json:"encrypted_content,omitempty"`
			}{Type: "reasoning", ID: fields.ID, EncryptedContent: fields.EncryptedContent})
		case "message":
			text, refusal, parseErr := responsesChatMessageContent(item)
			if parseErr != nil || refusal != "" {
				safe[i] = json.RawMessage(`{}`)
				continue
			}
			narrowed, err = json.Marshal(struct {
				Type      string `json:"type"`
				Role      string `json:"role"`
				TextBytes int    `json:"text_bytes"`
			}{Type: "message", Role: fields.Role, TextBytes: len(text)})
		case "function_call":
			narrowed, err = json.Marshal(struct {
				Type string `json:"type"`
			}{Type: "function_call"})
		default:
			safe[i] = json.RawMessage(`{}`)
			continue
		}
		if err != nil {
			safe[i] = json.RawMessage(`{}`)
			continue
		}
		safe[i] = narrowed
	}
	return safe
}

func clientSafeCarrierCalls(calls []carriedCall) []carriedCall {
	if len(calls) == 0 {
		return nil
	}
	safe := make([]carriedCall, len(calls))
	copy(safe, calls)
	for i := range safe {
		safe[i].UpstreamID = ""
	}
	return safe
}

type reasoningCarrierWirePayload struct {
	Items                    json.RawMessage `json:"items"`
	Calls                    json.RawMessage `json:"calls"`
	TextItemIndex            *int            `json:"text_item_index"`
	RouteDigest              string          `json:"route_digest"`
	RouteTag                 string          `json:"route_tag"`
	ProjectionDigest         string          `json:"projection_digest"`
	OriginalProjectionDigest string          `json:"original_projection_digest"`
}

// Decode attacker-controlled arrays without first allocating one element per entry.
// The byte cap bounds inflation; these count caps bound slice/map amplification.
func decodeBoundedCarrierArray[T any](raw json.RawMessage, maximum int) ([]T, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, false
	}
	values := make([]T, 0, min(maximum, 16))
	for decoder.More() {
		if len(values) >= maximum {
			return nil, false
		}
		var value T
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		values = append(values, value)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return values, true
}

func decodeReasoningCarrierPayload(payload []byte) (reasoningCarrierPayload, bool) {
	var wire reasoningCarrierWirePayload
	if json.Unmarshal(payload, &wire) != nil {
		return reasoningCarrierPayload{}, false
	}
	items, ok := decodeBoundedCarrierArray[json.RawMessage](wire.Items, responsesChatReplayMaxItems)
	if !ok {
		return reasoningCarrierPayload{}, false
	}
	calls, ok := decodeBoundedCarrierArray[carriedCall](wire.Calls, responsesChatReplayMaxCalls)
	if !ok {
		return reasoningCarrierPayload{}, false
	}
	return reasoningCarrierPayload{
		Items:                    items,
		Calls:                    calls,
		TextItemIndex:            wire.TextItemIndex,
		RouteDigest:              wire.RouteDigest,
		RouteTag:                 wire.RouteTag,
		ProjectionDigest:         wire.ProjectionDigest,
		OriginalProjectionDigest: wire.OriginalProjectionDigest,
	}, true
}

func encodeReasoningCarrier(turn carriedTurn) (string, error) {
	if !turn.present() {
		return "", nil
	}
	if turn.Emit != nil && turn.Emit.Inbound.saturated() {
		logCarrierSuppressed(*turn.Emit, turn.Route, turn.Items, len(turn.Calls), 0)
		return "", nil
	}
	digest := carriedRouteDigest(turn.Route)
	carrierPayload := reasoningCarrierPayload{
		Items:                    clientSafeCarrierItems(turn.Items),
		Calls:                    clientSafeCarrierCalls(turn.Calls),
		TextItemIndex:            turn.TextItemIndex,
		RouteDigest:              digest,
		ProjectionDigest:         turn.Projection,
		OriginalProjectionDigest: turn.OriginalProjection,
	}
	carrierPayload.RouteTag = reasoningCarrierRouteTag(carrierPayload)
	payload, err := json.Marshal(carrierPayload)
	if err != nil {
		return "", err
	}
	// Do not emit a carrier our own decoder will reject. The visible transcript remains a
	// complete recovery path; exporting provider call mappings is not an acceptable fallback.
	if len(payload) > reasoningCarrierMaxDecodedBytes {
		if turn.Emit != nil {
			logCarrierSuppressed(*turn.Emit, turn.Route, turn.Items, len(turn.Calls), len(payload))
		}
		return "", nil
	}
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	signature := reasoningCarrierPrefix + base64.RawURLEncoding.EncodeToString(compressed.Bytes())
	if turn.Emit != nil && len(signature) > reasoningCarrierInboundBudget-turn.Emit.Inbound.Bytes {
		logCarrierSuppressed(*turn.Emit, turn.Route, turn.Items, len(turn.Calls), len(signature))
		return "", nil
	}
	return signature, nil
}

// Unusable carriers return false, not error: failing turns lost continuity into a dead conversation.
func decodeReasoningCarrier(signature string, budget *int) (carriedReplay, bool) {
	if !strings.HasPrefix(signature, reasoningCarrierPrefix) {
		return carriedReplay{}, false
	}
	compressed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(signature, reasoningCarrierPrefix))
	if err != nil || len(compressed) == 0 {
		return carriedReplay{}, false
	}
	limit := reasoningCarrierMaxDecodedBytes
	if budget != nil {
		if *budget <= 0 {
			return carriedReplay{}, false
		}
		limit = min(limit, *budget)
	}
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() { _ = reader.Close() }()
	payload, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if budget != nil {
		*budget -= len(payload)
	}
	if err != nil || len(payload) > limit {
		return carriedReplay{}, false
	}
	decoded, ok := decodeReasoningCarrierPayload(payload)
	if !ok || (len(decoded.Items) == 0 && len(decoded.Calls) == 0) {
		return carriedReplay{}, false
	}
	replay := carriedReplay{
		Items:                    decoded.Items,
		Calls:                    make(map[string]carriedCall, len(decoded.Calls)),
		TextItemIndex:            decoded.TextItemIndex,
		RouteDigest:              decoded.RouteDigest,
		RouteTagValid:            reasoningCarrierRouteTagValid(decoded),
		ProjectionDigest:         carriedDigest(decoded.ProjectionDigest),
		OriginalProjectionDigest: carriedDigest(decoded.OriginalProjectionDigest),
	}
	for _, call := range decoded.Calls {
		replay.Calls[call.ProxyID] = call
	}
	return replay, true
}

// `thinking` is empty on purpose: the block is transport, not content.
func reasoningCarrierBlock(turn carriedTurn) (*models.ContentBlock, error) {
	signature, err := encodeReasoningCarrier(turn)
	if err != nil || signature == "" {
		return nil, err
	}
	return &models.ContentBlock{Type: "thinking", Thinking: stringPtr(""), Signature: signature}, nil
}

// Keyed by tool_use id, not message index, because clients trim history. Every
// tool_use in a message maps to that message's carrier: one output array per turn.
// The budget is charged newest-first, so a long transcript keeps the reasoning that will
// actually survive the outbound trim; precedence below is still oldest-first.
func extractCarriedReasoning(messages []models.AnthropicMessage) (carried map[string]carriedReplay, inbound carrierInbound) {
	carried = make(map[string]carriedReplay)
	blocksByMessage := make(map[int][]models.ContentBlock, len(messages))
	order := make([]int, 0, len(messages))
	for index, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		var blocks []models.ContentBlock
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		blocksByMessage[index] = blocks
		order = append(order, index)
	}

	// Spend the budget NEWEST-first. Charging oldest-first spent it on turns trimAgedReasoning
	// then discards on the way out (see agedReasoningToolTurns), so a long transcript could
	// pay its whole 8 MiB for reasoning that never left the process and send none from the
	// retained window. Only the ORDER of spending changes here: the precedence below still
	// runs oldest-first, so which carrier wins an id is exactly what it was.
	decoded := make(map[int]carriedReplay, len(order))
	budget := reasoningCarrierRequestBudget
	for i := len(order) - 1; i >= 0; i-- {
		index := order[i]
		spent := false
		for _, block := range blocksByMessage[index] {
			if block.Type != "thinking" && block.Type != "redacted_thinking" {
				continue
			}
			if !strings.HasPrefix(block.Signature, reasoningCarrierPrefix) {
				continue
			}
			// Weighed before the budget decides, so a starved request still reports it all.
			inbound.Carriers++
			inbound.Bytes += len(block.Signature)
			// Charged on the attempt, so corrupt blocks cannot each buy an inflate.
			if spent {
				continue
			}
			spent = true
			if budget <= 0 {
				inbound.Starved = true
				continue
			}
			if replay, ok := decodeReasoningCarrier(block.Signature, &budget); ok {
				decoded[index] = replay
			} else if budget <= 0 {
				// The attempt is charged before it fails, so the carrier that EXHAUSTS
				// the budget starves as surely as one that found none left. Checking
				// only before an attempt misses it whenever it is the last carrier.
				inbound.Starved = true
			}
		}
	}

	for _, index := range order {
		replay, ok := decoded[index]
		if !ok || !replay.present() {
			continue
		}
		var toolUseIDs []string
		for _, block := range blocksByMessage[index] {
			if block.Type == "tool_use" && block.ID != "" {
				toolUseIDs = append(toolUseIDs, block.ID)
			}
		}
		// A carrier names the calls it covers, so index by its OWN mapping and not only by
		// tool_use blocks sitting beside it. Parallel calls interleaved with their results
		// split one logical turn across several wire messages, and the carrier can land in
		// a message holding no tool_use at all; requiring co-location discarded it and left
		// those ids unresolvable, which is the wedge this carrier exists to prevent.
		for id := range replay.Calls {
			if _, taken := carried[id]; !taken {
				carried[id] = replay
			}
		}
		// Co-located ids still win: a turn's own carrier is the most specific answer for it.
		for _, id := range toolUseIDs {
			carried[id] = replay
		}
	}
	if len(carried) == 0 {
		return nil, inbound
	}
	return carried, inbound
}

// A mixed group hands Copilot a chain that does not match the calls beside it.
func carriedReplayForCalls(carried map[string]carriedReplay, projected []responsesChatReplayProjectedCall) (carriedReplay, bool) {
	if len(carried) == 0 || len(projected) == 0 {
		return carriedReplay{}, false
	}
	var replay carriedReplay
	for i, call := range projected {
		found, ok := carried[call.ID]
		if !ok || !found.present() {
			return carriedReplay{}, false
		}
		if i == 0 {
			replay = found
			continue
		}
		if len(replay.Items) != len(found.Items) {
			return carriedReplay{}, false
		}
		for j := range replay.Items {
			if string(replay.Items[j]) != string(found.Items[j]) {
				return carriedReplay{}, false
			}
		}
		if !sameCarriedItemIndex(replay.TextItemIndex, found.TextItemIndex) {
			return carriedReplay{}, false
		}
	}
	return replay, true
}

// A shape we did not mint, or that the transcript cannot rebuild, restores nothing.
func carriedItemsWellShaped(items []json.RawMessage) bool {
	messages := 0
	messageWithoutLength := false
	messageBytes := 0
	for _, item := range items {
		var header struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			TextBytes *int   `json:"text_bytes"`
		}
		if json.Unmarshal(item, &header) != nil {
			return false
		}
		switch strings.TrimSpace(header.Type) {
		case "reasoning":
		case "message":
			messages++
			if strings.TrimSpace(header.Role) != "assistant" {
				return false
			}
			if header.TextBytes == nil {
				messageWithoutLength = true
				continue
			}
			if *header.TextBytes < 0 || *header.TextBytes > reasoningCarrierMaxDecodedBytes-messageBytes {
				return false
			}
			messageBytes += *header.TextBytes
		case "function_call":
		default:
			return false
		}
	}
	return messages <= 1 || !messageWithoutLength
}

func carriedMessageSegments(items []json.RawMessage, assistantText string) (map[int]string, bool) {
	type messageSlot struct {
		index int
		bytes *int
	}
	var slots []messageSlot
	for index, item := range items {
		var header struct {
			Type      string `json:"type"`
			TextBytes *int   `json:"text_bytes"`
		}
		if json.Unmarshal(item, &header) != nil {
			return nil, false
		}
		if strings.TrimSpace(header.Type) == "message" {
			slots = append(slots, messageSlot{index: index, bytes: header.TextBytes})
		}
	}
	if len(slots) == 0 {
		return nil, true
	}
	if len(slots) == 1 && slots[0].bytes == nil {
		return map[int]string{slots[0].index: assistantText}, true
	}

	segments := make(map[int]string, len(slots))
	offset := 0
	for _, slot := range slots {
		if slot.bytes == nil || *slot.bytes < 0 || *slot.bytes > len(assistantText)-offset {
			return nil, false
		}
		end := offset + *slot.bytes
		segment := assistantText[offset:end]
		if !utf8.ValidString(segment) {
			return nil, false
		}
		segments[slot.index] = segment
		offset = end
	}
	if offset != len(assistantText) {
		return nil, false
	}
	return segments, true
}

func carriedArgumentDigest(value string) ([sha256.Size]byte, bool) {
	var digest [sha256.Size]byte
	if len(value) != hex.EncodedLen(len(digest)) {
		return digest, false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(digest) {
		return digest, false
	}
	copy(digest[:], decoded)
	return digest, true
}

func carriedArgumentsMatch(arguments, digestValue string, defaults responsesChatReplayOptionalDefaults) ([sha256.Size]byte, bool) {
	want, ok := carriedArgumentDigest(digestValue)
	if !ok {
		return want, false
	}
	canonical, err := canonicalReplayArguments(arguments)
	if err != nil {
		return want, false
	}
	if sha256.Sum256(canonical) == want {
		return want, true
	}
	normalized, changed, err := canonicalReplayArgumentsWithoutOptionalDefaults(arguments, defaults)
	return want, err == nil && changed && sha256.Sum256(normalized) == want
}

func carriedProjectionMatches(replay carriedReplay, content []byte, projected []responsesChatReplayProjectedCall) bool {
	direct := carriedProjectionDigest(content, projected)
	if direct != "" && (direct == replay.ProjectionDigest || direct == replay.OriginalProjectionDigest) {
		return true
	}

	visible := make([][sha256.Size]byte, len(projected))
	original := make([][sha256.Size]byte, len(projected))
	visibleOK, originalOK := true, replay.OriginalProjectionDigest != ""
	for i, projectedCall := range projected {
		call, ok := replay.Calls[projectedCall.ID]
		if !ok || call.Name != strings.TrimSpace(projectedCall.Name) {
			return false
		}
		if visibleOK {
			visible[i], visibleOK = carriedArgumentsMatch(projectedCall.Arguments, call.VisibleArgumentDigest, call.VisibleOptionalDefaults)
		}
		if originalOK {
			original[i], originalOK = carriedArgumentsMatch(projectedCall.Arguments, call.OriginalArgumentDigest, call.OriginalOptionalDefaults)
		}
	}
	if visibleOK && carriedProjectionDigestWithArguments(content, projected, visible) == replay.ProjectionDigest {
		return true
	}
	return originalOK && carriedProjectionDigestWithArguments(content, projected, original) == replay.OriginalProjectionDigest
}

// The store's binding without the store: route, projection and each call's own minted id
// must match, and indices mirror Publish. "" means restored, else the guard that refused.
func carriedRestoredCalls(carried map[string]carriedReplay, projected []responsesChatReplayProjectedCall, route responsesChatReplayRoute, projectionContent json.RawMessage) (responsesChatRestoredCalls, string) {
	canonicalContent, err := canonicalReplayJSONValue(projectionContent)
	if err != nil {
		return responsesChatRestoredCalls{}, "projection"
	}
	replay, ok := carriedReplayForCalls(carried, projected)
	switch {
	case !ok:
		return responsesChatRestoredCalls{}, "absent"
	case replay.RouteDigest != carriedRouteDigest(route):
		return responsesChatRestoredCalls{}, "route"
	case !carriedProjectionMatches(replay, canonicalContent, projected):
		return responsesChatRestoredCalls{}, "projection"
	case !carriedItemsWellShaped(replay.Items):
		return responsesChatRestoredCalls{}, "shape"
	}
	var assistantText string
	if json.Unmarshal(canonicalContent, &assistantText) != nil {
		return responsesChatRestoredCalls{}, "projection"
	}
	if _, ok := carriedMessageSegments(replay.Items, assistantText); !ok {
		return responsesChatRestoredCalls{}, "projection"
	}
	calls := make([]responsesChatReplayResolvedCall, len(projected))
	boundCallItems := make(map[int]struct{}, len(projected))
	lastItemIndex := -1
	for i, projectedCall := range projected {
		call, known := replay.Calls[projectedCall.ID]
		if !known || call.Name != strings.TrimSpace(projectedCall.Name) || call.ItemIndex <= lastItemIndex {
			return responsesChatRestoredCalls{}, "binding"
		}
		if call.ItemIndex >= len(replay.Items) {
			return responsesChatRestoredCalls{}, "binding"
		}
		itemType, _ := carriedItemHeader(replay.Items[call.ItemIndex])
		if itemType != "function_call" {
			return responsesChatRestoredCalls{}, "binding"
		}
		if _, duplicate := boundCallItems[call.ItemIndex]; duplicate {
			return responsesChatRestoredCalls{}, "binding"
		}
		boundCallItems[call.ItemIndex] = struct{}{}
		lastItemIndex = call.ItemIndex
		upstreamCallID := projectedCall.ID
		if selfDescribed, ok := responsesChatReplayUpstreamCallID(projectedCall.ID); ok {
			upstreamCallID = selfDescribed
		}
		calls[i] = responsesChatReplayResolvedCall{
			ProxyCallID: projectedCall.ID,
			// Self-describing public IDs recover their upstream mapping. Opaque IDs
			// still rebuild both sides under the same proxy ID. A legacy carrier's
			// serialized provider ID remains deliberately ignored.
			UpstreamCallID:  upstreamCallID,
			Name:            call.Name,
			OutputItemIndex: call.ItemIndex,
			OutputItem:      replay.Items[call.ItemIndex],
		}
	}
	for index, item := range replay.Items {
		if itemType, _ := carriedItemHeader(item); itemType == "function_call" {
			if _, bound := boundCallItems[index]; !bound {
				return responsesChatRestoredCalls{}, "binding"
			}
		}
	}
	digest := sha256.New()
	for _, item := range replay.Items {
		digest.Write(item)
	}
	return responsesChatRestoredCalls{
		Key:           "carrier:" + replay.ProjectionDigest + ":" + hex.EncodeToString(digest.Sum(nil)),
		OutputItems:   replay.Items,
		Calls:         calls,
		TextItemIndex: replay.TextItemIndex,
		Rebuild:       true,
	}, ""
}

func sameCarriedItemIndex(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func carriedMessageItemIndex(items []json.RawMessage) *int {
	for index, item := range items {
		if itemType, _ := carriedItemHeader(item); itemType == "message" {
			return intVal(index)
		}
	}
	return nil
}

func carriedItemHeader(item json.RawMessage) (itemType, callID string) {
	var header struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(item, &header) != nil {
		return "", ""
	}
	return strings.TrimSpace(header.Type), strings.TrimSpace(header.CallID)
}

// A reasoning item keeps only its ciphertext and id -- Copilot's own, and the one thing
// a transcript cannot rebuild. Text and calls come from the transcript the classifier read.
func reconstructCarriedRestore(restored responsesChatRestoredCalls, projected []responsesChatReplayProjectedCall, assistantText string) responsesChatRestoredCalls {
	visible := make(map[string]responsesChatReplayProjectedCall, len(projected))
	for _, call := range projected {
		visible[call.ID] = call
	}
	callByItemIndex := make(map[int]int, len(restored.Calls))
	for i, call := range restored.Calls {
		callByItemIndex[call.OutputItemIndex] = i
	}
	items := make([]json.RawMessage, 0, len(restored.OutputItems))
	calls := make([]responsesChatReplayResolvedCall, len(restored.Calls))
	copy(calls, restored.Calls)
	rebuild := func(i int) {
		call := visible[calls[i].ProxyCallID]
		calls[i].OutputItem = responsesFunctionCallItem(calls[i].UpstreamCallID, call.Name, call.Arguments)
		calls[i].OutputItemIndex = len(items)
		items = append(items, calls[i].OutputItem)
	}
	// An ID-only restore has no carrier placeholders. Rebuild the visible text
	// and complete ordered call projection directly from the client transcript.
	if len(restored.OutputItems) == 0 {
		if assistantText != "" {
			items = appendAssistantHistoryMessage(items, assistantText)
		}
		for i := range calls {
			rebuild(i)
		}
		restored.OutputItems = items
		restored.Calls = calls
		return restored
	}
	messageSegments, _ := carriedMessageSegments(restored.OutputItems, assistantText)
	textSlot := -1
	if len(messageSegments) == 0 {
		textSlot = carriedTextSlot(restored.OutputItems, callByItemIndex)
	}
	for index, item := range restored.OutputItems {
		if segment, ok := messageSegments[index]; ok {
			items = appendAssistantHistoryMessage(items, segment)
		} else if index == textSlot && assistantText != "" {
			items = appendAssistantHistoryMessage(items, assistantText)
		}
		if i, ok := callByItemIndex[index]; ok {
			rebuild(i)
			continue
		}
		if itemType, _ := carriedItemHeader(item); itemType == "reasoning" {
			if reasoning := carriedReasoningCiphertext(item); reasoning != nil {
				items = append(items, reasoning)
			}
		}
	}
	restored.OutputItems = items
	restored.Calls = calls
	return restored
}

// Legacy carriers have one flattened text slot. Prefer their message placeholder, then the
// first call, so text that followed a call is not hoisted ahead of it.
func carriedTextSlot(items []json.RawMessage, callByItemIndex map[int]int) int {
	firstCall := -1
	for index, item := range items {
		if itemType, _ := carriedItemHeader(item); itemType == "message" {
			return index
		}
		if _, isCall := callByItemIndex[index]; isCall && firstCall < 0 {
			firstCall = index
		}
	}
	return firstCall
}

// summary, content and every other field are plain text no policy check ever read.
func carriedReasoningCiphertext(item json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil || rawJSONString(fields["encrypted_content"]) == "" {
		return nil
	}
	narrowed := map[string]json.RawMessage{
		"type":              json.RawMessage(`"reasoning"`),
		"encrypted_content": fields["encrypted_content"],
		"summary":           json.RawMessage(`[]`),
		"content":           json.RawMessage(`[]`),
	}
	if id, ok := fields["id"]; ok {
		narrowed["id"] = id
	}
	rebuilt, err := json.Marshal(narrowed)
	if err != nil {
		return nil
	}
	return rebuilt
}

func prependCarriedReasoning(resp *models.AnthropicResponse, turn carriedTurn) *models.AnthropicResponse {
	if resp == nil || !turn.present() {
		return resp
	}
	carrier, err := reasoningCarrierBlock(turn)
	if err != nil || carrier == nil {
		return resp
	}
	resp.Content = append([]models.ContentBlock{*carrier}, resp.Content...)
	return resp
}

// stripVekilCarrierBlocks removes vekil's own carrier blocks from an Anthropic request body,
// restoring the transcript to the shape it would have had if vekil had never injected one.
//
// A direct Anthropic route is another provider's endpoint. The carrier holds Copilot's opaque
// reasoning ciphertext and a route tag keyed to this process, and Anthropic validates the
// signature on thinking blocks it issued -- so forwarding one hands a third party another
// provider's payload and invites a rejection on a signature Anthropic never wrote. A client
// that switches models mid-conversation carries ours along without knowing it is ours.
//
// Operates on the raw body rather than the parsed request so every field vekil does not model
// survives untouched. An assistant message left with no content is dropped: it existed only to
// carry the block, which is exactly the shape the client would have sent without us.
func stripVekilCarrierBlocks(body []byte) ([]byte, int) {
	// No raw-bytes prefix scan before the parse, deliberately. A `bytes.Contains`
	// fast path here was a correctness hole, not an optimization: JSON escapes are
	// decoded by the parse but invisible to a byte scan, so a signature written as
	// "\u0076ekil1...." left the raw body without the literal prefix, returned the
	// body unchanged, and forwarded vekil's own Copilot reasoning to Anthropic --
	// the exact leak this function exists to prevent. Any cheap pre-check over
	// unparsed bytes has the same hole, because the escape can move to any
	// character. The parse below is the only thing that sees what the peer sees,
	// and the caller unmarshals this body immediately afterwards regardless.
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, 0
	}
	var messages []json.RawMessage
	if raw, ok := root["messages"]; !ok || json.Unmarshal(raw, &messages) != nil {
		return body, 0
	}
	stripped := 0
	kept := make([]json.RawMessage, 0, len(messages))
	for _, raw := range messages {
		var message map[string]json.RawMessage
		if json.Unmarshal(raw, &message) != nil {
			kept = append(kept, raw)
			continue
		}
		var role string
		if json.Unmarshal(message["role"], &role) != nil || role != "assistant" {
			kept = append(kept, raw)
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(message["content"], &blocks) != nil {
			kept = append(kept, raw)
			continue
		}
		remaining := make([]json.RawMessage, 0, len(blocks))
		for _, block := range blocks {
			var header struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			}
			if json.Unmarshal(block, &header) == nil &&
				(header.Type == "thinking" || header.Type == "redacted_thinking") &&
				strings.HasPrefix(header.Signature, reasoningCarrierPrefix) {
				stripped++
				continue
			}
			remaining = append(remaining, block)
		}
		if len(remaining) == len(blocks) {
			kept = append(kept, raw)
			continue
		}
		if len(remaining) == 0 {
			continue
		}
		content, err := json.Marshal(remaining)
		if err != nil {
			return body, 0
		}
		message["content"] = content
		rewritten, err := json.Marshal(message)
		if err != nil {
			return body, 0
		}
		kept = append(kept, rewritten)
	}
	if stripped == 0 {
		return body, 0
	}
	rewrittenMessages, err := json.Marshal(kept)
	if err != nil {
		return body, 0
	}
	root["messages"] = rewrittenMessages
	rewritten, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return rewritten, stripped
}
