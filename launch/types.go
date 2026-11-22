package launch

import (
	"context"
	"io"
	"os"
	"time"
)

const PolicyModelOwner = "vekil-policy"

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

type ModelCapabilities struct {
	Family   string                  `json:"family,omitempty"`
	Limits   ModelCapabilityLimits   `json:"limits,omitempty"`
	Supports ModelCapabilitySupports `json:"supports,omitempty"`
}

type ModelCapabilityLimits struct {
	MaxContextWindowTokens int64 `json:"max_context_window_tokens,omitempty"`
	MaxPromptTokens        int64 `json:"max_prompt_tokens,omitempty"`
	MaxPrompt              int64 `json:"max_prompt,omitempty"`
	MaxInputTokens         int64 `json:"max_input_tokens,omitempty"`
	ContextWindow          int64 `json:"context_window,omitempty"`
	ContextWindowTokens    int64 `json:"context_window_tokens,omitempty"`
	MaxOutputTokens        int64 `json:"max_output_tokens,omitempty"`
}

type ModelCapabilitySupports struct {
	ReasoningEffort   []string `json:"reasoning_effort,omitempty"`
	ParallelToolCalls bool     `json:"parallel_tool_calls,omitempty"`
	Vision            bool     `json:"vision,omitempty"`
}

type PrepareInput struct {
	BaseURL         string
	Model           ModelInfo
	Binary          string
	ForwardedArgs   []string
	LocalToken      string
	SensitiveEnv    []string
	Environment     []string
	NoProxy         string
	WebSearchMCPURL string
	CopilotAbsent   bool
	RoutesLikeModel func(alias string) bool
	DryRun          bool
}

type webSearchMCPAdapter interface {
	usesWebSearchMCP() bool
}

type auxiliaryModelAdapter interface {
	auxiliaryModels() []string
}

func AuxiliaryModels(adapter Adapter) []string {
	if auxiliary, ok := adapter.(auxiliaryModelAdapter); ok {
		return auxiliary.auxiliaryModels()
	}
	return nil
}

type messagesAliasProxy interface {
	MessagesAliasResolvesTo(ctx context.Context, alias, model string) bool
}

type PreparedProcess struct {
	Path       string
	Args       []string
	EnvSet     map[string]string
	EnvUnset   []string
	Unresolved []string
	Cleanup    func() error
}

type Adapter interface {
	Name() string
	Prepare(PrepareInput) (PreparedProcess, error)
}

type Proxy interface {
	Start(context.Context) error
	Addr() string
	Done() <-chan error
	Stop(context.Context) error
}

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

type Result struct {
	ExitCode int
	BaseURL  string
	Model    ModelInfo
}
