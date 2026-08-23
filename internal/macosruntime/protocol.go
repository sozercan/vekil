package macosruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolMin   = 1
	ProtocolMax   = 1
	MaxFrameBytes = 1 << 20
	maxRequestIDs = 4096
)

var (
	ErrFrameTooLarge     = errors.New("protocol frame exceeds 1 MiB")
	ErrInvalidUTF8       = errors.New("protocol frame is not valid UTF-8")
	ErrUnterminatedFrame = errors.New("protocol frame is missing a newline terminator")
	ErrDuplicateField    = errors.New("protocol envelope contains a duplicate field")
)

type requestEnvelope struct {
	Version int
	ID      string
	Command string
	Payload json.RawMessage
}

type responseEnvelope struct {
	Version     int            `json:"v"`
	ID          string         `json:"id"`
	HelperEpoch string         `json:"helper_epoch"`
	OK          bool           `json:"ok"`
	Result      any            `json:"result,omitempty"`
	Error       *ProtocolError `json:"error,omitempty"`
}

type eventEnvelope struct {
	Version       int    `json:"v"`
	Event         string `json:"event"`
	HelperEpoch   string `json:"helper_epoch"`
	StateRevision uint64 `json:"state_revision,omitempty"`
	Payload       any    `json:"payload"`
}

// ProtocolError is the complete allowlisted error shape.
type ProtocolError struct {
	Code           string       `json:"code"`
	UserMessage    string       `json:"user_message"`
	Retryable      bool         `json:"retryable"`
	RecoveryAction string       `json:"recovery_action,omitempty"`
	FieldErrors    []FieldError `json:"field_errors"`
}

// FieldError is structured so clients never parse Go error strings.
type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decodeRequestEnvelope(frame []byte) (requestEnvelope, error) {
	if len(frame) > MaxFrameBytes {
		return requestEnvelope{}, ErrFrameTooLarge
	}
	if !utf8.Valid(frame) {
		return requestEnvelope{}, ErrInvalidUTF8
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return requestEnvelope{}, fmt.Errorf("decode protocol envelope: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return requestEnvelope{}, errors.New("protocol envelope must be a JSON object")
	}
	seen := make(map[string]struct{}, 4)
	values := make(map[string]json.RawMessage, 4)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return requestEnvelope{}, fmt.Errorf("decode protocol field: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return requestEnvelope{}, errors.New("protocol field name is not a string")
		}
		if _, duplicate := seen[name]; duplicate {
			return requestEnvelope{}, fmt.Errorf("%w %q", ErrDuplicateField, name)
		}
		seen[name] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return requestEnvelope{}, fmt.Errorf("decode protocol field %q: %w", name, err)
		}
		values[name] = append(json.RawMessage(nil), raw...)
	}
	if _, err := decoder.Token(); err != nil {
		return requestEnvelope{}, fmt.Errorf("close protocol envelope: %w", err)
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("unexpected trailing token %v", token)
		}
		return requestEnvelope{}, fmt.Errorf("protocol envelope has trailing data: %w", err)
	}
	for name := range values {
		switch name {
		case "v", "id", "command", "payload":
		default:
			return requestEnvelope{}, fmt.Errorf("unknown protocol envelope field %q", name)
		}
	}

	var request requestEnvelope
	if raw := values["v"]; raw == nil {
		return requestEnvelope{}, errors.New("protocol version is required")
	} else if err := json.Unmarshal(raw, &request.Version); err != nil {
		return requestEnvelope{}, errors.New("protocol version must be an integer")
	}
	if request.Version < ProtocolMin || request.Version > ProtocolMax {
		return requestEnvelope{}, fmt.Errorf("unsupported protocol version %d", request.Version)
	}
	if raw := values["id"]; raw == nil {
		return requestEnvelope{}, errors.New("request id is required")
	} else if err := json.Unmarshal(raw, &request.ID); err != nil {
		return requestEnvelope{}, errors.New("request id must be a string")
	}
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" || len(request.ID) > 256 {
		return requestEnvelope{}, errors.New("request id must contain 1 to 256 bytes")
	}
	if raw := values["command"]; raw == nil {
		return requestEnvelope{}, errors.New("command is required")
	} else if err := json.Unmarshal(raw, &request.Command); err != nil {
		return requestEnvelope{}, errors.New("command must be a string")
	}
	request.Command = strings.TrimSpace(request.Command)
	if request.Command == "" || len(request.Command) > 128 {
		return requestEnvelope{}, errors.New("command must contain 1 to 128 bytes")
	}
	request.Payload = values["payload"]
	if len(request.Payload) == 0 || bytes.Equal(bytes.TrimSpace(request.Payload), []byte("null")) {
		request.Payload = json.RawMessage(`{}`)
	}
	return request, nil
}

func decodePayload(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func requestFingerprint(request requestEnvelope) ([32]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(request.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return [32]byte{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, request.Command)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(canonical)
	var fingerprint [sha256.Size]byte
	hash.Sum(fingerprint[:0])
	return fingerprint, nil
}

type cachedRequest struct {
	fingerprint [32]byte
	response    responseEnvelope
}

type requestCache struct {
	entries map[string]cachedRequest
}

func (c *requestCache) lookup(request requestEnvelope) (responseEnvelope, bool, bool, error) {
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return responseEnvelope{}, false, false, err
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedRequest)
	}
	entry, ok := c.entries[request.ID]
	if !ok {
		return responseEnvelope{}, false, false, nil
	}
	return entry.response, true, entry.fingerprint != fingerprint, nil
}

func (c *requestCache) store(request requestEnvelope, response responseEnvelope) error {
	fingerprint, err := requestFingerprint(request)
	if err != nil {
		return err
	}
	if c.entries == nil {
		c.entries = make(map[string]cachedRequest)
	}
	if existing, ok := c.entries[request.ID]; ok {
		if existing.fingerprint != fingerprint {
			return errors.New("request id reuse conflict")
		}
		return nil
	}
	if len(c.entries) >= maxRequestIDs {
		return errors.New("request id cache is full")
	}
	c.entries[request.ID] = cachedRequest{fingerprint: fingerprint, response: response}
	return nil
}
