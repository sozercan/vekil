package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// A JSON escape is decoded by the parse but invisible to a byte scan, so the
// `bytes.Contains` fast path this function used to open with let a carrier
// through: the raw body held no literal `vekil1.`, the helper returned early,
// and vekil's own Copilot reasoning was forwarded to Anthropic. The escape can
// sit on any character, so no pre-check over unparsed bytes can be sound.
func TestStripVekilCarrierBlocksSeesEscapedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		signature string
	}{
		{"literal", `vekil1.PAYLOAD`},
		{"escaped first char", `\u0076ekil1.PAYLOAD`},
		{"escaped middle char", `ve\u006bil1.PAYLOAD`},
		{"escaped dot", `vekil1\u002ePAYLOAD`},
		{"whole prefix escaped", `\u0076\u0065\u006b\u0069\u006c\u0031\u002ePAYLOAD`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-x","messages":[` +
				`{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":[` +
				`{"type":"thinking","thinking":"t","signature":"` + tc.signature + `"},` +
				`{"type":"text","text":"keep me"}` +
				`]}]}`)

			sanitized, stripped := stripVekilCarrierBlocks(body)
			if stripped != 1 {
				t.Fatalf("stripped = %d, want 1 -- the carrier reached Anthropic", stripped)
			}
			// The decoded prefix must be gone from the OUTPUT in decoded form, not
			// merely absent as literal bytes.
			var root struct {
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(sanitized, &root); err != nil {
				t.Fatalf("sanitized body is not valid JSON: %v", err)
			}
			var blocks []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Signature string `json:"signature"`
			}
			if err := json.Unmarshal(root.Messages[1].Content, &blocks); err != nil {
				t.Fatalf("decode assistant content: %v", err)
			}
			for _, b := range blocks {
				if strings.HasPrefix(b.Signature, reasoningCarrierPrefix) {
					t.Fatalf("a carrier survived sanitisation: %+v", b)
				}
			}
			if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "keep me" {
				t.Fatalf("non-carrier content was not preserved: %+v", blocks)
			}
		})
	}
}

// A body with no carrier at all must come back untouched, or the fast path's
// removal would have turned every direct-Anthropic request into a rewrite.
func TestStripVekilCarrierBlocksLeavesCleanBodyAlone(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"anthropic-native-sig"}]}]}`)
	sanitized, stripped := stripVekilCarrierBlocks(body)
	if stripped != 0 {
		t.Fatalf("stripped = %d, want 0", stripped)
	}
	if string(sanitized) != string(body) {
		t.Fatalf("clean body was rewritten:\n got %s\nwant %s", sanitized, body)
	}
}

func TestStripVekilCarrierBlocksSeesCaseFoldedAndEscapedFieldNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "messages",
			body: `{"model":"claude-x","Messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "escaped messages",
			body: `{"model":"claude-x","\u004dessages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "escaped structural and block fields",
			body: `{"model":"claude-x","\u006dessages":[{"\u0072ole":"assistant","\u0063ontent":[{"\u0074ype":"thinking","\u0073ignature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "role",
			body: `{"model":"claude-x","messages":[{"Role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "content",
			body: `{"model":"claude-x","messages":[{"role":"assistant","Content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "block fields",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"Type":"thinking","Signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCarrierStrippedAndControlPreserved(t, []byte(tc.body), 1)
		})
	}
}

func TestStripVekilCarrierBlocksSeesEveryDuplicateField(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "case-folded messages siblings",
			body: `{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"Messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}],"root_extra":"root marker"}`,
		},
		{
			name: "exact messages duplicates",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}],"messages":[{"role":"user","content":"later"}],"root_extra":"root marker"}`,
		},
		{
			name: "role siblings",
			body: `{"model":"claude-x","messages":[{"role":"assistant","Role":"user","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}],"message_extra":"message marker"}]}`,
		},
		{
			name: "exact role duplicates",
			body: `{"model":"claude-x","messages":[{"role":"assistant","role":"user","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}],"message_extra":"message marker"}]}`,
		},
		{
			name: "content siblings",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}],"Content":[{"type":"text","text":"later"}],"message_extra":"message marker"}]}`,
		},
		{
			name: "exact content duplicates",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}],"content":[{"type":"text","text":"later"}],"message_extra":"message marker"}]}`,
		},
		{
			name: "type siblings",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","Type":"text","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "exact type duplicates",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","type":"text","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "signature siblings",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD","Signature":"anthropic-native-sig"},{"type":"text","text":"keep me"}]}]}`,
		},
		{
			name: "exact signature duplicates",
			body: `{"model":"claude-x","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD","signature":"anthropic-native-sig"},{"type":"text","text":"keep me"}]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCarrierStrippedAndControlPreserved(t, []byte(tc.body), 1)
		})
	}
}

func TestStripVekilCarrierBlocksPreservesDuplicateMembers(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","signature":"vekil1.PAYLOAD"},{"type":"text","text":"keep me"}],"content":[{"type":"text","text":"later"}]}],"messages":[{"role":"user","content":"again"}]}`)
	sanitized, stripped := stripVekilCarrierBlocks(body)
	if stripped != 1 {
		t.Fatalf("stripped = %d, want 1: %s", stripped, sanitized)
	}
	if got := strings.Count(string(sanitized), `"messages":`); got != 2 {
		t.Fatalf("messages member count = %d, want 2: %s", got, sanitized)
	}
	if got := strings.Count(string(sanitized), `"content":`); got != 3 {
		t.Fatalf("content member count = %d, want 3: %s", got, sanitized)
	}
}

func TestStripVekilCarrierBlocksLeavesAliasedDuplicateCleanBodyAlone(t *testing.T) {
	body := []byte(" { \"Messages\" : [{\"Role\":\"assistant\",\"Content\":[{\"Type\":\"thinking\",\"Signature\":\"anthropic-native-sig\"}]}], \"messages\" : [] }\n")
	sanitized, stripped := stripVekilCarrierBlocks(body)
	if stripped != 0 {
		t.Fatalf("stripped = %d, want 0", stripped)
	}
	if string(sanitized) != string(body) {
		t.Fatalf("clean aliased body was rewritten:\n got %s\nwant %s", sanitized, body)
	}
}

func assertCarrierStrippedAndControlPreserved(t *testing.T, body []byte, wantStripped int) {
	t.Helper()
	sanitized, stripped := stripVekilCarrierBlocks(body)
	if stripped != wantStripped {
		t.Fatalf("stripped = %d, want %d -- body reached the direct provider unchanged: %s", stripped, wantStripped, sanitized)
	}
	if !json.Valid(sanitized) {
		t.Fatalf("sanitized body is not valid JSON: %s", sanitized)
	}
	if strings.Contains(string(sanitized), "PAYLOAD") {
		t.Fatalf("carrier payload survived sanitization: %s", sanitized)
	}
	if !strings.Contains(string(sanitized), "keep me") {
		t.Fatalf("ordinary assistant content was lost: %s", sanitized)
	}
	for _, marker := range []string{"root marker", "message marker"} {
		if strings.Contains(string(body), marker) && !strings.Contains(string(sanitized), marker) {
			t.Fatalf("unknown field %q was lost: %s", marker, sanitized)
		}
	}
}
