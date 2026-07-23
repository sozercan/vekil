package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/sozercan/vekil/models"
)

// chatStreamEventBufferSize bounds producer lead without buffering an entire
// provider response. Chunks are passed by value; producers must treat nested
// slices and RawMessage values as immutable after sendChunk returns.
const chatStreamEventBufferSize = 16

var (
	errChatStreamTerminated         = errors.New("chat stream already terminated")
	errChatStreamConsumerStopped    = errors.New("chat stream consumer stopped")
	errChatStreamMissingTermination = errors.New("chat stream closed without a terminal event")
	errChatStreamClientWriteFailed  = errors.New("writing chat stream to client failed")
)

type chatStreamEventKind uint8

const (
	chatStreamEventChunk chatStreamEventKind = iota + 1
	chatStreamEventSuccess
	chatStreamEventError
)

type chatStreamEvent struct {
	kind      chatStreamEventKind
	chunk     models.OpenAIStreamChunk
	streamErr *chatStreamError
}

// chatStreamError names the execution error carried by a terminal stream event.
// It is an alias so pre-stream and post-commit Chat failures retain one typed
// classification across the execution module and public-protocol adapters.
type chatStreamError = chatExecutionError

func chatStreamErrorStatus(streamErr *chatStreamError) int {
	if streamErr == nil || streamErr.StatusCode < http.StatusBadRequest || streamErr.StatusCode > 599 {
		return http.StatusBadGateway
	}
	return streamErr.StatusCode
}

func chatStreamErrorMessage(streamErr *chatStreamError) string {
	if streamErr == nil {
		return "upstream stream error"
	}
	if message := strings.TrimSpace(streamErr.Message); message != "" {
		return message
	}
	if code := strings.TrimSpace(streamErr.Code); code != "" {
		return code
	}
	if errorType := strings.TrimSpace(streamErr.Type); errorType != "" {
		return errorType
	}
	return "upstream stream error"
}

// chatStreamEventWriter is the producer side of the bounded canonical Chat
// event transport. A writer has one logical producer; calls are serialized so
// a terminal event cannot overtake an in-flight chunk.
type chatStreamEventWriter struct {
	ctx    context.Context
	events chan chatStreamEvent

	mu         sync.Mutex
	terminated bool
}

// chatStreamEventStream is the consumer side of the canonical Chat event
// transport. Canceling it releases a producer blocked by the bounded buffer.
type chatStreamEventStream struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	events <-chan chatStreamEvent
}

func newChatStreamEventPipe(parent context.Context) (*chatStreamEventWriter, *chatStreamEventStream) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	events := make(chan chatStreamEvent, chatStreamEventBufferSize)
	return &chatStreamEventWriter{
			ctx:    ctx,
			events: events,
		}, &chatStreamEventStream{
			ctx:    ctx,
			cancel: cancel,
			events: events,
		}
}

func (w *chatStreamEventWriter) sendChunk(chunk models.OpenAIStreamChunk) error {
	return w.send(chatStreamEvent{kind: chatStreamEventChunk, chunk: chunk}, false)
}

func (w *chatStreamEventWriter) succeed() error {
	return w.send(chatStreamEvent{kind: chatStreamEventSuccess}, true)
}

func (w *chatStreamEventWriter) fail(streamErr *chatStreamError) error {
	if streamErr == nil {
		streamErr = &chatStreamError{StatusCode: http.StatusBadGateway}
	}
	return w.send(chatStreamEvent{kind: chatStreamEventError, streamErr: streamErr}, true)
}

func (w *chatStreamEventWriter) send(event chatStreamEvent, terminal bool) error {
	if w == nil {
		return errChatStreamTerminated
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminated {
		return errChatStreamTerminated
	}
	if terminal {
		w.terminated = true
	}

	select {
	case w.events <- event:
		if terminal {
			close(w.events)
		}
		return nil
	case <-w.ctx.Done():
		if cause := context.Cause(w.ctx); cause != nil {
			return cause
		}
		return w.ctx.Err()
	}
}

func (s *chatStreamEventStream) next() (chatStreamEvent, error) {
	if s == nil {
		return chatStreamEvent{}, errChatStreamMissingTermination
	}
	// A provider terminal event that was already queued before cancellation wins.
	// Drain buffered chunks/terminal state deterministically before observing the
	// context so shutdown cannot rewrite a completed provider turn.
	select {
	case event, ok := <-s.events:
		if !ok {
			return chatStreamEvent{}, errChatStreamMissingTermination
		}
		return event, nil
	default:
	}
	select {
	case event, ok := <-s.events:
		if !ok {
			return chatStreamEvent{}, errChatStreamMissingTermination
		}
		return event, nil
	case <-s.ctx.Done():
		if cause := context.Cause(s.ctx); cause != nil {
			return chatStreamEvent{}, cause
		}
		return chatStreamEvent{}, s.ctx.Err()
	}
}

func (s *chatStreamEventStream) stop(cause error) {
	if s == nil || s.cancel == nil {
		return
	}
	if cause == nil {
		cause = errChatStreamConsumerStopped
	}
	s.cancel(cause)
}

func consumeChatStreamEvents(stream *chatStreamEventStream, onChunk func(models.OpenAIStreamChunk) error) (err error) {
	defer func() {
		if stream != nil {
			stream.stop(err)
		}
	}()

	for {
		event, nextErr := stream.next()
		if nextErr != nil {
			return nextErr
		}
		switch event.kind {
		case chatStreamEventChunk:
			if onChunk != nil {
				if err := onChunk(event.chunk); err != nil {
					return err
				}
			}
		case chatStreamEventSuccess:
			return nil
		case chatStreamEventError:
			if event.streamErr == nil {
				return &chatStreamError{StatusCode: http.StatusBadGateway}
			}
			return event.streamErr
		default:
			return fmt.Errorf("unknown chat stream event kind %d", event.kind)
		}
	}
}

type chatStreamEventCallbacks struct {
	DropUsage bool
	OnUsage   func(*models.OpenAIUsage)
	OnFinal   func(*models.OpenAIResponse)
}

func selectChatStreamEventCallbacks(callbacks []chatStreamEventCallbacks) chatStreamEventCallbacks {
	if len(callbacks) == 0 {
		return chatStreamEventCallbacks{}
	}
	return callbacks[0]
}

func addChatStreamChunkForToolCapture(aggregator *openAIResponseAggregator, chunk models.OpenAIStreamChunk) {
	if aggregator == nil {
		return
	}
	if len(chunk.Choices) == 0 {
		aggregator.addChunk(chunk)
		return
	}
	toolOnly := chunk
	toolOnly.Choices = append([]models.OpenAIStreamChoice(nil), chunk.Choices...)
	for i := range toolOnly.Choices {
		toolOnly.Choices[i].Delta.Content = nil
		toolOnly.Choices[i].Delta.Refusal = nil
	}
	aggregator.addChunk(toolOnly)
}

func streamChatEventsToOpenAI(w http.ResponseWriter, stream *chatStreamEventStream, callbackOptions ...chatStreamEventCallbacks) error {
	setChatStreamSSEHeaders(w)
	callbacks := selectChatStreamEventCallbacks(callbackOptions)
	var aggregator *openAIResponseAggregator
	if callbacks.OnFinal != nil {
		aggregator = newOpenAIResponseAggregator()
	}

	err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		if chunk.Usage != nil && callbacks.OnUsage != nil {
			callbacks.OnUsage(chunk.Usage)
		}
		if aggregator != nil {
			addChatStreamChunkForToolCapture(aggregator, chunk)
		}
		if callbacks.DropUsage && chunk.Usage != nil && len(chunk.Choices) == 0 {
			return nil
		}
		if err := writeOpenAIChatSSEData(w, chunk); err != nil {
			return errors.Join(errChatStreamClientWriteFailed, err)
		}
		return nil
	})
	if err != nil {
		var streamErr *chatStreamError
		if errors.As(err, &streamErr) {
			if writeErr := writeOpenAIChatSSEError(w, streamErr); writeErr != nil {
				return errors.Join(streamErr, writeErr)
			}
		}
		return err
	}
	if aggregator != nil {
		callbacks.OnFinal(aggregator.buildResponse())
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return errors.Join(errChatStreamClientWriteFailed, err)
	}
	flushChatStreamWriter(w)
	return nil
}

func setChatStreamSSEHeaders(w http.ResponseWriter) {
	setSSEHeaders(w)
	w.Header().Del("Content-Length")
}

func writeOpenAIChatSSEData(w http.ResponseWriter, data interface{}) error {
	return writeChatStreamJSONFrame(w, "data: ", data)
}

func writeChatStreamJSONFrame(w http.ResponseWriter, prefix string, data interface{}) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	encoder := json.NewEncoder(buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(data); err != nil {
		return err
	}
	payload := bytes.TrimRight(buf.Bytes(), "\n")
	if _, err := fmt.Fprintf(w, "%s%s\n\n", prefix, payload); err != nil {
		return err
	}
	flushChatStreamWriter(w)
	return nil
}

func flushChatStreamWriter(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

type openAIChatStreamErrorEnvelope struct {
	Error openAIChatStreamErrorBody `json:"error"`
}

type openAIChatStreamErrorBody struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

func writeOpenAIChatSSEError(w http.ResponseWriter, streamErr *chatStreamError) error {
	errorType := strings.TrimSpace(streamErr.Type)
	if errorType == "" {
		switch chatStreamErrorStatus(streamErr) {
		case http.StatusTooManyRequests:
			errorType = "rate_limit_error"
		case http.StatusServiceUnavailable:
			errorType = "overloaded_error"
		default:
			errorType = "api_error"
		}
	}
	return writeChatStreamJSONFrame(w, "event: error\ndata: ", openAIChatStreamErrorEnvelope{
		Error: openAIChatStreamErrorBody{
			Type:    errorType,
			Code:    strings.TrimSpace(streamErr.Code),
			Param:   strings.TrimSpace(streamErr.Param),
			Message: chatStreamErrorMessage(streamErr),
		},
	})
}

func aggregateChatStreamEvents(stream *chatStreamEventStream) (*models.OpenAIResponse, error) {
	return aggregateChatStreamEventsWithOptions(stream, openAIResponseBuildOptions{})
}

func aggregatePolicyChatStreamEvents(stream *chatStreamEventStream) (*models.OpenAIResponse, error) {
	return aggregateChatStreamEventsWithOptions(stream, openAIResponseBuildOptions{
		preserveInvalidToolArguments: true,
		rejectInvalidTextDeltas:      true,
	})
}

func aggregateChatStreamEventsWithOptions(stream *chatStreamEventStream, options openAIResponseBuildOptions) (*models.OpenAIResponse, error) {
	aggregator := newOpenAIResponseAggregator()
	if err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		aggregator.addChunk(chunk)
		return nil
	}); err != nil {
		return nil, err
	}
	if options.rejectInvalidTextDeltas {
		if err := aggregator.policyTextDeltaError(); err != nil {
			return nil, err
		}
	}
	return aggregator.buildResponseWithOptions(options), nil
}

func streamChatEventsToAnthropic(
	w http.ResponseWriter,
	stream *chatStreamEventStream,
	model string,
	requestID string,
	callbackOptions ...chatStreamEventCallbacks,
) error {
	setChatStreamSSEHeaders(w)
	callbacks := selectChatStreamEventCallbacks(callbackOptions)
	var aggregator *openAIResponseAggregator
	if callbacks.OnFinal != nil {
		aggregator = newOpenAIResponseAggregator()
	}
	state := newAnthropicStreamState(w, model, requestID)
	if !state.start() {
		stream.stop(errChatStreamClientWriteFailed)
		return errChatStreamClientWriteFailed
	}

	err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		if chunk.Usage != nil && callbacks.OnUsage != nil {
			callbacks.OnUsage(chunk.Usage)
		}
		if aggregator != nil {
			addChatStreamChunkForToolCapture(aggregator, chunk)
		}
		if !state.consumeChunk(chunk) {
			return errChatStreamClientWriteFailed
		}
		return nil
	})
	if err != nil {
		var streamErr *chatStreamError
		if errors.As(err, &streamErr) {
			if !state.emitTypedError(mapAnthropicUpstreamStatus(chatStreamErrorStatus(streamErr)), chatStreamErrorMessage(streamErr)) {
				return errors.Join(streamErr, errChatStreamClientWriteFailed)
			}
		}
		return err
	}
	if !state.finish() {
		return errChatStreamClientWriteFailed
	}
	if aggregator != nil {
		callbacks.OnFinal(aggregator.buildResponse())
	}
	return nil
}

func streamChatEventsToGemini(w http.ResponseWriter, stream *chatStreamEventStream, callbackOptions ...chatStreamEventCallbacks) error {
	setChatStreamSSEHeaders(w)
	callbacks := selectChatStreamEventCallbacks(callbackOptions)
	var aggregator *openAIResponseAggregator
	if callbacks.OnFinal != nil {
		aggregator = newOpenAIResponseAggregator()
	}
	state := newGeminiStreamState(w)

	err := consumeChatStreamEvents(stream, func(chunk models.OpenAIStreamChunk) error {
		if chunk.Usage != nil && callbacks.OnUsage != nil {
			callbacks.OnUsage(chunk.Usage)
		}
		if aggregator != nil {
			addChatStreamChunkForToolCapture(aggregator, chunk)
		}
		if state.consumeChunk(chunk) {
			return nil
		}
		if state.upstreamProtocolError != nil {
			return &chatStreamError{
				StatusCode: http.StatusBadGateway,
				Type:       "api_error",
				Message:    state.upstreamProtocolError.Error(),
			}
		}
		return errChatStreamClientWriteFailed
	})
	if err != nil {
		var streamErr *chatStreamError
		if errors.As(err, &streamErr) {
			if !state.writeError(chatStreamErrorStatus(streamErr), chatStreamErrorMessage(streamErr)) {
				return errors.Join(streamErr, errChatStreamClientWriteFailed)
			}
		}
		return err
	}
	if !state.finish() {
		if state.upstreamProtocolError != nil {
			streamErr := &chatStreamError{
				StatusCode: http.StatusBadGateway,
				Type:       "api_error",
				Message:    state.upstreamProtocolError.Error(),
			}
			if !state.writeError(chatStreamErrorStatus(streamErr), chatStreamErrorMessage(streamErr)) {
				return errors.Join(streamErr, errChatStreamClientWriteFailed)
			}
			return streamErr
		}
		return errChatStreamClientWriteFailed
	}
	if aggregator != nil {
		callbacks.OnFinal(aggregator.buildResponse())
	}
	return nil
}
