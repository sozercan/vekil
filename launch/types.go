// Package launch starts a short-lived Vekil proxy and runs an agent CLI against it.
package launch

import (
	"context"
	"io"
	"os"
	"time"
)

// PolicyModelOwner is the /v1/models owner for policy-routing public IDs.
const PolicyModelOwner = "vekil-policy"

// ModelInfo is the launcher-relevant subset of one /v1/models entry.
type ModelInfo struct {
	ID                               string            `json:"id"`
	Name                             string            `json:"name,omitempty"`
	OwnedBy                          string            `json:"owned_by,omitempty"`
	SupportedEndpoints               []string          `json:"supported_endpoints,omitempty"`
	Capabilities                     ModelCapabilities `json:"capabilities,omitempty"`
	ContextWindow                    *int64            `json:"context_window,omitempty"`
	MaxContextWindow                 *int64            `json:"max_context_window,omitempty"`
	EffectiveContextWindowPercentage int64             `json:"effective_context_window_percent,omitempty"`
}

// ModelCapabilities contains the bounded model metadata launchers can use to
// configure client-side context, output, reasoning, and tool behavior.
type ModelCapabilities struct {
	Family   string                  `json:"family,omitempty"`
	Limits   ModelCapabilityLimits   `json:"limits,omitempty"`
	Supports ModelCapabilitySupports `json:"supports,omitempty"`
}

// ModelCapabilityLimits contains advertised token limits.
type ModelCapabilityLimits struct {
	MaxContextWindowTokens int64 `json:"max_context_window_tokens,omitempty"`
	MaxPromptTokens        int64 `json:"max_prompt_tokens,omitempty"`
	MaxOutputTokens        int64 `json:"max_output_tokens,omitempty"`
}

// ModelCapabilitySupports contains advertised client-relevant features.
type ModelCapabilitySupports struct {
	ReasoningEffort   []string `json:"reasoning_effort,omitempty"`
	ParallelToolCalls bool     `json:"parallel_tool_calls,omitempty"`
	Vision            bool     `json:"vision,omitempty"`
}

// PrepareInput contains the resolved values an agent adapter needs to construct
// its child process. BaseURL is the root Vekil URL without a trailing slash.
type PrepareInput struct {
	BaseURL       string
	Model         ModelInfo
	Binary        string
	ForwardedArgs []string
	LocalToken    string
	SensitiveEnv  []string
	Environment   []string
	NoProxy       string
	DryRun        bool
}

// PreparedProcess is an agent-specific process plan. EnvSet and EnvUnset are
// applied over a sanitized copy of the parent environment immediately before
// the child is started.
type PreparedProcess struct {
	Path       string
	Args       []string
	EnvSet     map[string]string
	EnvUnset   []string
	Unresolved []string
	Cleanup    func() error
}

// Adapter prepares one supported agent CLI for a Vekil-backed launch.
type Adapter interface {
	Name() string
	Prepare(PrepareInput) (PreparedProcess, error)
}

// Proxy is the lifecycle surface the launcher needs from a ready-capable Vekil
// server. Start must return only after startup authentication and dynamic model
// validation have completed.
type Proxy interface {
	Start(context.Context) error
	Addr() string
	Done() <-chan error
	Stop(context.Context) error
}

// Options configures one launcher run.
type Options struct {
	Model            string
	Binary           string
	LocalToken       string
	ForwardedArgs    []string
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
	ChildStopTimeout time.Duration
	SensitiveEnv     []string
	Environment      []string
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	LogPath          string
	DryRunBaseURL    string
	DryRunModel      *ModelInfo
	Signals          <-chan os.Signal
	DryRun           bool
	NoSummary        bool
}

// Result is the completed launcher outcome. A non-zero child exit is represented
// by ExitCode rather than an error so callers can preserve the agent's status.
type Result struct {
	ExitCode int
	BaseURL  string
	Model    ModelInfo
}
