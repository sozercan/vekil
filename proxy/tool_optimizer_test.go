package proxy

import "testing"

func TestValidCommandReplacementAllowsInternalNewlinesAndCarriageReturns(t *testing.T) {
	if !validCommandReplacement("grep foo big.log", "printf 'foo\\nbar'\nrg foo big.log") {
		t.Fatalf("expected replacement with internal newline to be valid")
	}
	if !validCommandReplacement("grep foo big.log", "printf 'foo\\rbar'\rrg foo big.log") {
		t.Fatalf("expected replacement with internal carriage return to be valid")
	}
}

func TestValidCommandReplacementRejectsUnsafeOrNoopValues(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		replacement string
	}{
		{name: "nul", original: "grep foo big.log", replacement: "rg foo\x00 big.log"},
		{name: "trim empty", original: "grep foo big.log", replacement: " \t\n"},
		{name: "unchanged", original: "grep foo big.log", replacement: "  grep foo big.log\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validCommandReplacement(tt.original, tt.replacement) {
				t.Fatalf("expected replacement %q to be invalid", tt.replacement)
			}
		})
	}
}
