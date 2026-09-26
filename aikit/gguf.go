package aikit

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// GGUFInfo is the subset of GGUF metadata the launcher needs.
type GGUFInfo struct {
	Architecture string
	// ContextLength is {arch}.context_length, the trained context (n_ctx_train).
	ContextLength int64
}

const (
	// maxGGUFHeaderBytes bounds how much of a file the reader consumes. Metadata
	// normally precedes the large tokenizer arrays, so a typical read stops
	// within a few kilobytes.
	maxGGUFHeaderBytes = 256 << 20
	maxGGUFKeyBytes    = 1 << 16
	maxGGUFKVCount     = 1 << 20
	maxGGUFArrayDepth  = 4
)

const (
	ggufTypeUint8   = 0
	ggufTypeInt8    = 1
	ggufTypeUint16  = 2
	ggufTypeInt16   = 3
	ggufTypeUint32  = 4
	ggufTypeInt32   = 5
	ggufTypeFloat32 = 6
	ggufTypeBool    = 7
	ggufTypeString  = 8
	ggufTypeArray   = 9
	ggufTypeUint64  = 10
	ggufTypeInt64   = 11
	ggufTypeFloat64 = 12
)

var errGGUFNotFound = errors.New("gguf metadata does not declare a trained context length")

type ggufReader struct {
	r *bufio.Reader
}

// ReadGGUFInfo parses GGUF v2/v3 metadata from the start of r. It stops as soon
// as both the architecture and its trained context length are known.
func ReadGGUFInfo(r io.Reader) (GGUFInfo, error) {
	reader := ggufReader{r: bufio.NewReaderSize(io.LimitReader(r, maxGGUFHeaderBytes), 64<<10)}
	var magic [4]byte
	if _, err := io.ReadFull(reader.r, magic[:]); err != nil {
		return GGUFInfo{}, fmt.Errorf("read gguf magic: %w", err)
	}
	if string(magic[:]) != "GGUF" {
		return GGUFInfo{}, fmt.Errorf("file is not a GGUF model")
	}
	version, err := reader.uint32()
	if err != nil {
		return GGUFInfo{}, fmt.Errorf("read gguf version: %w", err)
	}
	if version < 2 || version > 3 {
		return GGUFInfo{}, fmt.Errorf("unsupported GGUF version %d", version)
	}
	if _, err := reader.uint64(); err != nil { // tensor count
		return GGUFInfo{}, fmt.Errorf("read gguf tensor count: %w", err)
	}
	kvCount, err := reader.uint64()
	if err != nil {
		return GGUFInfo{}, fmt.Errorf("read gguf metadata count: %w", err)
	}
	if kvCount > maxGGUFKVCount {
		return GGUFInfo{}, fmt.Errorf("gguf metadata count %d is implausibly large", kvCount)
	}

	var info GGUFInfo
	contextLengths := map[string]int64{}
	for i := uint64(0); i < kvCount; i++ {
		key, err := reader.string(maxGGUFKeyBytes)
		if err != nil {
			return GGUFInfo{}, fmt.Errorf("read gguf metadata key: %w", err)
		}
		valueType, err := reader.uint32()
		if err != nil {
			return GGUFInfo{}, fmt.Errorf("read gguf metadata type for %q: %w", key, err)
		}
		switch {
		case key == "general.architecture" && valueType == ggufTypeString:
			value, err := reader.string(maxGGUFKeyBytes)
			if err != nil {
				return GGUFInfo{}, fmt.Errorf("read gguf architecture: %w", err)
			}
			info.Architecture = value
		case strings.HasSuffix(key, ".context_length"):
			value, ok, err := reader.integer(valueType)
			if err != nil {
				return GGUFInfo{}, fmt.Errorf("read gguf %q: %w", key, err)
			}
			if !ok {
				if err := reader.skip(valueType, 0); err != nil {
					return GGUFInfo{}, err
				}
				continue
			}
			contextLengths[strings.TrimSuffix(key, ".context_length")] = value
		default:
			if err := reader.skip(valueType, 0); err != nil {
				return GGUFInfo{}, fmt.Errorf("skip gguf metadata %q: %w", key, err)
			}
		}
		if info.Architecture != "" {
			if value, ok := contextLengths[info.Architecture]; ok {
				info.ContextLength = value
				return info, nil
			}
		}
	}
	if info.Architecture == "" {
		return GGUFInfo{}, fmt.Errorf("gguf metadata does not declare general.architecture")
	}
	return info, errGGUFNotFound
}

func (g ggufReader) uint32() (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(g.r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func (g ggufReader) uint64() (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(g.r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

func (g ggufReader) string(limit uint64) (string, error) {
	length, err := g.uint64()
	if err != nil {
		return "", err
	}
	if length > limit {
		return "", fmt.Errorf("gguf string of %d bytes exceeds %d", length, limit)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(g.r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (g ggufReader) integer(valueType uint32) (int64, bool, error) {
	switch valueType {
	case ggufTypeUint32:
		value, err := g.uint32()
		return int64(value), true, err
	case ggufTypeInt32:
		value, err := g.uint32()
		return int64(int32(value)), true, err
	case ggufTypeUint64:
		value, err := g.uint64()
		if value > 1<<62 {
			return 0, false, fmt.Errorf("value %d is out of range", value)
		}
		return int64(value), true, err
	case ggufTypeInt64:
		value, err := g.uint64()
		return int64(value), true, err
	default:
		return 0, false, nil
	}
}

func ggufScalarSize(valueType uint32) (uint64, bool) {
	switch valueType {
	case ggufTypeUint8, ggufTypeInt8, ggufTypeBool:
		return 1, true
	case ggufTypeUint16, ggufTypeInt16:
		return 2, true
	case ggufTypeUint32, ggufTypeInt32, ggufTypeFloat32:
		return 4, true
	case ggufTypeUint64, ggufTypeInt64, ggufTypeFloat64:
		return 8, true
	default:
		return 0, false
	}
}

func (g ggufReader) discard(n uint64) error {
	for n > 0 {
		step := n
		if step > 1<<30 {
			step = 1 << 30
		}
		discarded, err := g.r.Discard(int(step))
		n -= uint64(discarded)
		if err != nil {
			return err
		}
	}
	return nil
}

func (g ggufReader) skip(valueType uint32, depth int) error {
	if size, ok := ggufScalarSize(valueType); ok {
		return g.discard(size)
	}
	switch valueType {
	case ggufTypeString:
		length, err := g.uint64()
		if err != nil {
			return err
		}
		return g.discard(length)
	case ggufTypeArray:
		if depth >= maxGGUFArrayDepth {
			return fmt.Errorf("gguf arrays nest deeper than %d", maxGGUFArrayDepth)
		}
		elementType, err := g.uint32()
		if err != nil {
			return err
		}
		count, err := g.uint64()
		if err != nil {
			return err
		}
		if size, ok := ggufScalarSize(elementType); ok {
			if count > maxGGUFHeaderBytes/size {
				return fmt.Errorf("gguf array of %d elements exceeds the metadata limit", count)
			}
			return g.discard(count * size)
		}
		// Every string or nested array spends at least an 8-byte length.
		if count > maxGGUFHeaderBytes/8 {
			return fmt.Errorf("gguf array of %d elements exceeds the metadata limit", count)
		}
		for i := uint64(0); i < count; i++ {
			if err := g.skip(elementType, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown gguf metadata type %d", valueType)
	}
}
