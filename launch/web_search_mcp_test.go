package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type webSearchRecordingAdapter struct {
	recordingAdapter
}

func (*webSearchRecordingAdapter) usesWebSearchMCP() bool { return true }

func TestRunProbesWebSearchMCPOnlyForConsumingAdapters(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	for _, tc := range []struct {
		name              string
		consumer          bool
		status            int
		body              string
		wantProbes        int32
		wantURL           bool
		wantCopilotAbsent bool
		wantWarning       bool
	}{
		{name: "offered", consumer: true, status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"web_search"}]}}`, wantProbes: 1, wantURL: true},
		{name: "withheld", consumer: true, status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, wantProbes: 1},
		{name: "not copilot", consumer: true, status: http.StatusNotFound, body: `{}`, wantProbes: 1, wantCopilotAbsent: true},
		{name: "upstream failure", consumer: true, status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"GitHub MCP initialize returned HTTP 401"}}`, wantProbes: 1, wantWarning: true},
		{name: "non-consumer", consumer: false, status: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"web_search"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes atomic.Int32
			proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/readyz":
					_, _ = w.Write([]byte(`{"status":"ready"}`))
				case "/mcp/web-search":
					probes.Add(1)
					if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-token-placeholder" {
						t.Errorf("probe %s with Authorization %q", r.Method, r.Header.Get("Authorization"))
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				default:
					http.NotFound(w, r)
				}
			}))
			recording := recordingAdapter{binary: binary, args: []string{"-test.run=TestLaunchHelperProcess"}}
			var adapter Adapter = &recording
			inputOf := func() PrepareInput { return recording.input }
			if tc.consumer {
				consumer := &webSearchRecordingAdapter{recordingAdapter: recording}
				adapter = consumer
				inputOf = func() PrepareInput { return consumer.input }
			}
			var stderr bytes.Buffer
			result, err := Run(context.Background(), proxy, adapter, Options{
				LocalToken:     "test-token-placeholder",
				Environment:    []string{"GO_WANT_LAUNCH_HELPER=1", "LAUNCH_HELPER_CAPTURE=" + t.TempDir() + "/child.json"},
				StartupTimeout: 5 * time.Second,
				Stderr:         &stderr,
				Stdout:         &bytes.Buffer{},
				NoSummary:      true,
			})
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("Run() = %+v, %v", result, err)
			}
			if got := probes.Load(); got != tc.wantProbes {
				t.Fatalf("probes = %d, want %d", got, tc.wantProbes)
			}
			gotURL := inputOf().WebSearchMCPURL
			if wantURL := result.BaseURL + "/mcp/web-search"; tc.wantURL != (gotURL == wantURL) || (!tc.wantURL && gotURL != "") {
				t.Fatalf("WebSearchMCPURL = %q, want set=%v", gotURL, tc.wantURL)
			}
			if got := inputOf().CopilotAbsent; got != tc.wantCopilotAbsent {
				t.Fatalf("CopilotAbsent = %v, want %v", got, tc.wantCopilotAbsent)
			}
			if got := strings.Contains(stderr.String(), "vekil: warning: web search unavailable: GitHub MCP initialize returned HTTP 401"); got != tc.wantWarning {
				t.Fatalf("warning present = %v, want %v; stderr:\n%s", got, tc.wantWarning, stderr.String())
			}
		})
	}
}

func TestRunWebSearchProbeDoesNotSpendStartupDeadline(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	proxy := newFakeProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/mcp/web-search":
			time.Sleep(400 * time.Millisecond)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"web_search"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	adapter := &webSearchRecordingAdapter{recordingAdapter{binary: binary, args: []string{"-test.run=TestLaunchHelperProcess"}}}
	result, err := Run(context.Background(), proxy, adapter, Options{
		LocalToken:     "test-token-placeholder",
		Environment:    []string{"GO_WANT_LAUNCH_HELPER=1", "LAUNCH_HELPER_CAPTURE=" + t.TempDir() + "/child.json"},
		StartupTimeout: 200 * time.Millisecond,
		Stderr:         &bytes.Buffer{},
		Stdout:         &bytes.Buffer{},
		NoSummary:      true,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %+v, %v; a slow optional probe must not abort startup", result, err)
	}
	if adapter.input.WebSearchMCPURL == "" {
		t.Fatal("slow probe result was dropped")
	}
}

type aliasResolvingProxy struct {
	*fakeProxy
	asked [][2]string
}

func (p *aliasResolvingProxy) MessagesAliasResolvesTo(_ context.Context, alias, model string) bool {
	p.asked = append(p.asked, [2]string{alias, model})
	return alias == "claude-public-alias"
}

func TestRunLetsAdapterConfirmModelAliasesWithProxy(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	proxy := &aliasResolvingProxy{fakeProxy: launcherTestProxy()}
	adapter := &recordingAdapter{binary: binary, args: []string{"-test.run=TestLaunchHelperProcess"}}
	result, err := Run(context.Background(), proxy, adapter, Options{
		Model:       "claude-public",
		LocalToken:  "test-token-placeholder",
		Environment: []string{"GO_WANT_LAUNCH_HELPER=1", "LAUNCH_HELPER_CAPTURE=" + t.TempDir() + "/child.json"},
		Stderr:      &bytes.Buffer{},
		Stdout:      &bytes.Buffer{},
		NoSummary:   true,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	routesLikeModel := adapter.input.RoutesLikeModel
	if routesLikeModel == nil {
		t.Fatal("RoutesLikeModel is nil for a proxy that can resolve aliases")
	}
	if !routesLikeModel("claude-public-alias") || routesLikeModel("other") {
		t.Fatal("RoutesLikeModel did not return the proxy's answers")
	}
	if len(proxy.asked) != 2 || proxy.asked[0] != [2]string{"claude-public-alias", "claude-public"} {
		t.Fatalf("proxy asked %v, want aliases checked against the pinned model", proxy.asked)
	}
}

func TestClaudeAdapterDeniesWebSearchUnlessNativeMessagesWithoutCopilot(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	for _, tc := range []struct {
		name          string
		model         ModelInfo
		copilotAbsent bool
		wantDeny      bool
	}{
		{name: "copilot", model: ModelInfo{ID: "claude-opus-5.5", SupportedEndpoints: []string{"/v1/messages"}}, wantDeny: true},
		{name: "native messages without copilot", model: ModelInfo{ID: "claude-opus-5-5", SupportedEndpoints: []string{"/v1/messages"}}, copilotAbsent: true},
		{name: "chat translation without copilot", model: ModelInfo{ID: "gpt-5.4", SupportedEndpoints: []string{"/chat/completions"}}, copilotAbsent: true, wantDeny: true},
		{name: "unpinned without copilot", copilotAbsent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
				BaseURL:       "http://127.0.0.1:43210",
				Model:         tc.model,
				Binary:        binary,
				LocalToken:    "test-token-placeholder",
				CopilotAbsent: tc.copilotAbsent,
				DryRun:        true,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			t.Cleanup(func() { _ = prepared.Cleanup() })
			body, err := os.ReadFile(prepared.Args[1])
			if err != nil {
				t.Fatalf("read managed settings: %v", err)
			}
			var settings struct {
				Permissions struct {
					Deny []string `json:"deny"`
				} `json:"permissions"`
			}
			if err := json.Unmarshal(body, &settings); err != nil {
				t.Fatalf("decode managed settings: %v", err)
			}
			if got := len(settings.Permissions.Deny) == 1 && settings.Permissions.Deny[0] == "WebSearch"; got != tc.wantDeny {
				t.Fatalf("permissions.deny = %v, want WebSearch denied = %v", settings.Permissions.Deny, tc.wantDeny)
			}
		})
	}
}

func TestClaudeAdapterRegistersWebSearchMCPServer(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	prepare := func(url string) PreparedProcess {
		t.Helper()
		prepared, err := (ClaudeAdapter{}).Prepare(PrepareInput{
			BaseURL:         "http://127.0.0.1:43210",
			Model:           ModelInfo{ID: "claude-opus-5.5", SupportedEndpoints: []string{"/v1/messages"}},
			Binary:          binary,
			ForwardedArgs:   []string{"fix the bug", "--print"},
			LocalToken:      "test-token-placeholder",
			WebSearchMCPURL: url,
			DryRun:          true,
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		return prepared
	}

	without := prepare("")
	defer func() { _ = without.Cleanup() }()
	for _, arg := range without.Args {
		if strings.HasPrefix(arg, "--mcp-config") {
			t.Fatalf("Args = %#v, want no MCP config without a web search URL", without.Args)
		}
	}

	prepared := prepare("http://127.0.0.1:43210/mcp/web-search")
	args := prepared.Args
	if len(args) != 5 || args[0] != "--settings" || !strings.HasPrefix(args[2], "--mcp-config=") ||
		args[3] != "fix the bug" || args[4] != "--print" {
		t.Fatalf("Args = %#v, want settings, single-value --mcp-config=, then forwarded args", args)
	}
	// writePrivateTempFile's platform-specific permission tests cover privacy.
	settingsPath, mcpPath := args[1], strings.TrimPrefix(args[2], "--mcp-config=")
	body, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read MCP config: %v", err)
	}
	var config struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatalf("decode MCP config: %v", err)
	}
	server, ok := config.MCPServers["vekil"]
	if !ok || len(config.MCPServers) != 1 || server.Type != "http" ||
		server.URL != "http://127.0.0.1:43210/mcp/web-search" ||
		server.Headers["Authorization"] != "Bearer test-token-placeholder" {
		t.Fatalf("MCP config = %s", body)
	}

	if err := prepared.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	for _, path := range []string{settingsPath, mcpPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after cleanup: %v", path, err)
		}
	}
}
