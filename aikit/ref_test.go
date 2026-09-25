package aikit

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tests := []struct {
		name      string
		raw       string
		kind      RefKind
		image     string
		source    string
		modelName string
		premade   bool
		wantErr   string
	}{
		{name: "premade", raw: "qwen3.8:27b", kind: RefImage, image: "ghcr.io/kaito-project/aikit/qwen3.8:27b", premade: true},
		{name: "premade with dash", raw: "devstral-small2:24b", kind: RefImage, image: "ghcr.io/kaito-project/aikit/devstral-small2:24b", premade: true},
		{name: "premade missing tag", raw: "qwen3.8", wantErr: "needs a tag"},
		{name: "full image", raw: "ghcr.io/me/model:v1", kind: RefImage, image: "ghcr.io/me/model:v1"},
		{name: "localhost image", raw: "localhost/model:v1", kind: RefImage, image: "localhost/model:v1"},
		{name: "registry port", raw: "registry:5000/model:v1", kind: RefImage, image: "registry:5000/model:v1"},
		{name: "digest image", raw: "ghcr.io/me/model@sha256:" + strings.Repeat("b", 64), kind: RefImage, image: "ghcr.io/me/model@sha256:" + strings.Repeat("b", 64)},
		{
			name: "gguf url", raw: "https://huggingface.co/unsloth/Qwen3.5-2B-GGUF/resolve/main/Qwen3.5-2B-Q4_K_M.gguf",
			kind: RefRunnerGGUF, source: "https://huggingface.co/unsloth/Qwen3.5-2B-GGUF/resolve/main/Qwen3.5-2B-Q4_K_M.gguf", modelName: "Qwen3.5-2B-Q4_K_M",
		},
		{
			name: "gguf url with query", raw: "https://example.com/models/tiny.gguf?download=true",
			kind: RefRunnerGGUF, source: "https://example.com/models/tiny.gguf?download=true", modelName: "tiny",
		},
		{
			name: "hf shorthand gguf", raw: "hf.co/unsloth/Qwen3.5-2B-GGUF/Qwen3.5-2B-Q4_K_M.gguf",
			kind: RefRunnerGGUF, source: "https://huggingface.co/unsloth/Qwen3.5-2B-GGUF/resolve/main/Qwen3.5-2B-Q4_K_M.gguf", modelName: "Qwen3.5-2B-Q4_K_M",
		},
		{
			name: "hf repo", raw: "hf.co/Qwen/Qwen3-0.6B@" + commit,
			kind: RefRunnerRepo, source: "Qwen/Qwen3-0.6B@" + commit, modelName: "Qwen3-0.6B",
		},
		{name: "hf repo without commit", raw: "hf.co/Qwen/Qwen3-0.6B", wantErr: "must pin a full 40-character commit"},
		{name: "hf repo short commit", raw: "hf.co/Qwen/Qwen3-0.6B@abc", wantErr: "must pin a full 40-character commit"},
		{name: "url not gguf", raw: "https://example.com/model.bin", wantErr: "must point to a .gguf file"},
		{name: "plain http remote", raw: "http://example.com/model.gguf", wantErr: "must use https"},
		{name: "plain http hugging face", raw: "http://huggingface.co/org/repo/resolve/main/model.gguf", wantErr: "must use https"},
		{
			name: "plain http loopback", raw: "http://127.0.0.1:8000/model.gguf",
			kind: RefRunnerGGUF, source: "http://127.0.0.1:8000/model.gguf", modelName: "model",
		},
		{name: "url with credentials", raw: "https://user:pass@example.com/model.gguf", wantErr: "must not embed credentials"},
		{name: "premade selector", raw: "multi:v1#chat-model", kind: RefImage, image: "ghcr.io/kaito-project/aikit/multi:v1", premade: true},
		{name: "image selector", raw: "ghcr.io/me/multi:v1#chat-model", kind: RefImage, image: "ghcr.io/me/multi:v1"},
		{name: "bad selector", raw: "ghcr.io/me/multi:v1#bad selector", wantErr: "whitespace"},
		{name: "empty selector", raw: "ghcr.io/me/multi:v1#", wantErr: "invalid #model selector"},
		{name: "empty", raw: " ", wantErr: "is empty"},
		{name: "whitespace", raw: "qwen 3:27b", wantErr: "whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseReference(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseReference(%q) error = %v, want %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tt.raw, err)
			}
			if ref.Kind != tt.kind || ref.Image != tt.image || ref.Source != tt.source || ref.ModelName != tt.modelName || ref.Premade != tt.premade {
				t.Fatalf("ParseReference(%q) = %+v", tt.raw, ref)
			}
		})
	}
}

func TestParsePrefixed(t *testing.T) {
	if !HasPrefix(" aikit:qwen3.8:27b") || HasPrefix("gpt-5.4-mini") {
		t.Fatal("HasPrefix mismatch")
	}
	ref, err := ParsePrefixed("aikit:qwen3.8:27b")
	if err != nil || ref.Image != "ghcr.io/kaito-project/aikit/qwen3.8:27b" {
		t.Fatalf("ParsePrefixed = %+v, %v", ref, err)
	}
	if _, err := ParsePrefixed("qwen3.8:27b"); err == nil {
		t.Fatal("ParsePrefixed accepted a value without the prefix")
	}
}

func TestReferenceRedacted(t *testing.T) {
	signed, _ := ParseReference("https://bucket.example.com/m.gguf?X-Amz-Signature=secret#frag")
	image, _ := ParseReference("ghcr.io/org/model:v1#chat")
	if signed.Redacted() != "https://bucket.example.com/m.gguf" || image.Redacted() != "ghcr.io/org/model:v1#chat" {
		t.Fatalf("Redacted = %q, %q", signed.Redacted(), image.Redacted())
	}
}

func TestReferenceUsesHuggingFace(t *testing.T) {
	for raw, want := range map[string]bool{
		"hf.co/org/repo/file.gguf":                               true,
		"https://huggingface.co/org/repo/resolve/main/file.gguf": true,
		"https://cdn-lfs.huggingface.co/org/repo/file.gguf":      true,
		"hf.co/org/repo@" + strings.Repeat("a", 40):              true,
		"https://example.com/huggingface.co/file.gguf":           false,
		"https://huggingface.co.evil.example/org/repo/file.gguf": false,
		"qwen3.8:27b": false,
	} {
		ref, err := ParseReference(raw)
		if err != nil {
			t.Fatalf("ParseReference(%q): %v", raw, err)
		}
		if got := ref.UsesHuggingFace(); got != want {
			t.Fatalf("UsesHuggingFace(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestReferenceDefaultBackend(t *testing.T) {
	image, _ := ParseReference("qwen3.8:27b")
	repo, _ := ParseReference("hf.co/Qwen/Qwen3-0.6B@" + strings.Repeat("c", 40))
	if image.DefaultBackend() != BackendLlamaCPP || repo.DefaultBackend() != BackendVLLMCPP {
		t.Fatalf("default backends = %q, %q", image.DefaultBackend(), repo.DefaultBackend())
	}
	if image.IsRunner() || !repo.IsRunner() {
		t.Fatal("IsRunner mismatch")
	}
}
