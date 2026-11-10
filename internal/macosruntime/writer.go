package macosruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

var jsonMarshal = json.Marshal

type writeBatch struct {
	frames []protocolFrame
	ack    chan error
}

type protocolFrame struct {
	body          []byte
	stateRevision uint64
}

// protocolWriter is the sole stdout owner. Critical batches are bounded and
// never dropped; replaceable state is coalesced to the newest frame.
type protocolWriter struct {
	writer io.Writer

	critical  chan writeBatch
	stateWake chan struct{}
	closeCh   chan struct{}
	done      chan struct{}

	stateMu sync.Mutex
	state   protocolFrame
	errMu   sync.Mutex
	err     error
}

func newProtocolWriter(writer io.Writer) *protocolWriter {
	w := &protocolWriter{
		writer:    writer,
		critical:  make(chan writeBatch, 64),
		stateWake: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		done:      make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *protocolWriter) SendCritical(ctx context.Context, values ...any) error {
	frames := make([]protocolFrame, 0, len(values))
	for _, value := range values {
		frame, err := encodeFrame(value)
		if err != nil {
			return err
		}
		frames = append(frames, protocolFrame{body: frame})
	}
	batch := writeBatch{frames: frames, ack: make(chan error, 1)}
	select {
	case w.critical <- batch:
	case <-w.done:
		return w.writeError()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-batch.ack:
		return err
	case <-w.done:
		return w.writeError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendCriticalWithState writes a terminal/correlated frame and its matching
// state snapshot as one ordered, non-droppable batch.
func (w *protocolWriter) SendCriticalWithState(ctx context.Context, value any, revision uint64, state any) error {
	first, err := encodeFrame(value)
	if err != nil {
		return err
	}
	second, err := encodeFrame(state)
	if err != nil {
		return err
	}
	batch := writeBatch{frames: []protocolFrame{{body: first}, {body: second, stateRevision: revision}}, ack: make(chan error, 1)}
	select {
	case w.critical <- batch:
	case <-w.done:
		return w.writeError()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-batch.ack:
		return err
	case <-w.done:
		return w.writeError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *protocolWriter) SendState(revision uint64, value any) error {
	frame, err := encodeFrame(value)
	if err != nil {
		return err
	}
	w.stateMu.Lock()
	w.state = protocolFrame{body: frame, stateRevision: revision}
	w.stateMu.Unlock()
	select {
	case w.stateWake <- struct{}{}:
	default:
	}
	return nil
}

func (w *protocolWriter) Close(ctx context.Context) error {
	select {
	case <-w.closeCh:
	default:
		close(w.closeCh)
	}
	select {
	case <-w.done:
		return w.terminalError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *protocolWriter) run() {
	defer close(w.done)
	var lastStateRevision uint64
	for {
		// Prefer correlated/terminal frames whenever they are already queued.
		select {
		case batch := <-w.critical:
			if !w.writeBatch(batch, &lastStateRevision) {
				return
			}
			continue
		default:
		}
		select {
		case batch := <-w.critical:
			if !w.writeBatch(batch, &lastStateRevision) {
				return
			}
		case <-w.stateWake:
			frame := w.takeState()
			if len(frame.body) != 0 && frame.stateRevision > lastStateRevision {
				if err := writeAll(w.writer, frame.body); err != nil {
					w.setError(err)
					return
				}
				lastStateRevision = frame.stateRevision
			}
		case <-w.closeCh:
			// All critical callers wait for their acknowledgements before close.
			// A final coalesced state is best-effort.
			if frame := w.takeState(); len(frame.body) != 0 && frame.stateRevision > lastStateRevision {
				if err := writeAll(w.writer, frame.body); err != nil {
					w.setError(err)
				}
			}
			return
		}
	}
}

func (w *protocolWriter) writeBatch(batch writeBatch, lastStateRevision *uint64) bool {
	var err error
	for _, frame := range batch.frames {
		if frame.stateRevision > 0 && frame.stateRevision <= *lastStateRevision {
			continue
		}
		if err = writeAll(w.writer, frame.body); err != nil {
			break
		}
		if frame.stateRevision > 0 {
			*lastStateRevision = frame.stateRevision
		}
	}
	batch.ack <- err
	close(batch.ack)
	if err != nil {
		w.setError(err)
		return false
	}
	return true
}

func (w *protocolWriter) takeState() protocolFrame {
	w.stateMu.Lock()
	frame := w.state
	w.state = protocolFrame{}
	w.stateMu.Unlock()
	return frame
}

func (w *protocolWriter) setError(err error) {
	w.errMu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.errMu.Unlock()
}

func (w *protocolWriter) writeError() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	if w.err != nil {
		return w.err
	}
	return errors.New("protocol writer closed")
}

func (w *protocolWriter) terminalError() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}
