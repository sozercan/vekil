package macosruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/vekil/internal/appcontrol"
)

func TestFrameReaderEnforcesIncrementalLimitAndUTF8(t *testing.T) {
	t.Run("exact limit", func(t *testing.T) {
		body := bytes.Repeat([]byte{'a'}, MaxFrameBytes)
		frame, err := newFrameReader(bytes.NewReader(append(body, '\n'))).ReadFrame()
		if err != nil || len(frame) != MaxFrameBytes {
			t.Fatalf("ReadFrame() len=%d err=%v", len(frame), err)
		}
	})
	t.Run("exact limit with CRLF", func(t *testing.T) {
		body := bytes.Repeat([]byte{'a'}, MaxFrameBytes)
		wire := append(append(body, '\r'), '\n')
		frame, err := newFrameReader(bytes.NewReader(wire)).ReadFrame()
		if err != nil || len(frame) != MaxFrameBytes {
			t.Fatalf("ReadFrame() len=%d err=%v", len(frame), err)
		}
	})
	t.Run("exact limit with split CRLF", func(t *testing.T) {
		body := bytes.Repeat([]byte{'a'}, MaxFrameBytes)
		wire := append(append(body, '\r'), '\n')
		reader := &frameReader{reader: bufio.NewReaderSize(bytes.NewReader(wire), MaxFrameBytes+1)}
		frame, err := reader.ReadFrame()
		if err != nil || !bytes.Equal(frame, body) {
			t.Fatalf("ReadFrame() len=%d err=%v", len(frame), err)
		}
	})
	t.Run("one byte over", func(t *testing.T) {
		body := bytes.Repeat([]byte{'a'}, MaxFrameBytes+1)
		_, err := newFrameReader(bytes.NewReader(append(body, '\n'))).ReadFrame()
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("ReadFrame() error = %v, want frame too large", err)
		}
	})
	t.Run("one byte over with CRLF", func(t *testing.T) {
		body := bytes.Repeat([]byte{'a'}, MaxFrameBytes+1)
		wire := append(append(body, '\r'), '\n')
		_, err := newFrameReader(bytes.NewReader(wire)).ReadFrame()
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("ReadFrame() error = %v, want frame too large", err)
		}
	})
	t.Run("invalid UTF-8", func(t *testing.T) {
		_, err := newFrameReader(bytes.NewReader([]byte{0xff, '\n'})).ReadFrame()
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("ReadFrame() error = %v, want invalid UTF-8", err)
		}
	})
	t.Run("unterminated EOF", func(t *testing.T) {
		_, err := newFrameReader(bytes.NewReader([]byte(`{"v":1}`))).ReadFrame()
		if !errors.Is(err, ErrUnterminatedFrame) {
			t.Fatalf("ReadFrame() error = %v, want unterminated frame", err)
		}
	})
}

func TestDecodeRequestEnvelopeRejectsDuplicateTopLevelFields(t *testing.T) {
	_, err := decodeRequestEnvelope([]byte(`{"v":1,"id":"one","id":"two","command":"get_state","payload":{}}`))
	if !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("decodeRequestEnvelope() error = %v, want duplicate field", err)
	}
}

func TestDecodeRequestEnvelopeRejectsDuplicatePayloadFieldsAtAnyDepth(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "direct", payload: `{"operation_id":"expected","operation_id":"other"}`},
		{name: "nested array", payload: `{"items":[{"operation_id":"expected","operation_id":"other"}]}`},
		{name: "escaped equivalent", payload: `{"operation_id":"expected","operation\u005fid":"other"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := []byte(`{"v":1,"id":"req_1","command":"cancel_operation","payload":` + test.payload + `}`)
			_, err := decodeRequestEnvelope(frame)
			if !errors.Is(err, ErrDuplicateField) {
				t.Fatalf("decodeRequestEnvelope() error = %v, want duplicate field", err)
			}
		})
	}
}

func TestRequestCacheIdempotencyAndIDReuseConflict(t *testing.T) {
	first, err := decodeRequestEnvelope([]byte(`{"v":1,"id":"req_1","command":"start","payload":{"a":1,"b":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := decodeRequestEnvelope([]byte(`{"v":1,"id":"req_1","command":"start","payload":{"b":2,"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := decodeRequestEnvelope([]byte(`{"v":1,"id":"req_1","command":"stop","payload":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	response := responseEnvelope{Version: 1, ID: "req_1", HelperEpoch: "hep", OK: true, Result: map[string]any{"operation_id": "op_1"}}
	var cache requestCache
	if err := cache.store(first, response); err != nil {
		t.Fatal(err)
	}
	got, found, reuseConflict, err := cache.lookup(reordered)
	if err != nil || !found || reuseConflict || got.ID != response.ID {
		t.Fatalf("reordered lookup = (%+v,%v,%v,%v)", got, found, reuseConflict, err)
	}
	_, found, reuseConflict, err = cache.lookup(conflict)
	if err != nil || !found || !reuseConflict {
		t.Fatalf("conflict lookup = (found=%v conflict=%v err=%v)", found, reuseConflict, err)
	}
}

func TestRequestCacheEvictsOldestCompletedIDsAtCapacity(t *testing.T) {
	request := func(index int, command string) requestEnvelope {
		return requestEnvelope{
			Version: ProtocolMax,
			ID:      fmt.Sprintf("req_%d", index),
			Command: command,
			Payload: json.RawMessage(`{}`),
		}
	}
	response := func(index int) responseEnvelope {
		return responseEnvelope{Version: ProtocolMax, ID: fmt.Sprintf("req_%d", index), HelperEpoch: "hep", OK: true}
	}

	var cache requestCache
	for index := 0; index < maxRequestIDs; index++ {
		if err := cache.store(request(index, "get_state"), response(index)); err != nil {
			t.Fatalf("store request %d: %v", index, err)
		}
	}
	if err := cache.store(request(maxRequestIDs, "get_state"), response(maxRequestIDs)); err != nil {
		t.Fatal(err)
	}
	if got := len(cache.entries); got != maxRequestIDs {
		t.Fatalf("cache entries = %d, want %d", got, maxRequestIDs)
	}
	if _, found, _, err := cache.lookup(request(0, "get_state")); err != nil || found {
		t.Fatalf("oldest lookup = found %v, err %v", found, err)
	}
	if got, found, conflict, err := cache.lookup(request(1, "get_state")); err != nil || !found || conflict || got.ID != "req_1" {
		t.Fatalf("retained lookup = (%+v,%v,%v,%v)", got, found, conflict, err)
	}
	if got, found, conflict, err := cache.lookup(request(maxRequestIDs, "get_state")); err != nil || !found || conflict || got.ID != fmt.Sprintf("req_%d", maxRequestIDs) {
		t.Fatalf("newest lookup = (%+v,%v,%v,%v)", got, found, conflict, err)
	}

	reused := request(0, "stop")
	if _, found, _, err := cache.lookup(reused); err != nil || found {
		t.Fatalf("evicted ID reuse lookup = found %v, err %v", found, err)
	}
	if err := cache.store(reused, response(0)); err != nil {
		t.Fatalf("store evicted ID: %v", err)
	}
}

func TestStartPayloadRejectsAutomaticInteractiveAuthentication(t *testing.T) {
	h := &helper{epoch: "hep_test"}
	response, _, _ := h.dispatch(t.Context(), requestEnvelope{
		Version: ProtocolMax,
		ID:      "req_start",
		Command: "start",
		Payload: json.RawMessage(`{"expected_config_revision":"","reason":"automaticLaunch","allows_interactive_authentication":true}`),
	})
	if response.OK || response.Error == nil || response.Error.Code != "invalid_payload" {
		t.Fatalf("response = %+v, want invalid_payload", response)
	}
}

func TestProtocolWriterConcurrentFramesRemainCompleteAndStateNeverRegresses(t *testing.T) {
	var output bytes.Buffer
	writer := newProtocolWriter(&output)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for index := 0; index < 128; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := responseEnvelope{Version: 1, ID: fmt.Sprintf("req_%d", index), HelperEpoch: "hep", OK: true, Result: map[string]int{"index": index}}
			if err := writer.SendCritical(ctx, response); err != nil {
				t.Errorf("SendCritical() error = %v", err)
			}
		}()
	}
	for revision := uint64(1); revision <= 100; revision++ {
		event := eventEnvelope{Version: 1, Event: "state", HelperEpoch: "hep", StateRevision: revision, Payload: map[string]any{"revision": revision}}
		if err := writer.SendState(revision, event); err != nil {
			t.Fatal(err)
		}
	}
	terminal := eventEnvelope{Version: 1, Event: "operation", HelperEpoch: "hep", StateRevision: 101, Payload: map[string]string{"status": "succeeded"}}
	state := eventEnvelope{Version: 1, Event: "state", HelperEpoch: "hep", StateRevision: 101, Payload: map[string]any{"revision": 101}}
	if err := writer.SendCriticalWithState(ctx, terminal, 101, state); err != nil {
		t.Fatal(err)
	}
	// This stale replaceable state must not be emitted after the terminal batch.
	_ = writer.SendState(50, eventEnvelope{Version: 1, Event: "state", HelperEpoch: "hep", StateRevision: 50, Payload: map[string]any{}})
	wg.Wait()
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	responses := 0
	lastState := uint64(0)
	for scanner.Scan() {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatalf("invalid frame %q: %v", scanner.Text(), err)
		}
		if _, ok := envelope["id"]; ok {
			responses++
		}
		if raw := envelope["event"]; raw != nil {
			var event string
			_ = json.Unmarshal(raw, &event)
			if event == "state" {
				var revision uint64
				_ = json.Unmarshal(envelope["state_revision"], &revision)
				if revision <= lastState {
					t.Fatalf("state revision regressed from %d to %d", lastState, revision)
				}
				lastState = revision
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if responses != 128 {
		t.Fatalf("responses = %d, want 128", responses)
	}
	if lastState != 101 {
		t.Fatalf("last state revision = %d, want 101", lastState)
	}
	if strings.Contains(output.String(), "\n\n") {
		t.Fatal("writer emitted an empty frame")
	}
}

type stateSignalingWriter struct {
	bytes.Buffer
	stateWritten chan struct{}
	once         sync.Once
}

func (w *stateSignalingWriter) Write(body []byte) (int, error) {
	if bytes.Contains(body, []byte(`"event":"state"`)) {
		w.once.Do(func() { close(w.stateWritten) })
	}
	return w.Buffer.Write(body)
}

func TestTerminalPairCannotBeOvertakenByNewerState(t *testing.T) {
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			return newTestRuntimeForHelper(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	output := &stateSignalingWriter{stateWritten: make(chan struct{})}
	writer := newProtocolWriter(output)
	h := &helper{
		epoch:  "hep_ordering",
		writer: writer,
		opts: HelperOptions{
			Controller: controller, Configuration: manager, ShutdownTimeout: 5 * time.Second,
		},
	}

	originalMarshal := jsonMarshal
	terminalMarshalStarted := make(chan struct{})
	releaseTerminalMarshal := make(chan struct{})
	var terminalMarshalOnce sync.Once
	var releaseOnce sync.Once
	releaseTerminal := func() {
		releaseOnce.Do(func() { close(releaseTerminalMarshal) })
	}
	t.Cleanup(releaseTerminal)
	jsonMarshal = func(value any) ([]byte, error) {
		if event, ok := value.(eventEnvelope); ok && event.Event == "operation" {
			terminalMarshalOnce.Do(func() {
				close(terminalMarshalStarted)
				<-releaseTerminalMarshal
			})
		}
		return originalMarshal(value)
	}
	defer func() { jsonMarshal = originalMarshal }()

	terminalDone := make(chan struct{})
	go func() {
		defer close(terminalDone)
		h.emitTerminal("op_ordering", "start", string(appcontrol.OperationSucceeded), nil)
	}()

	select {
	case <-terminalMarshalStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal event did not reach the admission barrier")
	}

	// If the projector lock is no longer held here, force the vulnerable
	// schedule: publish and physically write the newer replaceable state before
	// allowing the older terminal pair to enter the critical queue. With the
	// fix, the publisher blocks on the projector lock until the pair is admitted.
	projectorUnlocked := h.projector.mu.TryLock()
	if projectorUnlocked {
		h.projector.mu.Unlock()
	}
	publishStarted := make(chan struct{})
	publishDone := make(chan error, 1)
	go func() {
		close(publishStarted)
		publishDone <- h.publishState()
	}()
	<-publishStarted

	if projectorUnlocked {
		select {
		case err := <-publishDone:
			if err != nil {
				t.Fatalf("publishState() error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("newer state did not reach the writer")
		}
		select {
		case <-output.stateWritten:
		case <-time.After(5 * time.Second):
			t.Fatal("newer state was not written before releasing the terminal event")
		}
	}
	releaseTerminal()

	select {
	case <-terminalDone:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal event did not finish")
	}
	if !projectorUnlocked {
		select {
		case err := <-publishDone:
			if err != nil {
				t.Fatalf("publishState() error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("newer state did not publish after the terminal pair")
		}
	}

	closeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := writer.Close(closeCtx); err != nil {
		t.Fatal(err)
	}

	type wireEvent struct {
		Event         string `json:"event"`
		StateRevision uint64 `json:"state_revision"`
	}
	var events []wireEvent
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	for scanner.Scan() {
		var event wireEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode frame %q: %v", scanner.Text(), err)
		}
		if event.Event != "" && event.StateRevision != 0 {
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("revisioned events = %+v, want operation plus paired and newer states", events)
	}
	for index := 1; index < len(events); index++ {
		if events[index].StateRevision <= events[index-1].StateRevision {
			t.Fatalf("protocol revision regressed: %+v", events)
		}
	}
	if events[0].Event != "operation" || events[1].Event != "state" || events[1].StateRevision != events[0].StateRevision+1 {
		t.Fatalf("terminal pair was not emitted atomically: %+v", events)
	}
}

func TestStateProjectorCriticalPairsAllocateConsecutiveRevisions(t *testing.T) {
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			return newTestRuntimeForHelper(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{epoch: "hep", opts: HelperOptions{Controller: controller, Configuration: manager}}
	type pair struct{ event, state uint64 }
	pairs := make(chan pair, 256)
	var wg sync.WaitGroup
	for index := 0; index < 128; index++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.projector.mu.Lock()
			eventRevision, stateRevision, _ := h.projector.criticalPairLocked(h)
			h.projector.mu.Unlock()
			pairs <- pair{event: eventRevision, state: stateRevision}
		}()
		go func() {
			defer wg.Done()
			h.projector.mu.Lock()
			_, _ = h.projector.nextLocked(h)
			h.projector.mu.Unlock()
		}()
	}
	wg.Wait()
	close(pairs)
	for pair := range pairs {
		if pair.state != pair.event+1 {
			t.Fatalf("critical pair interleaved: event=%d state=%d", pair.event, pair.state)
		}
	}
}

func TestStateProjectorCurrentRebuildsExternalConfigurationState(t *testing.T) {
	path, initialRevision := writeExternalConfigForSwitchTest(t, "initial-model")
	manager := newManagerForTest(t)
	if _, err := manager.SelectExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			return newTestRuntimeForHelper(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{epoch: "hep", opts: HelperOptions{Controller: controller, Configuration: manager}}

	firstStateRevision, first := h.projector.current(h)
	if first.Configuration.SelectedRevision != initialRevision {
		t.Fatalf("initial selected revision = %q, want %q", first.Configuration.SelectedRevision, initialRevision)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(body, []byte("initial-model"), []byte("updated-model"), 1)
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	updatedRevision, _ := configRevision(updated)

	refreshedStateRevision, refreshed := h.projector.current(h)
	if refreshedStateRevision != firstStateRevision+1 {
		t.Fatalf("refreshed state revision = %d, want %d", refreshedStateRevision, firstStateRevision+1)
	}
	if refreshed.Configuration.SelectedRevision != updatedRevision {
		t.Fatalf("refreshed selected revision = %q, want %q", refreshed.Configuration.SelectedRevision, updatedRevision)
	}
}
