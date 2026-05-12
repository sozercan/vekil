package proxy

import "testing"

func TestToolShapesExtractStringArgumentAtConfiguredJSONPointer(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		path      string
		want      string
		wantFound bool
	}{
		{
			name:      "default command",
			args:      `{"command":"grep foo big.log"}`,
			path:      "/command",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "codex cmd",
			args:      `{"cmd":"grep foo big.log"}`,
			path:      "/cmd",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "nested object",
			args:      `{"shell":{"cmd":"grep foo big.log"}}`,
			path:      "/shell/cmd",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "array index",
			args:      `{"commands":["pwd","grep foo big.log"]}`,
			path:      "/commands/1",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "escaped pointer segment",
			args:      `{"shell/cmd":{"name~value":"grep foo big.log"}}`,
			path:      "/shell~1cmd/name~0value",
			want:      "grep foo big.log",
			wantFound: true,
		},
		{
			name:      "missing path",
			args:      `{"command":"grep foo big.log"}`,
			path:      "/cmd",
			wantFound: false,
		},
		{
			name:      "non-string leaf",
			args:      `{"cmd":["grep foo big.log"]}`,
			path:      "/cmd",
			wantFound: false,
		},
		{
			name:      "invalid escape",
			args:      `{"cmd":"grep foo big.log"}`,
			path:      "/cm~2d",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractStringArgumentAtPath(tt.args, tt.path)
			if ok != tt.wantFound {
				t.Fatalf("found = %v, want %v", ok, tt.wantFound)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolShapesReplaceStringArgumentAtConfiguredJSONPointer(t *testing.T) {
	got, ok := replaceStringArgumentAtPath(`{"shell":{"cmd":"grep foo big.log"}}`, "/shell/cmd", "rg foo big.log")
	if !ok {
		t.Fatalf("expected replacement to succeed")
	}
	command, ok := extractStringArgumentAtPath(got, "/shell/cmd")
	if !ok || command != "rg foo big.log" {
		t.Fatalf("expected rewritten nested command, got %q found=%v in %s", command, ok, got)
	}

	got, ok = replaceStringArgumentAtPath(`{"commands":["pwd","grep foo big.log"]}`, "/commands/1", "rg foo big.log")
	if !ok {
		t.Fatalf("expected array replacement to succeed")
	}
	command, ok = extractStringArgumentAtPath(got, "/commands/1")
	if !ok || command != "rg foo big.log" {
		t.Fatalf("expected rewritten array command, got %q found=%v in %s", command, ok, got)
	}

	if _, ok := replaceStringArgumentAtPath(`{"cmd":["grep foo big.log"]}`, "/cmd", "rg foo big.log"); ok {
		t.Fatalf("expected replacement of non-string leaf to fail")
	}
}
