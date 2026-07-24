package proxy

import (
	"bytes"
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	responsesChatReplayCallIDPrefix = "call_vekil_"

	responsesChatReplayTTL           = time.Hour
	responsesChatReplayMaxGroups     = 2048
	responsesChatReplayMaxGroupBytes = 2 << 20
	responsesChatReplayMaxTotalBytes = 64 << 20
	responsesChatReplayMaxItems      = 256
	responsesChatReplayMaxCalls      = 128

	responsesChatReplayRandomBytes   = 16
	responsesChatReplayIDLength      = len(responsesChatReplayCallIDPrefix) + 22
	responsesChatReplayMaxIDAttempts = 1024
)

const (
	responsesChatReplayMissingCode    = "responses_replay_state_missing"
	responsesChatReplayMixedCode      = "responses_replay_group_mismatch"
	responsesChatReplayProjectionCode = "responses_replay_projection_mismatch"
	responsesChatReplayTooLargeCode   = "responses_replay_group_too_large"
	responsesChatReplayClosedCode     = "responses_replay_store_closed"
)

const (
	responsesChatReplayMissingMessage    = "Responses-backed tool state is no longer available; restart the assistant tool-call turn."
	responsesChatReplayMixedMessage      = "Assistant tool calls reference multiple Responses replay groups."
	responsesChatReplayProjectionMessage = "Assistant tool-call projection does not match stored Responses replay state."
	responsesChatReplayTooLargeMessage   = "Responses replay group exceeds a configured storage limit."
	responsesChatReplayClosedMessage     = "Responses replay store is closed."
)

type responsesChatReplayMissingError struct{}

func (*responsesChatReplayMissingError) Error() string      { return responsesChatReplayMissingMessage }
func (*responsesChatReplayMissingError) ReplayCode() string { return responsesChatReplayMissingCode }
func (*responsesChatReplayMissingError) Is(target error) bool {
	_, ok := target.(*responsesChatReplayMissingError)
	return ok
}

type responsesChatReplayMixedError struct{}

func (*responsesChatReplayMixedError) Error() string      { return responsesChatReplayMixedMessage }
func (*responsesChatReplayMixedError) ReplayCode() string { return responsesChatReplayMixedCode }
func (*responsesChatReplayMixedError) Is(target error) bool {
	_, ok := target.(*responsesChatReplayMixedError)
	return ok
}

type responsesChatReplayProjectionError struct {
	Reason string
}

func (*responsesChatReplayProjectionError) Error() string {
	return responsesChatReplayProjectionMessage
}
func (*responsesChatReplayProjectionError) ReplayCode() string {
	return responsesChatReplayProjectionCode
}
func (*responsesChatReplayProjectionError) Is(target error) bool {
	_, ok := target.(*responsesChatReplayProjectionError)
	return ok
}

type responsesChatReplayLimit string

const (
	responsesChatReplayLimitItems      responsesChatReplayLimit = "items"
	responsesChatReplayLimitCalls      responsesChatReplayLimit = "calls"
	responsesChatReplayLimitGroupBytes responsesChatReplayLimit = "group_bytes"
	responsesChatReplayLimitTotalBytes responsesChatReplayLimit = "total_bytes"
)

type responsesChatReplayTooLargeError struct {
	Limit   responsesChatReplayLimit
	Actual  int
	Maximum int
}

func (*responsesChatReplayTooLargeError) Error() string { return responsesChatReplayTooLargeMessage }
func (*responsesChatReplayTooLargeError) ReplayCode() string {
	return responsesChatReplayTooLargeCode
}
func (*responsesChatReplayTooLargeError) Is(target error) bool {
	_, ok := target.(*responsesChatReplayTooLargeError)
	return ok
}

type responsesChatReplayClosedError struct{}

func (*responsesChatReplayClosedError) Error() string      { return responsesChatReplayClosedMessage }
func (*responsesChatReplayClosedError) ReplayCode() string { return responsesChatReplayClosedCode }
func (*responsesChatReplayClosedError) Is(target error) bool {
	_, ok := target.(*responsesChatReplayClosedError)
	return ok
}

var (
	errResponsesChatReplayMissing    = &responsesChatReplayMissingError{}
	errResponsesChatReplayMixed      = &responsesChatReplayMixedError{}
	errResponsesChatReplayProjection = &responsesChatReplayProjectionError{}
	errResponsesChatReplayTooLarge   = &responsesChatReplayTooLargeError{}
	errResponsesChatReplayClosed     = &responsesChatReplayClosedError{}
)

type responsesChatReplayRoute struct {
	ProviderID    string
	PublicModel   string
	UpstreamModel string
	RouteID       string
	PolicyTier    string
}

type responsesChatReplayProjectedCall struct {
	ID        string
	Name      string
	Arguments string
}

type responsesChatReplayAssistantProjection struct {
	Content json.RawMessage
	Calls   []responsesChatReplayProjectedCall
}

type responsesChatReplayOptionalDefaults map[string]json.RawMessage
type responsesChatReplayToolDefaults map[string]responsesChatReplayOptionalDefaults

type responsesChatReplayPublishCall struct {
	UpstreamCallID    string
	Name              string
	VisibleArguments  string
	OriginalArguments *string
	OutputItemIndex   int
	OptionalDefaults  responsesChatReplayOptionalDefaults
}

type responsesChatReplayPublishRequest struct {
	Route            responsesChatReplayRoute
	AssistantContent json.RawMessage
	OutputItems      []json.RawMessage
	Calls            []responsesChatReplayPublishCall
}

type responsesChatReplayProjectionMatch uint8

const (
	responsesChatReplayProjectionVisible responsesChatReplayProjectionMatch = iota + 1
	responsesChatReplayProjectionOriginal
)

type responsesChatReplayResolvedCall struct {
	ProxyCallID     string
	UpstreamCallID  string
	Name            string
	OutputItemIndex int
	OutputItem      json.RawMessage
}

type responsesChatReplayPublished struct {
	GroupID            uint64
	Projection         responsesChatReplayAssistantProjection
	OriginalProjection responsesChatReplayAssistantProjection
	Calls              []responsesChatReplayResolvedCall
	CreatedAt          time.Time
	ByteSize           int
}

type responsesChatReplayResolution struct {
	GroupID         uint64
	Route           responsesChatReplayRoute
	ProjectionMatch responsesChatReplayProjectionMatch
	OutputItems     []json.RawMessage
	Calls           []responsesChatReplayResolvedCall
	CreatedAt       time.Time
	ByteSize        int
}

type responsesChatReplayStoreStats struct {
	Groups     int
	Calls      int
	TotalBytes int
	Closed     bool
}

type responsesChatReplayStoreOptions struct {
	TTL           time.Duration
	MaxGroups     int
	MaxGroupBytes int
	MaxTotalBytes int
	MaxItems      int
	MaxCalls      int
	Now           func() time.Time
	Random        io.Reader
}

type responsesChatReplayStoredCall struct {
	proxyCallID              string
	upstreamCallID           string
	name                     string
	visibleHash              [sha256.Size]byte
	originalHash             [sha256.Size]byte
	visibleOptionalDefaults  responsesChatReplayOptionalDefaults
	originalOptionalDefaults responsesChatReplayOptionalDefaults
	outputItemIndex          int
}

type responsesChatReplayGroup struct {
	id               uint64
	route            responsesChatReplayRoute
	assistantContent []byte
	outputItems      []json.RawMessage
	calls            []responsesChatReplayStoredCall
	createdAt        time.Time
	expiresAt        time.Time
	byteSize         int
	lruElement       *list.Element
}

type responsesChatReplayCallRef struct {
	groupID uint64
}

type responsesChatReplayPreparedCall struct {
	upstreamCallID           string
	name                     string
	visibleArguments         string
	originalArguments        string
	visibleHash              [sha256.Size]byte
	originalHash             [sha256.Size]byte
	visibleOptionalDefaults  responsesChatReplayOptionalDefaults
	originalOptionalDefaults responsesChatReplayOptionalDefaults
	outputItemIndex          int
}

type responsesChatReplayPreparedGroup struct {
	route            responsesChatReplayRoute
	assistantContent []byte
	outputItems      []json.RawMessage
	calls            []responsesChatReplayPreparedCall
	byteSize         int
}

type responsesChatReplayStore struct {
	mu sync.Mutex

	groups      map[uint64]*responsesChatReplayGroup
	callsByID   map[string]responsesChatReplayCallRef
	lru         *list.List
	totalBytes  int
	nextGroupID uint64
	closed      bool

	ttl           time.Duration
	maxGroups     int
	maxGroupBytes int
	maxTotalBytes int
	maxItems      int
	maxCalls      int
	now           func() time.Time
	random        io.Reader
}

func newResponsesChatReplayStore() *responsesChatReplayStore {
	return newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{})
}

func newResponsesChatReplayStoreWithOptions(options responsesChatReplayStoreOptions) *responsesChatReplayStore {
	if options.TTL <= 0 {
		options.TTL = responsesChatReplayTTL
	}
	if options.MaxGroups <= 0 {
		options.MaxGroups = responsesChatReplayMaxGroups
	}
	if options.MaxGroupBytes <= 0 {
		options.MaxGroupBytes = responsesChatReplayMaxGroupBytes
	}
	if options.MaxTotalBytes <= 0 {
		options.MaxTotalBytes = responsesChatReplayMaxTotalBytes
	}
	if options.MaxItems <= 0 {
		options.MaxItems = responsesChatReplayMaxItems
	}
	if options.MaxCalls <= 0 {
		options.MaxCalls = responsesChatReplayMaxCalls
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return &responsesChatReplayStore{
		groups:        make(map[uint64]*responsesChatReplayGroup),
		callsByID:     make(map[string]responsesChatReplayCallRef),
		lru:           list.New(),
		ttl:           options.TTL,
		maxGroups:     options.MaxGroups,
		maxGroupBytes: options.MaxGroupBytes,
		maxTotalBytes: options.MaxTotalBytes,
		maxItems:      options.MaxItems,
		maxCalls:      options.MaxCalls,
		now:           options.Now,
		random:        options.Random,
	}
}

func (s *responsesChatReplayStore) Publish(request responsesChatReplayPublishRequest) (responsesChatReplayPublished, error) {
	if s == nil {
		return responsesChatReplayPublished{}, errResponsesChatReplayClosed
	}
	if err := s.checkOpen(); err != nil {
		return responsesChatReplayPublished{}, err
	}

	prepared, err := s.preparePublish(request)
	if err != nil {
		return responsesChatReplayPublished{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return responsesChatReplayPublished{}, errResponsesChatReplayClosed
	}
	now := s.now()
	s.expireLocked(now)

	proxyIDs := make([]string, len(prepared.calls))
	reserved := make(map[string]struct{}, len(prepared.calls))
	for i := range prepared.calls {
		proxyID, generateErr := s.generateUniqueCallIDLocked(reserved)
		if generateErr != nil {
			return responsesChatReplayPublished{}, fmt.Errorf("generate Responses replay call ID: %w", generateErr)
		}
		proxyIDs[i] = proxyID
		reserved[proxyID] = struct{}{}
	}

	groupID := s.nextGroupIDLocked()
	storedCalls := make([]responsesChatReplayStoredCall, len(prepared.calls))
	visibleCalls := make([]responsesChatReplayProjectedCall, len(prepared.calls))
	originalCalls := make([]responsesChatReplayProjectedCall, len(prepared.calls))
	resolvedCalls := make([]responsesChatReplayResolvedCall, len(prepared.calls))
	for i, call := range prepared.calls {
		storedCalls[i] = responsesChatReplayStoredCall{
			proxyCallID:              proxyIDs[i],
			upstreamCallID:           call.upstreamCallID,
			name:                     call.name,
			visibleHash:              call.visibleHash,
			originalHash:             call.originalHash,
			visibleOptionalDefaults:  cloneReplayOptionalDefaults(call.visibleOptionalDefaults),
			originalOptionalDefaults: cloneReplayOptionalDefaults(call.originalOptionalDefaults),
			outputItemIndex:          call.outputItemIndex,
		}
		visibleCalls[i] = responsesChatReplayProjectedCall{
			ID:        proxyIDs[i],
			Name:      call.name,
			Arguments: call.visibleArguments,
		}
		originalCalls[i] = responsesChatReplayProjectedCall{
			ID:        proxyIDs[i],
			Name:      call.name,
			Arguments: call.originalArguments,
		}
		resolvedCalls[i] = responsesChatReplayResolvedCall{
			ProxyCallID:     proxyIDs[i],
			UpstreamCallID:  call.upstreamCallID,
			Name:            call.name,
			OutputItemIndex: call.outputItemIndex,
			OutputItem:      cloneReplayRawMessage(prepared.outputItems[call.outputItemIndex]),
		}
	}

	group := &responsesChatReplayGroup{
		id:               groupID,
		route:            prepared.route,
		assistantContent: cloneReplayBytes(prepared.assistantContent),
		outputItems:      cloneReplayRawMessages(prepared.outputItems),
		calls:            storedCalls,
		createdAt:        now,
		expiresAt:        now.Add(s.ttl),
		byteSize:         prepared.byteSize,
	}
	group.lruElement = s.lru.PushBack(groupID)
	s.groups[groupID] = group
	for _, call := range storedCalls {
		s.callsByID[call.proxyCallID] = responsesChatReplayCallRef{groupID: groupID}
	}
	s.totalBytes += group.byteSize
	s.enforceLimitsLocked()

	return responsesChatReplayPublished{
		GroupID: groupID,
		Projection: responsesChatReplayAssistantProjection{
			Content: cloneReplayRawMessage(prepared.assistantContent),
			Calls:   visibleCalls,
		},
		OriginalProjection: responsesChatReplayAssistantProjection{
			Content: cloneReplayRawMessage(prepared.assistantContent),
			Calls:   originalCalls,
		},
		Calls:     resolvedCalls,
		CreatedAt: now,
		ByteSize:  prepared.byteSize,
	}, nil
}

func (s *responsesChatReplayStore) Resolve(route responsesChatReplayRoute, projection responsesChatReplayAssistantProjection) (responsesChatReplayResolution, error) {
	if s == nil {
		return responsesChatReplayResolution{}, errResponsesChatReplayClosed
	}
	if err := s.checkOpen(); err != nil {
		return responsesChatReplayResolution{}, err
	}
	canonicalContent, err := canonicalReplayJSONValue(projection.Content)
	if err != nil {
		return responsesChatReplayResolution{}, newResponsesChatReplayProjectionError("invalid assistant content")
	}
	if len(projection.Calls) == 0 {
		return responsesChatReplayResolution{}, newResponsesChatReplayProjectionError("missing assistant calls")
	}

	seen := make(map[string]struct{}, len(projection.Calls))
	for _, call := range projection.Calls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return responsesChatReplayResolution{}, newResponsesChatReplayProjectionError("invalid assistant call")
		}
		if _, exists := seen[call.ID]; exists {
			return responsesChatReplayResolution{}, newResponsesChatReplayProjectionError("duplicate assistant call")
		}
		seen[call.ID] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return responsesChatReplayResolution{}, errResponsesChatReplayClosed
	}
	s.expireLocked(s.now())

	var group *responsesChatReplayGroup
	for _, projectedCall := range projection.Calls {
		ref, ok := s.callsByID[projectedCall.ID]
		if !ok {
			return responsesChatReplayResolution{}, errResponsesChatReplayMissing
		}
		candidate := s.groups[ref.groupID]
		if candidate == nil || !candidate.route.equal(route) {
			return responsesChatReplayResolution{}, errResponsesChatReplayMissing
		}
		if group == nil {
			group = candidate
			continue
		}
		if group.id != candidate.id {
			return responsesChatReplayResolution{}, errResponsesChatReplayMixed
		}
	}
	if group == nil {
		return responsesChatReplayResolution{}, errResponsesChatReplayMissing
	}

	match := responsesChatReplayProjectionMatch(0)
	if group.matchesProjection(canonicalContent, projection.Calls, false) {
		match = responsesChatReplayProjectionVisible
	} else if group.matchesProjection(canonicalContent, projection.Calls, true) {
		match = responsesChatReplayProjectionOriginal
	} else {
		return responsesChatReplayResolution{}, newResponsesChatReplayProjectionError("assistant projection mismatch")
	}

	if group.lruElement != nil {
		s.lru.MoveToBack(group.lruElement)
	}
	return cloneResponsesChatReplayResolution(group, match), nil
}

func (s *responsesChatReplayStore) Stats() responsesChatReplayStoreStats {
	if s == nil {
		return responsesChatReplayStoreStats{Closed: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return responsesChatReplayStoreStats{Closed: true}
	}
	s.expireLocked(s.now())
	return responsesChatReplayStoreStats{
		Groups:     len(s.groups),
		Calls:      len(s.callsByID),
		TotalBytes: s.totalBytes,
	}
}

func (s *responsesChatReplayStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.groups = nil
	s.callsByID = nil
	s.lru = nil
	s.totalBytes = 0
	return nil
}

func (s *responsesChatReplayStore) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errResponsesChatReplayClosed
	}
	return nil
}

func (s *responsesChatReplayStore) preparePublish(request responsesChatReplayPublishRequest) (responsesChatReplayPreparedGroup, error) {
	if strings.TrimSpace(request.Route.ProviderID) == "" || strings.TrimSpace(request.Route.PublicModel) == "" {
		return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid route")
	}
	if len(request.OutputItems) == 0 {
		return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("missing output items")
	}
	if len(request.OutputItems) > s.maxItems {
		return responsesChatReplayPreparedGroup{}, &responsesChatReplayTooLargeError{
			Limit: responsesChatReplayLimitItems, Actual: len(request.OutputItems), Maximum: s.maxItems,
		}
	}
	if len(request.Calls) == 0 {
		return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("missing function calls")
	}
	if len(request.Calls) > s.maxCalls {
		return responsesChatReplayPreparedGroup{}, &responsesChatReplayTooLargeError{
			Limit: responsesChatReplayLimitCalls, Actual: len(request.Calls), Maximum: s.maxCalls,
		}
	}

	canonicalContent, err := canonicalReplayJSONValue(request.AssistantContent)
	if err != nil {
		return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid assistant content")
	}
	outputItems := make([]json.RawMessage, len(request.OutputItems))
	functionItemIndexes := make(map[int]struct{}, len(request.Calls))
	for i, item := range request.OutputItems {
		if !json.Valid(item) {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid output item")
		}
		outputItems[i] = cloneReplayRawMessage(item)
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &header) == nil && header.Type == "function_call" {
			functionItemIndexes[i] = struct{}{}
		}
	}

	calls := make([]responsesChatReplayPreparedCall, len(request.Calls))
	seenUpstreamIDs := make(map[string]struct{}, len(request.Calls))
	seenOutputIndexes := make(map[int]struct{}, len(request.Calls))
	lastOutputItemIndex := -1
	for i, call := range request.Calls {
		if strings.TrimSpace(call.UpstreamCallID) == "" || strings.TrimSpace(call.Name) == "" {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid function call")
		}
		if _, exists := seenUpstreamIDs[call.UpstreamCallID]; exists {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("duplicate upstream call ID")
		}
		seenUpstreamIDs[call.UpstreamCallID] = struct{}{}
		if call.OutputItemIndex < 0 || call.OutputItemIndex >= len(outputItems) {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid output item index")
		}
		if _, exists := seenOutputIndexes[call.OutputItemIndex]; exists {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("duplicate output item index")
		}
		if call.OutputItemIndex <= lastOutputItemIndex {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("function call order mismatch")
		}
		lastOutputItemIndex = call.OutputItemIndex
		seenOutputIndexes[call.OutputItemIndex] = struct{}{}

		originalArguments := call.VisibleArguments
		if call.OriginalArguments != nil {
			originalArguments = *call.OriginalArguments
		}
		var outputCall struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(outputItems[call.OutputItemIndex], &outputCall); err != nil ||
			outputCall.Type != "function_call" || outputCall.CallID != call.UpstreamCallID ||
			outputCall.Name != call.Name || outputCall.Arguments != originalArguments {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("function output item mismatch")
		}
		delete(functionItemIndexes, call.OutputItemIndex)
		canonicalVisibleArguments, err := canonicalReplayArguments(call.VisibleArguments)
		if err != nil {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid visible function arguments")
		}
		canonicalOriginalArguments, err := canonicalReplayArguments(originalArguments)
		if err != nil {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid original function arguments")
		}
		optionalDefaults, err := canonicalReplayOptionalDefaults(call.OptionalDefaults)
		if err != nil {
			return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("invalid optional function defaults")
		}
		calls[i] = responsesChatReplayPreparedCall{
			upstreamCallID:           call.UpstreamCallID,
			name:                     call.Name,
			visibleArguments:         call.VisibleArguments,
			originalArguments:        originalArguments,
			visibleHash:              sha256.Sum256(canonicalVisibleArguments),
			originalHash:             sha256.Sum256(canonicalOriginalArguments),
			visibleOptionalDefaults:  replayOptionalDefaultsAbsentFromArguments(call.VisibleArguments, optionalDefaults),
			originalOptionalDefaults: replayOptionalDefaultsAbsentFromArguments(originalArguments, optionalDefaults),
			outputItemIndex:          call.OutputItemIndex,
		}
	}
	if len(functionItemIndexes) != 0 {
		return responsesChatReplayPreparedGroup{}, newResponsesChatReplayProjectionError("unmapped function output item")
	}

	byteSize := replayGroupByteSize(request.Route, canonicalContent, outputItems, calls)
	if byteSize > s.maxGroupBytes {
		return responsesChatReplayPreparedGroup{}, &responsesChatReplayTooLargeError{
			Limit: responsesChatReplayLimitGroupBytes, Actual: byteSize, Maximum: s.maxGroupBytes,
		}
	}
	if byteSize > s.maxTotalBytes {
		return responsesChatReplayPreparedGroup{}, &responsesChatReplayTooLargeError{
			Limit: responsesChatReplayLimitTotalBytes, Actual: byteSize, Maximum: s.maxTotalBytes,
		}
	}
	return responsesChatReplayPreparedGroup{
		route:            request.Route,
		assistantContent: canonicalContent,
		outputItems:      outputItems,
		calls:            calls,
		byteSize:         byteSize,
	}, nil
}

func (s *responsesChatReplayStore) generateUniqueCallIDLocked(reserved map[string]struct{}) (string, error) {
	for attempt := 0; attempt < responsesChatReplayMaxIDAttempts; attempt++ {
		var randomBytes [responsesChatReplayRandomBytes]byte
		if _, err := io.ReadFull(s.random, randomBytes[:]); err != nil {
			return "", err
		}
		proxyID := responsesChatReplayCallIDPrefix + base64.RawURLEncoding.EncodeToString(randomBytes[:])
		if len(proxyID) != responsesChatReplayIDLength {
			return "", fmt.Errorf("unexpected replay ID length %d", len(proxyID))
		}
		if _, exists := s.callsByID[proxyID]; exists {
			continue
		}
		if _, exists := reserved[proxyID]; exists {
			continue
		}
		return proxyID, nil
	}
	return "", fmt.Errorf("replay call ID collision retry limit exceeded")
}

func (s *responsesChatReplayStore) nextGroupIDLocked() uint64 {
	for {
		s.nextGroupID++
		if s.nextGroupID == 0 {
			continue
		}
		if _, exists := s.groups[s.nextGroupID]; !exists {
			return s.nextGroupID
		}
	}
}

func (s *responsesChatReplayStore) expireLocked(now time.Time) {
	if s.lru == nil {
		return
	}
	for element := s.lru.Front(); element != nil; {
		next := element.Next()
		groupID, ok := element.Value.(uint64)
		if ok {
			group := s.groups[groupID]
			if group == nil || !now.Before(group.expiresAt) {
				s.removeGroupLocked(groupID)
			}
		}
		element = next
	}
}

func (s *responsesChatReplayStore) enforceLimitsLocked() {
	for len(s.groups) > s.maxGroups || s.totalBytes > s.maxTotalBytes {
		front := s.lru.Front()
		if front == nil {
			break
		}
		groupID, ok := front.Value.(uint64)
		if !ok {
			s.lru.Remove(front)
			continue
		}
		s.removeGroupLocked(groupID)
	}
}

func (s *responsesChatReplayStore) removeGroupLocked(groupID uint64) {
	group := s.groups[groupID]
	if group == nil {
		return
	}
	delete(s.groups, groupID)
	for _, call := range group.calls {
		if ref, ok := s.callsByID[call.proxyCallID]; ok && ref.groupID == groupID {
			delete(s.callsByID, call.proxyCallID)
		}
	}
	if group.lruElement != nil && s.lru != nil {
		s.lru.Remove(group.lruElement)
	}
	s.totalBytes -= group.byteSize
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
}

func (r responsesChatReplayRoute) equal(other responsesChatReplayRoute) bool {
	return r.ProviderID == other.ProviderID && r.PublicModel == other.PublicModel && r.UpstreamModel == other.UpstreamModel && r.RouteID == other.RouteID && r.PolicyTier == other.PolicyTier
}

func (g *responsesChatReplayGroup) matchesProjection(content []byte, projected []responsesChatReplayProjectedCall, original bool) bool {
	if !bytes.Equal(g.assistantContent, content) || len(g.calls) != len(projected) {
		return false
	}
	for i, stored := range g.calls {
		got := projected[i]
		if stored.proxyCallID != got.ID || stored.name != got.Name {
			return false
		}
		canonicalArguments, err := canonicalReplayArguments(got.Arguments)
		if err != nil {
			return false
		}
		hash := sha256.Sum256(canonicalArguments)
		matchesHash := func(want [sha256.Size]byte, defaults responsesChatReplayOptionalDefaults) bool {
			if hash == want {
				return true
			}
			normalized, changed, normalizeErr := canonicalReplayArgumentsWithoutOptionalDefaults(got.Arguments, defaults)
			return normalizeErr == nil && changed && sha256.Sum256(normalized) == want
		}
		if original {
			if !matchesHash(stored.originalHash, stored.originalOptionalDefaults) {
				return false
			}
		} else if !matchesHash(stored.visibleHash, stored.visibleOptionalDefaults) {
			return false
		}
	}
	return true
}

func cloneResponsesChatReplayResolution(group *responsesChatReplayGroup, match responsesChatReplayProjectionMatch) responsesChatReplayResolution {
	outputItems := cloneReplayRawMessages(group.outputItems)
	calls := make([]responsesChatReplayResolvedCall, len(group.calls))
	for i, call := range group.calls {
		calls[i] = responsesChatReplayResolvedCall{
			ProxyCallID:     call.proxyCallID,
			UpstreamCallID:  call.upstreamCallID,
			Name:            call.name,
			OutputItemIndex: call.outputItemIndex,
			OutputItem:      cloneReplayRawMessage(group.outputItems[call.outputItemIndex]),
		}
	}
	return responsesChatReplayResolution{
		GroupID:         group.id,
		Route:           group.route,
		ProjectionMatch: match,
		OutputItems:     outputItems,
		Calls:           calls,
		CreatedAt:       group.createdAt,
		ByteSize:        group.byteSize,
	}
}

func replayGroupByteSize(route responsesChatReplayRoute, content []byte, outputItems []json.RawMessage, calls []responsesChatReplayPreparedCall) int {
	size := len(route.ProviderID) + len(route.PublicModel) + len(route.UpstreamModel) + len(route.RouteID) + len(route.PolicyTier) + len(content)
	for _, item := range outputItems {
		size += len(item)
	}
	for _, call := range calls {
		size += responsesChatReplayIDLength
		size += len(call.upstreamCallID) + len(call.name)
		size += sha256.Size * 2
		size += replayOptionalDefaultsByteSize(call.visibleOptionalDefaults)
		size += replayOptionalDefaultsByteSize(call.originalOptionalDefaults)
		size += 8 // output item index accounting
	}
	return size
}

func canonicalReplayArguments(arguments string) ([]byte, error) {
	if !json.Valid([]byte(arguments)) {
		return []byte(arguments), nil
	}
	return canonicalReplayJSONValue(json.RawMessage(arguments))
}

func canonicalReplayOptionalDefaults(defaults responsesChatReplayOptionalDefaults) (responsesChatReplayOptionalDefaults, error) {
	if len(defaults) == 0 {
		return nil, nil
	}
	canonical := make(responsesChatReplayOptionalDefaults, len(defaults))
	for name, value := range defaults {
		encoded, err := canonicalReplayJSONValue(value)
		if err != nil {
			return nil, err
		}
		canonical[name] = cloneReplayRawMessage(encoded)
	}
	return canonical, nil
}

func cloneReplayOptionalDefaults(defaults responsesChatReplayOptionalDefaults) responsesChatReplayOptionalDefaults {
	if len(defaults) == 0 {
		return nil
	}
	cloned := make(responsesChatReplayOptionalDefaults, len(defaults))
	for name, value := range defaults {
		cloned[name] = cloneReplayRawMessage(value)
	}
	return cloned
}

func replayOptionalDefaultsAbsentFromArguments(arguments string, defaults responsesChatReplayOptionalDefaults) responsesChatReplayOptionalDefaults {
	if len(defaults) == 0 || !json.Valid([]byte(arguments)) {
		return nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &values) != nil || values == nil {
		return nil
	}
	absent := make(responsesChatReplayOptionalDefaults)
	for name, value := range defaults {
		if _, exists := values[name]; exists {
			continue
		}
		absent[name] = cloneReplayRawMessage(value)
	}
	if len(absent) == 0 {
		return nil
	}
	return absent
}

func replayOptionalDefaultsByteSize(defaults responsesChatReplayOptionalDefaults) int {
	size := 0
	for name, value := range defaults {
		size += len(name) + len(value)
	}
	return size
}

func canonicalReplayArgumentsWithoutOptionalDefaults(arguments string, defaults responsesChatReplayOptionalDefaults) ([]byte, bool, error) {
	canonical, err := canonicalReplayArguments(arguments)
	if err != nil || len(defaults) == 0 || !json.Valid([]byte(arguments)) {
		return canonical, false, err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &values); err != nil || values == nil {
		return canonical, false, err
	}
	changed := false
	for name, defaultValue := range defaults {
		value, ok := values[name]
		if !ok {
			continue
		}
		canonicalValue, valueErr := canonicalReplayJSONValue(value)
		if valueErr == nil && bytes.Equal(canonicalValue, defaultValue) {
			delete(values, name)
			changed = true
		}
	}
	if !changed {
		return canonical, false, nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, false, err
	}
	normalized, err := canonicalReplayJSONValue(encoded)
	return normalized, true, err
}

func canonicalReplayJSONValue(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}

func cloneReplayRawMessages(messages []json.RawMessage) []json.RawMessage {
	if messages == nil {
		return nil
	}
	cloned := make([]json.RawMessage, len(messages))
	for i, message := range messages {
		cloned[i] = cloneReplayRawMessage(message)
	}
	return cloned
}

func cloneReplayRawMessage(message json.RawMessage) json.RawMessage {
	return json.RawMessage(cloneReplayBytes(message))
}

func cloneReplayBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return bytes.Clone(value)
}

func newResponsesChatReplayProjectionError(reason string) *responsesChatReplayProjectionError {
	return &responsesChatReplayProjectionError{Reason: reason}
}
