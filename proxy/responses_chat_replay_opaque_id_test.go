package proxy

import (
	"bytes"
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestResponsesChatReplayIDsAreOpaqueAndFixedWidth(t *testing.T) {
	random := bytes.Repeat([]byte{0x5a}, responsesChatReplayRandomBytes)
	store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{Random: bytes.NewReader(random)})
	t.Cleanup(func() { _ = store.Close() })

	upstreamID := "call_PROVIDER_SECRET_IDENTIFIER"
	published, err := store.Publish(newResponsesChatReplayTestRequest("opaque-id", replayTestCallSpec{
		upstreamID: upstreamID,
		name:       "lookup",
		visible:    `{}`,
	}))
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	want := responsesChatReplayCallIDPrefix + base64.RawURLEncoding.EncodeToString(random)
	got := published.Projection.Calls[0].ID
	if got != want {
		t.Fatalf("minted ID = %q, want deterministic opaque ID %q", got, want)
	}
	if len(got) != responsesChatReplayIDLength || !isResponsesChatReplayCallID(got) {
		t.Fatalf("minted ID %q does not satisfy the fixed-width replay contract", got)
	}
	if strings.Contains(got, upstreamID) || strings.Contains(got, "PROVIDER_SECRET") {
		t.Fatalf("minted ID exposed provider-owned ID %q", got)
	}
}

func TestResponsesChatReplayIDRecognitionIsExact(t *testing.T) {
	valid := responsesChatReplayCallIDPrefix + strings.Repeat("A", 22)
	if !isResponsesChatReplayCallID(valid) {
		t.Fatalf("fixed-width opaque ID %q was not recognised", valid)
	}
	for _, invalid := range []string{
		"call_vekil_customer_job",
		"call_vekil_call_customer_job",
		"call_vekil_v1_call_customer_job_AAAAAAAA",
		"call_vekil_v2_AAAAAAAAAAAAAAA_call_customer_job_AAAA",
		"call_vekil_x",
		"call_vekil_",
		responsesChatReplayCallIDPrefix + strings.Repeat("A", 21),
		responsesChatReplayCallIDPrefix + strings.Repeat("A", 23),
		responsesChatReplayCallIDPrefix + strings.Repeat("A", 21) + ":",
	} {
		if isResponsesChatReplayCallID(invalid) {
			t.Errorf("non-contract ID %q was recognised as replay state", invalid)
		}
	}
}

func TestLiveSmokeCallIDPatternMatchesOpaqueContract(t *testing.T) {
	script, err := os.ReadFile("../scripts/live-chat-over-responses-smoke.sh")
	if err != nil {
		t.Fatalf("read smoke script: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^CALL_ID_PATTERN='([^']+)'`).FindSubmatch(script)
	if declared == nil {
		t.Fatal("smoke script no longer declares CALL_ID_PATTERN")
	}
	pattern, err := regexp.Compile(string(declared[1]))
	if err != nil {
		t.Fatalf("compile %q: %v", declared[1], err)
	}
	valid := responsesChatReplayCallIDPrefix + strings.Repeat("A", 22)
	if !pattern.MatchString(valid) {
		t.Errorf("smoke pattern %q rejects opaque replay ID %q", declared[1], valid)
	}
	for _, invalid := range []string{
		"call_vekil_v2_AAAAAAAAAAAAAAA_call_customer_job_AAAA",
		responsesChatReplayCallIDPrefix + strings.Repeat("A", 21),
		responsesChatReplayCallIDPrefix + strings.Repeat("A", 23),
	} {
		if pattern.MatchString(invalid) {
			t.Errorf("smoke pattern %q admits non-contract ID %q", declared[1], invalid)
		}
	}
}

func TestClaudeCarrierSmokeRestrictsBashToTheHarnessCommand(t *testing.T) {
	script, err := os.ReadFile("../scripts/live-claude-reasoning-carrier-smoke.sh")
	if err != nil {
		t.Fatalf("read Claude carrier smoke: %v", err)
	}
	text := string(script)
	for _, forbidden := range []string{"--dangerously-skip-permissions", "--allowedTools=Bash"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Claude carrier smoke still contains unrestricted permission form %q", forbidden)
		}
	}
	for _, required := range []string{
		`CLAUDE_BASH_ALLOW_RULE="Bash(${EXPECTED_TOOL_COMMAND})"`,
		`--permission-mode dontAsk`,
		`--allowedTools="${CLAUDE_BASH_ALLOW_RULE}"`,
		`-u CLAUDE_CODE_USE_BEDROCK`,
		`-u CLAUDE_CODE_USE_VERTEX`,
		`-u CLAUDE_CODE_USE_FOUNDRY`,
		`PROXY_HOST="$(url_host)"`,
		`PROXY_BASE_URL="http://${PROXY_HOST}:${PROXY_PORT}"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Claude carrier smoke is missing exact permission guard %q", required)
		}
	}
}
