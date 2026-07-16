package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/models"
)

const (
	responsesChatPrecommitTimeout  = responsesPrecommitPeekTimeout
	responsesChatPrecommitMaxBytes = responsesPrecommitMaxPeekBytes
	responsesChatMaxSSEEventBytes  = openAIStreamScannerMaxBuffer
	responsesChatReadChunkSize     = responsesPeekReadChunkSize
)

type responsesChatStreamConfig struct {
	PublicModel string
	ReplayStore *responsesChatReplayStore
	ReplayRoute responsesChatReplayRoute

	PrecommitTimeout  time.Duration
	PrecommitMaxBytes int
	MaxEventBytes     int
	Now               func() time.Time
}

type responsesChatStreamReady struct {
	err error
}

type responsesChatStreamRead struct {
	data []byte
	err  error
}

type responsesChatStreamControl struct {
	body io.ReadCloser

	commitCh chan struct{}
	abortCh  chan struct{}
	doneCh   chan struct{}

	commitOnce sync.Once
	abortOnce  sync.Once
	closeOnce  sync.Once
}

func (c *responsesChatStreamControl) commit() {
	c.commitOnce.Do(func() { close(c.commitCh) })
}

func (c *responsesChatStreamControl) abort() {
	c.abortOnce.Do(func() { close(c.abortCh) })
	c.closeBody()
}

func (c *responsesChatStreamControl) closeBody() {
	c.closeOnce.Do(func() {
		if c.body != nil {
			_ = c.body.Close()
		}
	})
}

func translateResponsesSSEToChat(ctx context.Context, body io.ReadCloser, options responsesChatResponseOptions) (*chatStreamEventStream, error) {
	return prepareResponsesChatStream(ctx, body, responsesChatStreamConfig{
		PublicModel: options.PublicModel,
		ReplayStore: options.ReplayStore,
		ReplayRoute: options.ReplayRoute,
	})
}

func prepareResponsesChatStream(ctx context.Context, body io.ReadCloser, config responsesChatStreamConfig) (*chatStreamEventStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if body == nil {
		return nil, newChatServerError("invalid_responses_stream", "upstream Responses stream body is missing")
	}
	if config.PrecommitTimeout <= 0 {
		config.PrecommitTimeout = responsesChatPrecommitTimeout
	}
	if config.PrecommitMaxBytes <= 0 {
		config.PrecommitMaxBytes = responsesChatPrecommitMaxBytes
	}
	if config.MaxEventBytes <= 0 {
		config.MaxEventBytes = responsesChatMaxSSEEventBytes
	}
	if config.Now == nil {
		config.Now = time.Now
	}

	writer, stream := newChatStreamEventPipe(ctx)
	readyCh := make(chan responsesChatStreamReady, 1)
	readCh := make(chan responsesChatStreamRead, 1)
	control := &responsesChatStreamControl{
		body:     body,
		commitCh: make(chan struct{}),
		abortCh:  make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	go readResponsesChatStream(writer.ctx, control, readCh)
	go runResponsesChatStream(writer, control, readCh, readyCh, config)

	select {
	case ready := <-readyCh:
		if ready.err != nil {
			stream.stop(ready.err)
			control.abort()
			<-control.doneCh
			return nil, ready.err
		}
		control.commit()
		return stream, nil
	case <-ctx.Done():
		stream.stop(ctx.Err())
		control.abort()
		<-control.doneCh
		return nil, ctx.Err()
	}
}

func readResponsesChatStream(ctx context.Context, control *responsesChatStreamControl, readCh chan<- responsesChatStreamRead) {
	defer close(readCh)
	buf := make([]byte, responsesChatReadChunkSize)
	for {
		n, err := control.body.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case readCh <- responsesChatStreamRead{data: chunk}:
			case <-control.abortCh:
				return
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			if context.Cause(ctx) != nil {
				return
			}
			select {
			case <-control.abortCh:
				return
			case <-ctx.Done():
				return
			default:
			}
			select {
			case <-control.abortCh:
			case <-ctx.Done():
			case readCh <- responsesChatStreamRead{err: err}:
			}
			return
		}
	}
}

type responsesChatSSEDecoder struct {
	pending    []byte
	allowBOM   bool
	maxBytes   int
	scanOffset int
}

func newResponsesChatSSEDecoder(maxBytes int) responsesChatSSEDecoder {
	return responsesChatSSEDecoder{allowBOM: true, maxBytes: maxBytes}
}

func (d *responsesChatSSEDecoder) push(data []byte, onMessage func(responsesSSEMessage) error) error {
	d.pending = append(d.pending, data...)
	return d.consume(false, onMessage)
}

func (d *responsesChatSSEDecoder) finalize(onMessage func(responsesSSEMessage) error) error {
	if err := d.consume(true, onMessage); err != nil {
		return err
	}
	if len(d.pending) == 0 {
		return nil
	}

	// A final line ending terminates the last SSE field line at EOF even when
	// the optional blank-line separator is absent. Decode that complete final
	// event, but still reject an unterminated last line as truncated.
	last := d.pending[len(d.pending)-1]
	if last != '\n' && last != '\r' {
		return newChatServerError("invalid_responses_stream", "upstream returned truncated Responses SSE")
	}
	frame := normalizeResponsesSSEFrame(d.pending)
	frame = append(frame, '\n')
	msg, consumed, incomplete := nextResponsesSSEMessage(frame, d.allowBOM)
	if incomplete || consumed <= 0 || consumed != len(frame) {
		return newChatServerError("invalid_responses_stream", "upstream returned malformed Responses SSE")
	}
	d.pending = nil
	d.scanOffset = 0
	d.allowBOM = false
	if msg.semantic {
		return onMessage(msg)
	}
	return nil
}

func (d *responsesChatSSEDecoder) consume(final bool, onMessage func(responsesSSEMessage) error) error {
	for {
		boundaryEnd := responsesSSEBoundaryEndFrom(d.pending, d.scanOffset, final)
		if boundaryEnd == 0 {
			if len(d.pending) > d.maxBytes {
				return newChatServerError("responses_sse_event_too_large", "upstream Responses SSE event exceeds the supported limit")
			}
			if len(d.pending) > 3 {
				d.scanOffset = len(d.pending) - 3
			} else {
				d.scanOffset = 0
			}
			return nil
		}
		if boundaryEnd > d.maxBytes {
			return newChatServerError("responses_sse_event_too_large", "upstream Responses SSE event exceeds the supported limit")
		}
		frame := normalizeResponsesSSEFrame(d.pending[:boundaryEnd])
		msg, consumed, incomplete := nextResponsesSSEMessage(frame, d.allowBOM)
		if incomplete || consumed <= 0 || consumed != len(frame) {
			return newChatServerError("invalid_responses_stream", "upstream returned malformed Responses SSE")
		}
		d.allowBOM = false
		remaining := len(d.pending) - boundaryEnd
		copy(d.pending, d.pending[boundaryEnd:])
		d.pending = d.pending[:remaining]
		d.scanOffset = 0
		if msg.semantic {
			if err := onMessage(msg); err != nil {
				return err
			}
		}
	}
}

func (d *responsesChatSSEDecoder) hasPending() bool {
	return len(d.pending) != 0
}

func responsesSSEBoundaryEndFrom(buf []byte, start int, final bool) int {
	if start < 0 {
		start = 0
	}
	if start > len(buf) {
		start = len(buf)
	}
	for i := start; i < len(buf); {
		first := responsesSSELineEndingLen(buf, i, final)
		if first < 0 {
			return 0
		}
		if first == 0 {
			i++
			continue
		}
		next := i + first
		second := responsesSSELineEndingLen(buf, next, final)
		if second < 0 {
			return 0
		}
		if second > 0 {
			return next + second
		}
		i = next
	}
	return 0
}

func responsesSSELineEndingLen(buf []byte, index int, final bool) int {
	if index < 0 || index >= len(buf) {
		return 0
	}
	switch buf[index] {
	case '\n':
		return 1
	case '\r':
		if index+1 < len(buf) && buf[index+1] == '\n' {
			return 2
		}
		if index+1 == len(buf) && !final {
			return -1
		}
		return 1
	default:
		return 0
	}
}

func normalizeResponsesSSEFrame(frame []byte) []byte {
	if !bytes.ContainsRune(frame, '\r') {
		return frame
	}
	normalized := make([]byte, 0, len(frame))
	for i := 0; i < len(frame); i++ {
		if frame[i] != '\r' {
			normalized = append(normalized, frame[i])
			continue
		}
		if i+1 < len(frame) && frame[i+1] == '\n' {
			i++
		}
		normalized = append(normalized, '\n')
	}
	return normalized
}

type responsesChatTextPart struct {
	outputIndex  int
	contentIndex int
	kind         string
	digest       hash.Hash
	bytes        int
	valueDone    bool
	partDone     bool
}

type responsesChatMessageState struct {
	outputIndex int
	done        bool
	parts       map[int]*responsesChatTextPart
}

type responsesChatToolState struct {
	outputIndex   int
	denseIndex    int
	upstreamCall  string
	name          string
	arguments     strings.Builder
	argumentsDone bool
	completed     bool
	done          bool
}

type responsesChatStreamState struct {
	config responsesChatStreamConfig

	chatID  string
	created int64
	model   string
	locked  bool

	hasSequence         bool
	sequence            int64
	createdSeen         bool
	progressSeen        bool
	terminalSeen        bool
	messagesByIndex     map[int]*responsesChatMessageState
	itemsByIndex        map[int]string
	doneByIndex         map[int]bool
	tools               []*responsesChatToolState
	toolsByIndex        map[int]*responsesChatToolState
	replayBytes         int
	hasIncompleteTool   bool
	contentParts        int
	visibleBytes        int
	visibleBytesCharged bool
}

func newResponsesChatStreamState(config responsesChatStreamConfig) *responsesChatStreamState {
	return &responsesChatStreamState{
		config:          config,
		chatID:          responsesChatCompletionID(""),
		created:         config.Now().Unix(),
		model:           strings.TrimSpace(config.PublicModel),
		messagesByIndex: make(map[int]*responsesChatMessageState),
		itemsByIndex:    make(map[int]string),
		doneByIndex:     make(map[int]bool),
		toolsByIndex:    make(map[int]*responsesChatToolState),
	}
}

func (s *responsesChatStreamState) roleChunk() models.OpenAIStreamChunk {
	s.locked = true
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{{
		Index: 0,
		Delta: models.OpenAIMessage{Role: "assistant"},
	}}
	return chunk
}

func (s *responsesChatStreamState) baseChunk() models.OpenAIStreamChunk {
	return models.OpenAIStreamChunk{
		ID:      s.chatID,
		Object:  openAIChatCompletionChunkObject,
		Created: s.created,
		Model:   s.model,
	}
}

func (s *responsesChatStreamState) textChunk(text string) models.OpenAIStreamChunk {
	content, _ := json.Marshal(text)
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Content: content}}}
	return chunk
}

func (s *responsesChatStreamState) refusalChunk(refusal string) models.OpenAIStreamChunk {
	value, _ := json.Marshal(refusal)
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Refusal: value}}}
	return chunk
}

func (s *responsesChatStreamState) finishChunk(reason string) models.OpenAIStreamChunk {
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{{Index: 0, FinishReason: &reason}}
	return chunk
}

func (s *responsesChatStreamState) usageChunk(usage *models.OpenAIUsage) models.OpenAIStreamChunk {
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{}
	chunk.Usage = usage
	return chunk
}

func (s *responsesChatStreamState) toolStartChunk(tool *responsesChatToolState, proxyID string) models.OpenAIStreamChunk {
	index := tool.denseIndex
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
		ID:    proxyID,
		Type:  "function",
		Index: &index,
		Function: models.OpenAIFunctionCall{
			Name: tool.name,
		},
	}}}}}
	return chunk
}

func (s *responsesChatStreamState) toolArgumentsChunk(tool *responsesChatToolState) models.OpenAIStreamChunk {
	index := tool.denseIndex
	chunk := s.baseChunk()
	chunk.Choices = []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{
		Index: &index,
		Function: models.OpenAIFunctionCall{
			Arguments: tool.arguments.String(),
		},
	}}}}}
	return chunk
}

type responsesChatStreamTransition struct {
	chunks   []models.OpenAIStreamChunk
	terminal bool
}

func (s *responsesChatStreamState) handleMessage(msg responsesSSEMessage) (responsesChatStreamTransition, error) {
	if s.terminalSeen {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "upstream Responses stream continued after a terminal event")
	}
	if strings.TrimSpace(msg.data) == "[DONE]" {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "upstream Responses stream ended without a terminal response event")
	}
	var header struct {
		Type           string `json:"type"`
		SequenceNumber int64  `json:"sequence_number"`
	}
	if err := json.Unmarshal([]byte(msg.data), &header); err != nil || strings.TrimSpace(header.Type) == "" {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "upstream returned malformed Responses stream JSON")
	}
	eventType := strings.TrimSpace(header.Type)
	if named := strings.TrimSpace(msg.event); named != "" && named != eventType {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "Responses SSE event name does not match its JSON type")
	}
	if s.hasSequence && header.SequenceNumber != s.sequence+1 {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "Responses stream sequence is not contiguous")
	}
	s.hasSequence = true
	s.sequence = header.SequenceNumber

	switch eventType {
	case "response.created":
		return s.handleCreated([]byte(msg.data))
	case "response.in_progress":
		if !s.createdSeen || s.progressSeen {
			return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "invalid response.in_progress transition")
		}
		s.progressSeen = true
		return responsesChatStreamTransition{}, nil
	case "response.output_item.added":
		return s.handleOutputItemAdded([]byte(msg.data))
	case "response.content_part.added":
		return s.handleContentPartAdded([]byte(msg.data))
	case "response.output_text.delta":
		return s.handleOutputTextDelta([]byte(msg.data))
	case "response.output_text.done":
		return s.handleOutputTextDone([]byte(msg.data))
	case "response.refusal.delta":
		return s.handleRefusalDelta([]byte(msg.data))
	case "response.refusal.done":
		return s.handleRefusalDone([]byte(msg.data))
	case "response.function_call_arguments.delta":
		return s.handleFunctionArgumentsDelta([]byte(msg.data))
	case "response.function_call_arguments.done":
		return s.handleFunctionArgumentsDone([]byte(msg.data))
	case "response.content_part.done":
		return s.handleContentPartDone([]byte(msg.data))
	case "response.output_item.done":
		return s.handleOutputItemDone([]byte(msg.data))
	case "response.completed":
		return s.handleCompleted([]byte(msg.data))
	case "response.incomplete":
		return s.handleIncomplete([]byte(msg.data))
	case "response.failed":
		return s.handleFailed([]byte(msg.data))
	case "error":
		return responsesChatStreamTransition{}, parseResponsesChatTopLevelError([]byte(msg.data))
	case "response.reasoning_summary_part.added",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_text.delta",
		"response.reasoning_text.done":
		// Hidden reasoning progress is non-semantic for Chat output. The exact
		// authoritative reasoning item is retained from output_item.done/terminal output.
		return responsesChatStreamTransition{}, nil
	default:
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_event", fmt.Sprintf("upstream Responses event %q is not supported", eventType))
	}
}

func (s *responsesChatStreamState) handleCreated(data []byte) (responsesChatStreamTransition, error) {
	if s.createdSeen {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "duplicate response.created event")
	}
	var event struct {
		Response struct {
			ID        string `json:"id"`
			CreatedAt int64  `json:"created_at"`
			Status    string `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil || strings.TrimSpace(event.Response.ID) == "" || event.Response.Status != "in_progress" {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "response.created is malformed")
	}
	s.createdSeen = true
	if !s.locked {
		s.chatID = responsesChatCompletionID(event.Response.ID)
		if event.Response.CreatedAt != 0 {
			s.created = event.Response.CreatedAt
		}
	}
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleOutputItemAdded(data []byte) (responsesChatStreamTransition, error) {
	if !s.createdSeen {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "invalid response.output_item.added transition")
	}
	var event struct {
		OutputIndex *int `json:"output_index"`
		Item        struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Role      string `json:"role"`
			Status    string `json:"status"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil || *event.OutputIndex < 0 || strings.TrimSpace(event.Item.ID) == "" {
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "Responses output item is malformed")
	}
	if len(s.itemsByIndex) >= responsesChatReplayMaxItems {
		return responsesChatStreamTransition{}, mapResponsesChatReplayPublishError(&responsesChatReplayTooLargeError{Limit: responsesChatReplayLimitItems, Actual: len(s.itemsByIndex) + 1, Maximum: responsesChatReplayMaxItems})
	}
	if err := s.chargeReplayBytes(64); err != nil {
		return responsesChatStreamTransition{}, err
	}
	if _, exists := s.itemsByIndex[*event.OutputIndex]; exists {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "duplicate Responses output index")
	}
	s.itemsByIndex[*event.OutputIndex] = event.Item.Type
	switch event.Item.Type {
	case "message":
		if event.Item.Role != "assistant" {
			return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "assistant message output item is malformed")
		}
		s.messagesByIndex[*event.OutputIndex] = &responsesChatMessageState{outputIndex: *event.OutputIndex, parts: make(map[int]*responsesChatTextPart)}
	case "function_call":
		if len(s.tools) >= responsesChatReplayMaxCalls {
			return responsesChatStreamTransition{}, mapResponsesChatReplayPublishError(&responsesChatReplayTooLargeError{Limit: responsesChatReplayLimitCalls, Actual: len(s.tools) + 1, Maximum: responsesChatReplayMaxCalls})
		}
		if strings.TrimSpace(event.Item.CallID) == "" || strings.TrimSpace(event.Item.Name) == "" {
			return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "function-call output item is malformed")
		}
		if !s.visibleBytesCharged {
			if err := s.chargeReplayBytes(s.visibleBytes); err != nil {
				return responsesChatStreamTransition{}, err
			}
			s.visibleBytesCharged = true
		}
		if err := s.chargeReplayBytes(len(event.Item.CallID) + len(event.Item.Name) + len(event.Item.Arguments) + 256); err != nil {
			return responsesChatStreamTransition{}, err
		}
		tool := &responsesChatToolState{
			outputIndex:  *event.OutputIndex,
			denseIndex:   len(s.tools),
			upstreamCall: event.Item.CallID,
			name:         event.Item.Name,
		}
		tool.arguments.WriteString(event.Item.Arguments)
		s.tools = append(s.tools, tool)
		s.toolsByIndex[tool.outputIndex] = tool
	case "reasoning":
		// Hidden from Chat and retained only from authoritative terminal output.
	default:
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", fmt.Sprintf("Responses output item type %q is not supported", event.Item.Type))
	}
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleContentPartAdded(data []byte) (responsesChatStreamTransition, error) {
	var event struct {
		ItemID       string `json:"item_id"`
		OutputIndex  *int   `json:"output_index"`
		ContentIndex *int   `json:"content_index"`
		Part         struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"part"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil || event.ContentIndex == nil || *event.ContentIndex < 0 || (event.Part.Type != "output_text" && event.Part.Type != "refusal") || event.Part.Text != "" || event.Part.Refusal != "" {
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "assistant content part is malformed")
	}
	message := s.messagesByIndex[*event.OutputIndex]
	if message == nil || message.done {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "invalid response.content_part.added transition")
	}
	if s.contentParts >= responsesChatReplayMaxItems {
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "Responses stream contains too many content parts")
	}
	if err := s.chargeReplayBytes(128 + len(event.Part.Type)); err != nil {
		return responsesChatStreamTransition{}, err
	}
	s.contentParts++
	if _, duplicate := message.parts[*event.ContentIndex]; duplicate {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "duplicate assistant content index")
	}
	message.parts[*event.ContentIndex] = &responsesChatTextPart{
		outputIndex:  *event.OutputIndex,
		contentIndex: *event.ContentIndex,
		kind:         event.Part.Type,
		digest:       sha256.New(),
	}
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleOutputTextDelta(data []byte) (responsesChatStreamTransition, error) {
	return s.handleVisibleTextDelta(data, "output_text")
}

func (s *responsesChatStreamState) handleRefusalDelta(data []byte) (responsesChatStreamTransition, error) {
	return s.handleVisibleTextDelta(data, "refusal")
}

func (s *responsesChatStreamState) handleVisibleTextDelta(data []byte, kind string) (responsesChatStreamTransition, error) {
	var event struct {
		ItemID       string `json:"item_id"`
		OutputIndex  *int   `json:"output_index"`
		ContentIndex *int   `json:"content_index"`
		Delta        string `json:"delta"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil || event.ContentIndex == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "output text delta correlation is invalid")
	}
	message := s.messagesByIndex[*event.OutputIndex]
	if message == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "output text delta correlation is invalid")
	}
	part := message.parts[*event.ContentIndex]
	if part == nil || part.kind != kind || part.valueDone || part.partDone || !part.matches(event.ItemID, event.OutputIndex, event.ContentIndex) {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "output text delta correlation is invalid")
	}
	if s.visibleBytesCharged {
		if err := s.chargeReplayBytes(len(event.Delta)); err != nil {
			return responsesChatStreamTransition{}, err
		}
	} else if s.visibleBytes <= responsesChatReplayMaxGroupBytes {
		remaining := responsesChatReplayMaxGroupBytes + 1 - s.visibleBytes
		if len(event.Delta) < remaining {
			s.visibleBytes += len(event.Delta)
		} else {
			s.visibleBytes = responsesChatReplayMaxGroupBytes + 1
		}
	}
	_, _ = part.digest.Write([]byte(event.Delta))
	part.bytes += len(event.Delta)
	if event.Delta == "" {
		return responsesChatStreamTransition{}, nil
	}
	chunk := s.textChunk(event.Delta)
	if kind == "refusal" {
		chunk = s.refusalChunk(event.Delta)
	}
	return responsesChatStreamTransition{chunks: []models.OpenAIStreamChunk{chunk}}, nil
}

func (s *responsesChatStreamState) handleOutputTextDone(data []byte) (responsesChatStreamTransition, error) {
	return s.handleVisibleTextDone(data, "output_text")
}

func (s *responsesChatStreamState) handleRefusalDone(data []byte) (responsesChatStreamTransition, error) {
	return s.handleVisibleTextDone(data, "refusal")
}

func (s *responsesChatStreamState) handleVisibleTextDone(data []byte, kind string) (responsesChatStreamTransition, error) {
	var event struct {
		ItemID       string `json:"item_id"`
		OutputIndex  *int   `json:"output_index"`
		ContentIndex *int   `json:"content_index"`
		Text         string `json:"text"`
		Refusal      string `json:"refusal"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil || event.ContentIndex == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "visible text completion is malformed")
	}
	message := s.messagesByIndex[*event.OutputIndex]
	if message == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "output text completion is malformed")
	}
	part := message.parts[*event.ContentIndex]
	if part == nil || part.kind != kind || part.valueDone || part.partDone {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "invalid response.output_text.done transition")
	}
	value := event.Text
	if kind == "refusal" {
		value = event.Refusal
	}
	if !part.matches(event.ItemID, event.OutputIndex, event.ContentIndex) || !part.matchesValue(value) {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "output text completion does not match streamed deltas")
	}
	part.valueDone = true
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleContentPartDone(data []byte) (responsesChatStreamTransition, error) {
	var event struct {
		ItemID       string `json:"item_id"`
		OutputIndex  *int   `json:"output_index"`
		ContentIndex *int   `json:"content_index"`
		Part         struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"part"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil || event.ContentIndex == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "completed content part is malformed")
	}
	message := s.messagesByIndex[*event.OutputIndex]
	if message == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "completed content part is malformed")
	}
	part := message.parts[*event.ContentIndex]
	if part == nil || !part.valueDone || part.partDone {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "invalid response.content_part.done transition")
	}
	value := event.Part.Text
	if part.kind == "refusal" {
		value = event.Part.Refusal
	}
	if !part.matches(event.ItemID, event.OutputIndex, event.ContentIndex) || event.Part.Type != part.kind || !part.matchesValue(value) {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "completed content part does not match streamed text")
	}
	part.partDone = true
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleFunctionArgumentsDelta(data []byte) (responsesChatStreamTransition, error) {
	var event struct {
		ItemID      string `json:"item_id"`
		OutputIndex *int   `json:"output_index"`
		Delta       string `json:"delta"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "function argument delta is malformed")
	}
	tool := s.toolsByIndex[*event.OutputIndex]
	if tool == nil || tool.outputIndex != *event.OutputIndex || tool.argumentsDone || tool.done {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "function argument delta correlation is invalid")
	}
	if err := s.chargeReplayBytes(len(event.Delta)); err != nil {
		return responsesChatStreamTransition{}, err
	}
	tool.arguments.WriteString(event.Delta)
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleFunctionArgumentsDone(data []byte) (responsesChatStreamTransition, error) {
	var event struct {
		ItemID      string `json:"item_id"`
		OutputIndex *int   `json:"output_index"`
		Arguments   string `json:"arguments"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "function argument completion is malformed")
	}
	tool := s.toolsByIndex[*event.OutputIndex]
	if tool == nil || tool.outputIndex != *event.OutputIndex || tool.argumentsDone || tool.done {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "function argument completion correlation is invalid")
	}
	accumulated := tool.arguments.String()
	switch {
	case event.Arguments == accumulated:
	case strings.HasPrefix(event.Arguments, accumulated):
		suffix := event.Arguments[len(accumulated):]
		if err := s.chargeReplayBytes(len(suffix)); err != nil {
			return responsesChatStreamTransition{}, err
		}
		tool.arguments.WriteString(suffix)
	default:
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "completed function arguments diverge from streamed deltas")
	}
	tool.argumentsDone = true
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) chargeReplayBytes(additional int) error {
	if additional < 0 || additional > responsesChatReplayMaxGroupBytes-s.replayBytes {
		actual := s.replayBytes + additional
		if additional < 0 {
			actual = responsesChatReplayMaxGroupBytes + 1
		}
		return mapResponsesChatReplayPublishError(&responsesChatReplayTooLargeError{Limit: responsesChatReplayLimitGroupBytes, Actual: actual, Maximum: responsesChatReplayMaxGroupBytes})
	}
	s.replayBytes += additional
	return nil
}

func (s *responsesChatStreamState) handleOutputItemDone(data []byte) (responsesChatStreamTransition, error) {
	var event struct {
		OutputIndex *int            `json:"output_index"`
		Item        json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.OutputIndex == nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "completed output item is malformed")
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(event.Item, &header); err != nil || header.ID == "" || s.itemsByIndex[*event.OutputIndex] != header.Type || s.doneByIndex[*event.OutputIndex] {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "completed output item correlation is invalid")
	}
	switch header.Type {
	case "message":
		message := s.messagesByIndex[*event.OutputIndex]
		if message == nil || message.done {
			return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "assistant message completed before its content")
		}
		for _, part := range message.parts {
			if !part.partDone {
				return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "assistant message completed before its content")
			}
		}
		if err := validateResponsesChatTerminalMessage(event.Item, message); err != nil {
			return responsesChatStreamTransition{}, err
		}
		message.done = true
	case "function_call":
		tool := s.toolsByIndex[*event.OutputIndex]
		if tool == nil || tool.done {
			return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "function call completed before its arguments")
		}
		var call struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal(event.Item, &call); err != nil || call.CallID != tool.upstreamCall || call.Name != tool.name {
			return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "completed function call does not match streamed arguments")
		}
		if strings.TrimSpace(call.Status) == "completed" {
			if !tool.argumentsDone || call.Arguments != tool.arguments.String() {
				return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "completed function call does not match streamed arguments")
			}
			tool.completed = true
		} else {
			s.hasIncompleteTool = true
		}
		tool.done = true
	case "reasoning":
		// Hidden, but completion is still required before the terminal output.
	default:
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", fmt.Sprintf("Responses output item type %q is not supported", header.Type))
	}
	s.doneByIndex[*event.OutputIndex] = true
	return responsesChatStreamTransition{}, nil
}

func (s *responsesChatStreamState) handleCompleted(data []byte) (responsesChatStreamTransition, error) {
	return s.handleTerminal(data, "completed")
}

func (s *responsesChatStreamState) handleIncomplete(data []byte) (responsesChatStreamTransition, error) {
	return s.handleTerminal(data, "incomplete")
}

func (s *responsesChatStreamState) handleTerminal(data []byte, terminalStatus string) (transition responsesChatStreamTransition, err error) {
	if !s.createdSeen {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "response.completed arrived before response.created")
	}
	var event struct {
		Response responsesChatJSONEnvelope `json:"response"`
	}
	if unmarshalErr := json.Unmarshal(data, &event); unmarshalErr != nil {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "terminal Responses event is malformed")
	}
	var terminalUsage *models.OpenAIUsage
	usageFailureTransition := responsesChatStreamTransition{}
	if event.Response.Usage != nil {
		terminalUsage = event.Response.Usage.toOpenAIUsage()
		usageFailureTransition.chunks = []models.OpenAIStreamChunk{s.usageChunk(terminalUsage)}
	}
	defer func() {
		if err == nil {
			return
		}
		attachChatExecutionErrorUsage(err, terminalUsage)
		transition = usageFailureTransition
	}()
	if event.Response.Status != terminalStatus || event.Response.Error != nil || len(event.Response.Output) != len(s.itemsByIndex) {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "terminal Responses event is malformed")
	}
	for outputIndex := range s.itemsByIndex {
		if !s.doneByIndex[outputIndex] {
			return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "response.completed arrived before output completion")
		}
	}

	var assistantText strings.Builder
	hasRefusal := false
	// Chat-style surfaces intentionally do not run command_rewrite; they preserve
	// upstream arguments and only capture tool context for later output reduction.
	publishCalls := make([]responsesChatReplayPublishCall, 0, len(s.tools))
	for outputIndex, raw := range event.Response.Output {
		var header struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || s.itemsByIndex[outputIndex] != header.Type {
			return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "terminal output does not match streamed item order")
		}
		switch header.Type {
		case "message":
			message := s.messagesByIndex[outputIndex]
			if message == nil {
				return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "terminal assistant message was not streamed")
			}
			if err := validateResponsesChatTerminalMessage(raw, message); err != nil {
				return responsesChatStreamTransition{}, err
			}
			text, refusal, err := responsesChatMessageContent(raw)
			if err != nil {
				return responsesChatStreamTransition{}, err
			}
			assistantText.WriteString(text)
			hasRefusal = hasRefusal || refusal != ""
		case "function_call":
			tool := s.toolsByIndex[outputIndex]
			if tool == nil {
				return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "terminal function call was not streamed")
			}
			var call struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Status    string `json:"status"`
			}
			if err := json.Unmarshal(raw, &call); err != nil || call.CallID != tool.upstreamCall || call.Name != tool.name {
				return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "terminal function call does not match streamed arguments")
			}
			if strings.TrimSpace(call.Status) != "completed" {
				if terminalStatus != "incomplete" {
					return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "non-completed function call appeared in a completed Responses stream")
				}
				s.hasIncompleteTool = true
				continue
			}
			if !tool.completed || call.Arguments != tool.arguments.String() {
				return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "terminal function call does not match streamed arguments")
			}
			publishCalls = append(publishCalls, responsesChatReplayPublishCall{
				UpstreamCallID: call.CallID, Name: call.Name, VisibleArguments: call.Arguments, OutputItemIndex: outputIndex,
			})
		case "reasoning":
		default:
			return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", fmt.Sprintf("Responses output item type %q is not supported", header.Type))
		}
	}

	assistantContent, _ := json.Marshal(assistantText.String())
	if len(s.tools) > 0 && hasRefusal {
		return responsesChatStreamTransition{}, newChatServerError("unsupported_responses_output", "Responses tool-call turns with refusal content are not supported")
	}
	exposeTools := len(publishCalls) > 0 && !s.hasIncompleteTool
	if !exposeTools {
		publishCalls = nil
	}
	finishReason, err := responsesChatFinishReason(event.Response.Status, event.Response.IncompleteDetails, exposeTools)
	if err != nil {
		return responsesChatStreamTransition{}, err
	}
	chunks := make([]models.OpenAIStreamChunk, 0, len(s.tools)*2+2)
	if exposeTools {
		if s.config.ReplayStore == nil {
			replayErr := newChatServerError("responses_replay_unavailable", "Responses replay storage is unavailable")
			attachChatExecutionErrorUsage(replayErr, terminalUsage)
			return usageFailureTransition, replayErr
		}
		published, err := s.config.ReplayStore.Publish(responsesChatReplayPublishRequest{
			Route: s.config.ReplayRoute, AssistantContent: assistantContent, OutputItems: event.Response.Output, Calls: publishCalls,
		})
		if err != nil {
			replayErr := mapResponsesChatReplayPublishError(err)
			attachChatExecutionErrorUsage(replayErr, terminalUsage)
			return usageFailureTransition, replayErr
		}
		proxyByUpstream := make(map[string]string, len(published.Calls))
		for _, call := range published.Calls {
			proxyByUpstream[call.UpstreamCallID] = call.ProxyCallID
		}
		for _, tool := range s.tools {
			proxyID := proxyByUpstream[tool.upstreamCall]
			if proxyID == "" {
				return responsesChatStreamTransition{}, newChatServerError("responses_replay_state_invalid", "published Responses replay state is incomplete")
			}
			chunks = append(chunks, s.toolStartChunk(tool, proxyID))
			if tool.arguments.Len() > 0 {
				chunks = append(chunks, s.toolArgumentsChunk(tool))
			}
		}
	}

	chunks = append(chunks, s.finishChunk(finishReason))
	if terminalUsage != nil {
		// Canonical event streams always carry terminal usage for aggregation and
		// accounting. The OpenAI public adapter drops this chunk unless the original
		// client requested stream_options.include_usage; Anthropic/Gemini consume it internally.
		chunks = append(chunks, s.usageChunk(terminalUsage))
	}
	s.terminalSeen = true
	return responsesChatStreamTransition{chunks: chunks, terminal: true}, nil
}

func parseResponsesChatTopLevelError(data []byte) *chatExecutionError {
	var event struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		Param      string `json:"param"`
		StatusCode int    `json:"status_code"`
		Error      *struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return newChatServerError("invalid_responses_stream", "upstream returned malformed Responses error event")
	}
	errorType, code, message, param := "", strings.TrimSpace(event.Code), strings.TrimSpace(event.Message), strings.TrimSpace(event.Param)
	if event.Error != nil {
		errorType = strings.TrimSpace(event.Error.Type)
		if strings.TrimSpace(event.Error.Code) != "" {
			code = strings.TrimSpace(event.Error.Code)
		}
		if strings.TrimSpace(event.Error.Message) != "" {
			message = strings.TrimSpace(event.Error.Message)
		}
		if strings.TrimSpace(event.Error.Param) != "" {
			param = strings.TrimSpace(event.Error.Param)
		}
	}
	if errorType == "" {
		errorType = responsesChatErrorTypeForCode(code)
	}
	status := event.StatusCode
	if status < 400 || status > 599 {
		status = responsesChatFailureStatus(errorType, code)
	}
	if errorType == "" {
		switch status {
		case 400:
			errorType = "invalid_request_error"
		case 401:
			errorType = "authentication_error"
		case 403:
			errorType = "permission_error"
		case 429:
			errorType = "rate_limit_error"
		default:
			errorType = "server_error"
		}
	}
	if message == "" {
		message = "upstream Responses stream reported an error"
	}
	return &chatExecutionError{StatusCode: status, Type: errorType, Code: code, Param: param, Message: message}
}

func (s *responsesChatStreamState) handleFailed(data []byte) (responsesChatStreamTransition, error) {
	var event struct {
		Response responsesChatJSONEnvelope `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Response.Status != "failed" {
		return responsesChatStreamTransition{}, newChatServerError("invalid_responses_stream", "response.failed is malformed")
	}
	var usage *models.OpenAIUsage
	transition := responsesChatStreamTransition{}
	if event.Response.Usage != nil {
		usage = event.Response.Usage.toOpenAIUsage()
		transition.chunks = []models.OpenAIStreamChunk{s.usageChunk(usage)}
	}
	s.terminalSeen = true
	return transition, responsesChatFailedExecutionError(event.Response.Error, usage)
}

func (p *responsesChatTextPart) matches(_ string, outputIndex, contentIndex *int) bool {
	return outputIndex != nil && contentIndex != nil && *outputIndex == p.outputIndex && *contentIndex == p.contentIndex
}

func (p *responsesChatTextPart) matchesValue(value string) bool {
	if len(value) != p.bytes {
		return false
	}
	want := sha256.Sum256([]byte(value))
	return string(p.digest.Sum(nil)) == string(want[:])
}

func validateResponsesChatTerminalMessage(raw json.RawMessage, message *responsesChatMessageState) error {
	var item struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || message == nil || item.Type != "message" || item.Role != "assistant" || len(item.Content) != len(message.parts) {
		return newChatServerError("unsupported_responses_output", "terminal assistant message does not match the streamed output")
	}
	for contentIndex, content := range item.Content {
		part := message.parts[contentIndex]
		if part == nil || content.Type != part.kind {
			return newChatServerError("unsupported_responses_output", "terminal assistant message does not match the streamed output")
		}
		value := content.Text
		if part.kind == "refusal" {
			value = content.Refusal
		}
		if !part.matchesValue(value) {
			return newChatServerError("unsupported_responses_output", "terminal assistant message does not match the streamed output")
		}
	}
	return nil
}

func runResponsesChatStream(writer *chatStreamEventWriter, control *responsesChatStreamControl, readCh <-chan responsesChatStreamRead, readyCh chan<- responsesChatStreamReady, config responsesChatStreamConfig) {
	defer close(control.doneCh)
	defer control.closeBody()

	state := newResponsesChatStreamState(config)
	decoder := newResponsesChatSSEDecoder(config.MaxEventBytes)
	timer := time.NewTimer(config.PrecommitTimeout)
	defer timer.Stop()
	committed := false
	roleSent := false
	precommitBytes := 0
	readySent := false

	publishReady := func(err error) {
		if readySent {
			return
		}
		readySent = true
		readyCh <- responsesChatStreamReady{err: err}
	}
	awaitCommit := func() error {
		if committed {
			return nil
		}
		publishReady(nil)
		select {
		case <-control.commitCh:
			committed = true
			return nil
		case <-control.abortCh:
			return context.Canceled
		case <-writer.ctx.Done():
			return context.Cause(writer.ctx)
		}
	}
	emitRole := func() error {
		if roleSent {
			return nil
		}
		if err := writer.sendChunk(state.roleChunk()); err != nil {
			return err
		}
		roleSent = true
		return nil
	}
	emitChunks := func(chunks []models.OpenAIStreamChunk) error {
		if err := awaitCommit(); err != nil {
			return err
		}
		if err := emitRole(); err != nil {
			return err
		}
		for _, chunk := range chunks {
			if err := writer.sendChunk(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	fail := func(err error) {
		var streamErr *chatStreamError
		if !errors.As(err, &streamErr) {
			streamErr = newChatServerError("invalid_responses_stream", err.Error())
		}
		if !committed {
			publishReady(streamErr)
			return
		}
		_ = writer.fail(streamErr)
	}

	processMessage := func(msg responsesSSEMessage) error {
		transition, err := state.handleMessage(msg)
		if err != nil {
			if committed && len(transition.chunks) > 0 {
				if emitErr := emitChunks(transition.chunks); emitErr != nil {
					return emitErr
				}
			}
			return err
		}
		if len(transition.chunks) > 0 {
			if err := emitChunks(transition.chunks); err != nil {
				return err
			}
		}
		if transition.terminal {
			if !committed {
				if err := emitChunks(nil); err != nil {
					return err
				}
			}
			if err := writer.succeed(); err != nil {
				return err
			}
			return io.EOF
		}
		return nil
	}
	handleBytes := func(data []byte) error { return decoder.push(data, processMessage) }

	for {
		select {
		case <-timer.C:
			if !committed {
				if err := emitChunks(nil); err != nil {
					return
				}
			}
		case <-control.abortCh:
			return
		case <-writer.ctx.Done():
			return
		case read, ok := <-readCh:
			if !ok {
				if context.Cause(writer.ctx) != nil {
					return
				}
				fail(newChatServerError("responses_stream_truncated", "upstream Responses stream ended before a terminal event"))
				return
			}
			data := read.data
			if !committed && len(data) > 0 {
				remaining := config.PrecommitMaxBytes - precommitBytes
				if remaining < 0 {
					remaining = 0
				}
				if len(data) >= remaining {
					if remaining > 0 {
						if err := handleBytes(data[:remaining]); err != nil {
							if err == io.EOF {
								return
							}
							fail(err)
							return
						}
						precommitBytes += remaining
						data = data[remaining:]
					}
					if !committed {
						if err := emitChunks(nil); err != nil {
							return
						}
					}
				} else {
					precommitBytes += len(data)
				}
			}
			if len(data) > 0 {
				if err := handleBytes(data); err != nil {
					if err == io.EOF {
						return
					}
					fail(err)
					return
				}
			}
			if read.err != nil {
				if context.Cause(writer.ctx) != nil {
					return
				}
				if read.err == io.EOF && decoder.hasPending() {
					if err := decoder.finalize(processMessage); err != nil {
						if err == io.EOF {
							return
						}
						fail(err)
						return
					}
				}
				if read.err == io.EOF && state.terminalSeen {
					return
				}
				if read.err == io.EOF && decoder.hasPending() {
					fail(newChatServerError("responses_stream_truncated", "upstream Responses SSE event was truncated"))
				} else {
					fail(newChatServerError("responses_stream_truncated", "upstream Responses stream ended before a terminal event"))
				}
				return
			}
		}
	}
}
