package macosruntime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

type frameReader struct{ reader *bufio.Reader }

func newFrameReader(reader io.Reader) *frameReader {
	return &frameReader{reader: bufio.NewReaderSize(reader, 32<<10)}
}

func (r *frameReader) ReadFrame() ([]byte, error) {
	var frame []byte
	pendingCR := false
	for {
		fragment, err := r.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			newline := fragment[len(fragment)-1] == '\n'
			if newline {
				fragment = fragment[:len(fragment)-1]
			}
			if pendingCR {
				if !newline || len(fragment) > 0 {
					if len(frame) == MaxFrameBytes {
						return nil, ErrFrameTooLarge
					}
					frame = append(frame, '\r')
				}
				pendingCR = false
			}
			if newline && len(fragment) > 0 && fragment[len(fragment)-1] == '\r' {
				fragment = fragment[:len(fragment)-1]
			} else if !newline && len(fragment) > 0 && fragment[len(fragment)-1] == '\r' {
				fragment = fragment[:len(fragment)-1]
				pendingCR = true
			}
			if len(frame)+len(fragment) > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			frame = append(frame, fragment...)
			if newline {
				if len(frame) == 0 {
					return nil, errors.New("empty protocol frame")
				}
				if !utf8.Valid(frame) {
					return nil, ErrInvalidUTF8
				}
				return frame, nil
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if pendingCR {
				if len(frame) == MaxFrameBytes {
					return nil, ErrFrameTooLarge
				}
				frame = append(frame, '\r')
			}
			if len(frame) == 0 {
				return nil, io.EOF
			}
			if !utf8.Valid(frame) {
				return nil, ErrInvalidUTF8
			}
			return nil, ErrUnterminatedFrame
		case err != nil:
			return nil, fmt.Errorf("read protocol frame: %w", err)
		}
	}
}

func encodeFrame(value any) ([]byte, error) {
	body, err := jsonMarshal(value)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	if bytes.IndexByte(body, '\n') >= 0 {
		return nil, errors.New("encoded protocol frame contains a newline")
	}
	return append(body, '\n'), nil
}
