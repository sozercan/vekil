package proxy

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The live smoke's CALL_ID_PATTERN is the only thing that would catch a mint-shape regression
// against real Copilot, and it lives in a shell script no Go test would otherwise read. Pinned
// against the minter itself so the two cannot drift: a stricter regex cries wolf, a looser one
// stops catching anything. (Go's RE2 and jq's Oniguruma agree on a pattern this simple.)
func TestSmokeCallIDPatternMatchesTheMinter(t *testing.T) {
	script, err := os.ReadFile("../scripts/live-chat-over-responses-smoke.sh")
	if err != nil {
		t.Fatalf("read smoke script: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^CALL_ID_PATTERN='([^']+)'`).FindSubmatch(script)
	if declared == nil {
		t.Fatal("smoke script no longer declares CALL_ID_PATTERN; this test cannot pin what it does not find")
	}
	pattern, err := regexp.Compile(string(declared[1]))
	if err != nil {
		t.Fatalf("compile %q: %v", declared[1], err)
	}

	// Every upstream shape the minter has an opinion about, including the edges.
	for _, upstream := range []string{
		copilotUpstreamCallID,
		"call_a", "call_" + strings.Repeat("x", 48), "call_" + strings.Repeat("x", 49),
		"call_", "call", "upstream-call-1", "call_a:b", "",
	} {
		minted, embedded := responsesChatReplaySelfDescribingID(upstream)
		if !embedded {
			// Nothing is embedded, so this upstream can only produce the legacy random
			// form, which the pattern is checked against separately below.
			continue
		}
		if !pattern.MatchString(minted) {
			t.Errorf("minter emits %q for upstream %q but the smoke pattern %q rejects it", minted, upstream, declared[1])
		}
	}
	if !pattern.MatchString(responsesChatReplayCallIDPrefix + strings.Repeat("A", 22)) {
		t.Errorf("smoke pattern %q rejects the legacy minted form", declared[1])
	}
	// The other direction: the pattern must not admit what the minter would never emit.
	for _, rejected := range []string{
		"call_vekil_customer_job", "call_vekil_x", "call_vekil_",
		responsesChatReplayCallIDPrefix + "call_" + strings.Repeat("z", 49),
	} {
		if pattern.MatchString(rejected) && !isResponsesChatReplayCallID(rejected) {
			t.Errorf("smoke pattern %q admits %q, which is not a minted replay ID", declared[1], rejected)
		}
	}
}

// Mint and resolve are inverses, and resolve admits only what mint can emit -- that is what
// keeps a legacy random ID from being read as one that carries an upstream ID.
func TestSelfDescribingCallIDRoundTrip(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		upstream string
		embedded bool
	}{
		{name: "live Copilot ID", upstream: copilotUpstreamCallID, embedded: true},
		{name: "marker plus one character", upstream: "call_x", embedded: true},
		{name: "nothing after the marker", upstream: "call_", embedded: false},
		{name: "marker without its separator", upstream: "call", embedded: false},
		{name: "exactly fills Anthropic's limit", upstream: "call_" + strings.Repeat("x", 48), embedded: true},
		{name: "one over Anthropic's limit", upstream: "call_" + strings.Repeat("x", 49), embedded: false},
		{name: "no Copilot marker", upstream: "upstream-call-1", embedded: false},
		{name: "illegal character for Anthropic", upstream: "call_a:b", embedded: false},
		{name: "empty", upstream: "", embedded: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			minted, ok := responsesChatReplaySelfDescribingID(testCase.upstream)
			if ok != testCase.embedded {
				t.Fatalf("mint(%q) embedded = %v, want %v", testCase.upstream, ok, testCase.embedded)
			}
			if !ok {
				return
			}
			if len(minted) > responsesChatReplayMaxIDLength {
				t.Fatalf("minted %q is %d chars, over Anthropic's %d limit", minted, len(minted), responsesChatReplayMaxIDLength)
			}
			if !isResponsesChatReplayCallID(minted) {
				t.Fatalf("minted %q is not recognised as a replay ID", minted)
			}
			if got, ok := responsesChatReplayUpstreamCallID(minted); !ok || got != testCase.upstream {
				t.Fatalf("resolve(%q) = %q, %v, want %q, true", minted, got, ok, testCase.upstream)
			}
			// The minter may still fall back to the random form on a collision, which the
			// accounting cannot see coming, so it must never bill below either shape.
			size := responsesChatReplayMintedIDSize(testCase.upstream)
			if size < len(minted) || size < responsesChatReplayIDLength {
				t.Fatalf("accounted %d bytes for %q, under the %d it mints or the %d the fallback mints",
					size, testCase.upstream, len(minted), responsesChatReplayIDLength)
			}
		})
	}
}

// A random legacy ID must not resolve as self-describing: its 22 base64url characters can
// spell anything, and reading one as an upstream ID would send a fabricated call_id upstream
// while the store still held the real one.
func TestLegacyRandomIDsDoNotResolveAsSelfDescribing(t *testing.T) {
	store := forgottenReplayStore(t)
	callID, _ := selfDescribingFixture(t, store, selfDescribingRoute(), "upstream-call-1")
	if upstream, ok := responsesChatReplayUpstreamCallID(callID); ok {
		t.Fatalf("legacy ID %q resolved to a fabricated upstream ID %q", callID, upstream)
	}
	// The one shape that could be confused: a legacy-length ID whose suffix spells the marker.
	// base64url can spell "call_", so this is a legacy ID the mint can really produce.
	forged := responsesChatReplayCallIDPrefix + "call_" + strings.Repeat("z", 17)
	if len(forged) != responsesChatReplayIDLength {
		t.Fatalf("forged ID is %d chars, want the legacy %d so the two shapes actually collide", len(forged), responsesChatReplayIDLength)
	}
	if upstream, ok := responsesChatReplayUpstreamCallID(forged); ok {
		t.Fatalf("legacy-length ID %q resolved to a fabricated upstream ID %q; the mint must skip this length so the two shapes are disjoint by construction rather than 64^-5 unlikely", forged, upstream)
	}
	// Disjoint by skipping exactly one length, not by narrowing the shape: one character
	// longer is still self-describing, so the guard cannot be passing for a broader reason.
	if _, ok := responsesChatReplayUpstreamCallID(forged + "z"); !ok {
		t.Fatal("one character past the legacy length must still resolve; the guard is meant to skip a length, not a shape")
	}
}

// Widening the recogniser is the part of this change that can break a transcript nobody can
// rewrite, so it is pinned against the exact predicate it replaced: verdicts may only differ
// for IDs the minter could not have produced before, i.e. genuinely self-describing ones.
// The cases below are the literal shapes already used across this package's tests --
// "call_vekil_x", "call_vekil_orphan" and friends -- which a naive `len(id) <= 64` relaxation
// would silently promote onto the replay path.
func TestRecogniserVerdictIsUnchangedForEveryPreExistingIDShape(t *testing.T) {
	// The predicate exactly as it stood at 4a76e29.
	wasReplayCallID := func(id string) bool {
		id = strings.TrimSpace(id)
		if len(id) != responsesChatReplayIDLength || !strings.HasPrefix(id, responsesChatReplayCallIDPrefix) {
			return false
		}
		for _, char := range id[len(responsesChatReplayCallIDPrefix):] {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
				continue
			}
			return false
		}
		return true
	}
	for _, id := range []string{
		"call_vekil_AAAAAAAAAAAAAAAAAAAAAA", // the legacy 33-char form, still recognised
		"call_vekil_x", "call_vekil_y", "call_vekil_a", "call_vekil_b",
		"call_vekil_orphan", "call_vekil_absent", "call_vekil_customer_job",
		"call_vekil_exhaust", "call_vekil_budget", "call_vekil_budgeta", "call_vekil_budgetl",
		"call_vekil_", "call_vekil_call", "call_vekil_notcall_x",
		"call_upstream_1", "upstream-call-1", "", "   ",
		"call_vekil_AAAAAAAAAAAAAAAAAAAAA",   // one short of legacy
		"call_vekil_AAAAAAAAAAAAAAAAAAAAAAA", // one over legacy
		"call_vekil_A:AAAAAAAAAAAAAAAAAAAAA", // legacy length, illegal character
	} {
		_, selfDescribing := responsesChatReplayUpstreamCallID(id)
		if got, want := isResponsesChatReplayCallID(id), wasReplayCallID(id); got != want && !selfDescribing {
			t.Errorf("isResponsesChatReplayCallID(%q) = %v, was %v, and it is not self-describing", id, got, want)
		}
	}
}
