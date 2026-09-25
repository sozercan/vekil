package aikit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

type ggufBuilder struct {
	buf   bytes.Buffer
	count uint64
}

func (b *ggufBuilder) str(value string) {
	_ = binary.Write(&b.buf, binary.LittleEndian, uint64(len(value)))
	b.buf.WriteString(value)
}

func (b *ggufBuilder) kvString(key, value string) {
	b.str(key)
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeString))
	b.str(value)
	b.count++
}

func (b *ggufBuilder) kvUint32(key string, value uint32) {
	b.str(key)
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeUint32))
	_ = binary.Write(&b.buf, binary.LittleEndian, value)
	b.count++
}

func (b *ggufBuilder) kvUint64(key string, value uint64) {
	b.str(key)
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeUint64))
	_ = binary.Write(&b.buf, binary.LittleEndian, value)
	b.count++
}

func (b *ggufBuilder) kvStringArray(key string, values []string) {
	b.str(key)
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeArray))
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeString))
	_ = binary.Write(&b.buf, binary.LittleEndian, uint64(len(values)))
	for _, value := range values {
		b.str(value)
	}
	b.count++
}

func (b *ggufBuilder) kvFloatArray(key string, count int) {
	b.str(key)
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeArray))
	_ = binary.Write(&b.buf, binary.LittleEndian, uint32(ggufTypeFloat32))
	_ = binary.Write(&b.buf, binary.LittleEndian, uint64(count))
	b.buf.Write(make([]byte, 4*count))
	b.count++
}

func (b *ggufBuilder) bytes(version uint32) []byte {
	var out bytes.Buffer
	out.WriteString("GGUF")
	_ = binary.Write(&out, binary.LittleEndian, version)
	_ = binary.Write(&out, binary.LittleEndian, uint64(3)) // tensors
	_ = binary.Write(&out, binary.LittleEndian, b.count)
	out.Write(b.buf.Bytes())
	return out.Bytes()
}

func testGGUF(arch string, contextLength uint32) []byte {
	var b ggufBuilder
	b.kvString("general.architecture", arch)
	b.kvString("general.name", "Test Model")
	b.kvStringArray("tokenizer.ggml.tokens", []string{"a", "b", "c"})
	b.kvFloatArray("tokenizer.ggml.scores", 3)
	b.kvUint32(arch+".context_length", contextLength)
	return b.bytes(3)
}

func TestReadGGUFInfo(t *testing.T) {
	info, err := ReadGGUFInfo(bytes.NewReader(testGGUF("qwen35", 262144)))
	if err != nil {
		t.Fatalf("ReadGGUFInfo: %v", err)
	}
	if info.Architecture != "qwen35" || info.ContextLength != 262144 {
		t.Fatalf("info = %+v", info)
	}
}

func TestReadGGUFInfoStopsAfterContextLength(t *testing.T) {
	var b ggufBuilder
	b.kvString("general.architecture", "llama")
	b.kvUint64("llama.context_length", 131072)
	data := b.bytes(3)
	// Anything after the answer must not be read.
	reader := io.MultiReader(bytes.NewReader(data), errReader{})
	info, err := ReadGGUFInfo(reader)
	if err != nil || info.ContextLength != 131072 {
		t.Fatalf("ReadGGUFInfo = %+v, %v", info, err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read past metadata") }

func TestReadGGUFInfoIgnoresOtherArchitectureContext(t *testing.T) {
	var b ggufBuilder
	b.kvUint32("clip.context_length", 77)
	b.kvString("general.architecture", "gemma4")
	b.kvUint32("gemma4.context_length", 131072)
	info, err := ReadGGUFInfo(bytes.NewReader(b.bytes(3)))
	if err != nil || info.ContextLength != 131072 {
		t.Fatalf("ReadGGUFInfo = %+v, %v", info, err)
	}
}

func TestReadGGUFInfoErrors(t *testing.T) {
	var missing ggufBuilder
	missing.kvString("general.architecture", "llama")
	_, err := ReadGGUFInfo(bytes.NewReader(missing.bytes(3)))
	if !errors.Is(err, errGGUFNotFound) {
		t.Fatalf("missing context error = %v", err)
	}

	if _, err := ReadGGUFInfo(strings.NewReader("NOPE")); err == nil || !strings.Contains(err.Error(), "not a GGUF") {
		t.Fatalf("bad magic error = %v", err)
	}
	if _, err := ReadGGUFInfo(bytes.NewReader(testGGUF("llama", 1)[:4])); err == nil {
		t.Fatal("truncated header was accepted")
	}
	v1 := testGGUF("llama", 4096)
	binary.LittleEndian.PutUint32(v1[4:8], 1)
	if _, err := ReadGGUFInfo(bytes.NewReader(v1)); err == nil || !strings.Contains(err.Error(), "unsupported GGUF version") {
		t.Fatalf("v1 error = %v", err)
	}
}
