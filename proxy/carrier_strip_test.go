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
