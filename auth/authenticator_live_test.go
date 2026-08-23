package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	liveCopilotBaseURL       = "https://api.githubcopilot.com"
	liveCopilotIntegrationID = "vscode-chat"
)

type liveCopilotAuthObservation struct {
	host       string
	path       string
	statusCode int
}

type liveCopilotAuthRecorder struct {
	base http.RoundTripper

	mu           sync.Mutex
	observations []liveCopilotAuthObservation
}

type liveCopilotModel struct {
	ID                 string   `json:"id"`
	SupportedEndpoints []string `json:"supported_endpoints"`
	ModelPickerEnabled *bool    `json:"model_picker_enabled"`
	Policy             struct {
		State string `json:"state"`
	} `json:"policy"`
}

type liveCopilotModelsResponse struct {
	Data []liveCopilotModel `json:"data"`
}

type liveCopilotResponsesResponse struct {
	Object string `json:"object"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func (r *liveCopilotAuthRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil && resp != nil {
		r.mu.Lock()
		r.observations = append(r.observations, liveCopilotAuthObservation{
			host:       req.URL.Host,
			path:       req.URL.Path,
			statusCode: resp.StatusCode,
		})
		r.mu.Unlock()
	}
	return resp, err
}

func (r *liveCopilotAuthRecorder) snapshot() []liveCopilotAuthObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveCopilotAuthObservation(nil), r.observations...)
}

func TestLiveEnvAccessTokenDirectBearer(t *testing.T) {
	if os.Getenv("LIVE_COPILOT_DIRECT_BEARER_TEST") != "1" {
		t.Skip("set LIVE_COPILOT_DIRECT_BEARER_TEST=1 to run the credentialed live check")
	}

	envToken := strings.TrimSpace(os.Getenv("COPILOT_GITHUB_TOKEN"))
	if envToken == "" {
		t.Fatal("COPILOT_GITHUB_TOKEN is required for the credentialed live check")
	}
	if !strings.HasPrefix(envToken, "github_pat_") {
		t.Fatal("COPILOT_GITHUB_TOKEN must be a fine-grained personal access token")
	}
	t.Setenv("COPILOT_GITHUB_TOKEN", envToken)

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	transport := defaultTransport.Clone()
	defer transport.CloseIdleConnections()
	recorder := &liveCopilotAuthRecorder{base: transport}
	tokenDir := t.TempDir()
	a := &Authenticator{
		tokenDir: tokenDir,
		client: &http.Client{
			Transport: recorder,
			Timeout:   30 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	token, err := a.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() live direct bearer failed: %v", err)
	}
	if token != envToken {
		t.Fatal("GetToken() returned a different token instead of the environment bearer")
	}

	cachedToken, err := a.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() cached direct bearer failed: %v", err)
	}
	if cachedToken != envToken {
		t.Fatal("GetToken() returned a different cached token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, liveCopilotBaseURL+"/models", nil)
	if err != nil {
		t.Fatalf("create live Copilot models request: %v", err)
	}
	setLiveCopilotRequestHeaders(req, token)

	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("live Copilot models request failed: %v", err)
	}
	var modelsResponse liveCopilotModelsResponse
	if err := decodeLiveCopilotJSONResponse(resp, &modelsResponse); err != nil {
		t.Fatalf("live Copilot models request failed: %v", err)
	}
	model := selectLiveCopilotResponsesModel(modelsResponse.Data)
	if model == "" {
		t.Fatal("live Copilot catalog did not advertise a /responses model")
	}

	responsesToken, err := a.GetResponsesToken(ctx)
	if err != nil {
		t.Fatalf("GetResponsesToken() live direct bearer failed: %v", err)
	}
	if responsesToken != envToken {
		t.Fatal("GetResponsesToken() exchanged the fine-grained PAT instead of returning it directly")
	}

	requestBody, err := json.Marshal(struct {
		Model           string `json:"model"`
		Input           string `json:"input"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Store           bool   `json:"store"`
		Stream          bool   `json:"stream"`
	}{
		Model:           model,
		Input:           "Reply with one word: OK.",
		MaxOutputTokens: 1024,
		Store:           false,
		Stream:          false,
	})
	if err != nil {
		t.Fatalf("encode live Copilot Responses request: %v", err)
	}
	responsesReq, err := http.NewRequestWithContext(ctx, http.MethodPost, liveCopilotBaseURL+"/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create live Copilot Responses request: %v", err)
	}
	setLiveCopilotRequestHeaders(responsesReq, responsesToken)

	responsesHTTPResp, err := a.client.Do(responsesReq)
	if err != nil {
		t.Fatalf("live Copilot Responses request failed: %v", err)
	}
	var responsesResponse liveCopilotResponsesResponse
	if err := decodeLiveCopilotJSONResponse(responsesHTTPResp, &responsesResponse); err != nil {
		t.Fatalf("live Copilot Responses request failed: %v", err)
	}
	if responsesResponse.Object != "response" || responsesResponse.Status != "completed" {
		t.Fatalf("live Copilot Responses object/status = %q/%q, want response/completed",
			responsesResponse.Object, responsesResponse.Status)
	}
	if !hasLiveCopilotOutputText(responsesResponse) {
		t.Fatal("live Copilot Responses request returned no non-empty output text")
	}

	observations := recorder.snapshot()
	wantObservations := []liveCopilotAuthObservation{
		{host: "api.githubcopilot.com", path: "/models", statusCode: http.StatusOK},
		{host: "api.githubcopilot.com", path: "/responses", statusCode: http.StatusOK},
	}
	if len(observations) != len(wantObservations) {
		t.Fatalf("live Copilot requests = %d, want exactly one models request and one Responses request", len(observations))
	}
	for i := range wantObservations {
		assertLiveCopilotAuthObservation(t, observations[i], wantObservations[i].host,
			wantObservations[i].path, wantObservations[i].statusCode)
	}

	if a.accessToken != envToken || a.copilotToken != envToken {
		t.Fatal("authenticator did not retain the environment token as the in-memory direct bearer")
	}
	if !a.tokenExpiry.After(time.Now()) {
		t.Fatalf("direct-bearer expiry = %v, want a future refresh deadline", a.tokenExpiry)
	}
	for _, name := range []string{"access-token", "api-key.json"} {
		if _, err := os.Stat(filepath.Join(tokenDir, name)); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				t.Fatalf("live direct bearer unexpectedly persisted %s", name)
			}
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

func TestSelectLiveCopilotResponsesModel(t *testing.T) {
	disabled := false
	tests := []struct {
		name   string
		models []liveCopilotModel
		want   string
	}{
		{
			name: "prefers lightweight model",
			models: []liveCopilotModel{
				{ID: "gpt-5.6-sol", SupportedEndpoints: []string{"/responses"}},
				{ID: "gpt-5.6-luna", SupportedEndpoints: []string{"/responses"}},
			},
			want: "gpt-5.6-luna",
		},
		{
			name: "falls back to advertised model",
			models: []liveCopilotModel{
				{ID: "gpt-5.6-luna", SupportedEndpoints: []string{"/responses"}, ModelPickerEnabled: &disabled},
				{ID: "future-responses-model", SupportedEndpoints: []string{"/responses"}},
			},
			want: "future-responses-model",
		},
		{
			name: "skips policy-disabled model",
			models: []liveCopilotModel{
				liveCopilotModelWithPolicyState("gpt-5.6-luna", "disabled"),
				{ID: "future-responses-model", SupportedEndpoints: []string{"/responses"}},
			},
			want: "future-responses-model",
		},
		{
			name: "rejects missing responses support",
			models: []liveCopilotModel{
				{ID: "chat-only", SupportedEndpoints: []string{"/chat/completions"}},
				{ID: "", SupportedEndpoints: []string{"/responses"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectLiveCopilotResponsesModel(tt.models); got != tt.want {
				t.Fatalf("selectLiveCopilotResponsesModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func setLiveCopilotRequestHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("Editor-Version", "vscode/1.95.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("Copilot-Integration-Id", liveCopilotIntegrationID)
	req.Header.Set("X-GitHub-Api-Version", "2025-05-01")
	req.Header.Set("X-Request-Id", uuid.NewString())
	if req.Method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Openai-Intent", "conversation-panel")
	}
}

func decodeLiveCopilotJSONResponse(resp *http.Response, target any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if err != nil {
			return fmt.Errorf("status %d (reading error detail: %w)", resp.StatusCode, err)
		}
		if trimmed := strings.TrimSpace(string(detail)); trimmed != "" {
			return fmt.Errorf("status %d: %s", resp.StatusCode, trimmed)
		}
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(target); err != nil {
		return fmt.Errorf("decoding status 200 response: %w", err)
	}
	return nil
}

func selectLiveCopilotResponsesModel(models []liveCopilotModel) string {
	const responsesEndpoint = "/responses"
	preferred := []string{"gpt-5.6-luna", "gpt-5.4-mini", "gpt-5-mini"}
	available := make(map[string]struct{}, len(models))
	fallback := ""
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" || (model.ModelPickerEnabled != nil && !*model.ModelPickerEnabled) ||
			strings.EqualFold(strings.TrimSpace(model.Policy.State), "disabled") ||
			!slices.Contains(model.SupportedEndpoints, responsesEndpoint) {
			continue
		}
		available[id] = struct{}{}
		if fallback == "" {
			fallback = id
		}
	}
	for _, model := range preferred {
		if _, ok := available[model]; ok {
			return model
		}
	}
	return fallback
}

func liveCopilotModelWithPolicyState(id, state string) liveCopilotModel {
	model := liveCopilotModel{ID: id, SupportedEndpoints: []string{"/responses"}}
	model.Policy.State = state
	return model
}

func hasLiveCopilotOutputText(response liveCopilotResponsesResponse) bool {
	for _, output := range response.Output {
		if output.Type != "message" || output.Role != "assistant" {
			continue
		}
		for _, content := range output.Content {
			if content.Type == "output_text" && strings.TrimSpace(content.Text) != "" {
				return true
			}
		}
	}
	return false
}

func assertLiveCopilotAuthObservation(t *testing.T, got liveCopilotAuthObservation, wantHost, wantPath string, wantStatus int) {
	t.Helper()
	if got.host != wantHost || got.path != wantPath || got.statusCode != wantStatus {
		t.Fatalf("live auth request = host %q path %q status %d, want %s %s status %d",
			got.host, got.path, got.statusCode, wantHost, wantPath, wantStatus)
	}
}
