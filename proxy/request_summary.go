package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type requestSummaryContextKey struct{}

// RequestSummary is a mutable per-request summary populated by handlers and
// emitted by the server-level request logging middleware.
type RequestSummary struct {
	mu sync.Mutex

	endpoint          string
	model             string
	provider          string
	providerKind      string
	streamSet         bool
	stream            bool
	upstreamRequestID string
	promptTokens      *int
	completionTokens  *int
	totalTokens       *int
	cachedTokens      *int
	reasoningTokens   *int
	// failureStatus, when non-zero, is an error status the handler observed
	// out-of-band after the HTTP response status was already committed — e.g. a
	// streaming /responses turn that emits response.failed/incomplete after the
	// 200 header was sent. The stats middleware prefers it over the recorded HTTP
	// status so post-commit failures are not counted as successes.
	failureStatus int
}

// WithRequestSummary attaches a mutable request summary to ctx and returns both
// the derived context and the summary pointer. Handlers mutate the pointer in
// place so middleware can observe the final values after the handler returns.
func WithRequestSummary(ctx context.Context) (context.Context, *RequestSummary) {
	if ctx == nil {
		ctx = context.Background()
	}
	if existing := RequestSummaryFromContext(ctx); existing != nil {
		return ctx, existing
	}
	summary := &RequestSummary{}
	return context.WithValue(ctx, requestSummaryContextKey{}, summary), summary
}

// RequestSummaryFromContext returns the mutable request summary attached to ctx,
// if any.
func RequestSummaryFromContext(ctx context.Context) *RequestSummary {
	if ctx == nil {
		return nil
	}
	summary, _ := ctx.Value(requestSummaryContextKey{}).(*RequestSummary)
	return summary
}

func (s *RequestSummary) setRoute(endpoint, model string, stream bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		s.endpoint = endpoint
	}
	if model = strings.TrimSpace(model); model != "" {
		s.model = model
	}
	s.stream = stream
	s.streamSet = true
}

func (s *RequestSummary) setProvider(provider, kind string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider = strings.TrimSpace(provider); provider != "" {
		s.provider = provider
	}
	if kind = strings.TrimSpace(kind); kind != "" {
		s.providerKind = kind
	}
}

func (s *RequestSummary) setUpstreamRequestID(requestID string) {
	if s == nil {
		return
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreamRequestID = requestID
}

func (s *RequestSummary) setOpenAIUsage(usage *models.OpenAIUsage) {
	if s == nil || usage == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptTokens = summaryIntPtr(usage.PromptTokens)
	s.completionTokens = summaryIntPtr(usage.CompletionTokens)
	s.totalTokens = summaryIntPtr(usage.TotalTokens)
	if usage.PromptTokensDetails != nil {
		s.cachedTokens = summaryIntPtr(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails != nil {
		s.reasoningTokens = summaryIntPtr(usage.CompletionTokensDetails.ReasoningTokens)
	}
}

func summaryIntPtr(v int) *int { return &v }

// setFailureStatus records an out-of-band failure status (first one wins) for a
// request whose HTTP status was already committed before the failure was known.
func (s *RequestSummary) setFailureStatus(status int) {
	if s == nil || status == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failureStatus == 0 {
		s.failureStatus = status
	}
}

// FailureStatus returns the out-of-band failure status, or 0 if none. The stats
// middleware prefers a non-zero value over the committed HTTP status.
func (s *RequestSummary) FailureStatus() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failureStatus
}

// observeResponseFailureStatus records an out-of-band failure status onto the
// request summary attached to ctx, if any.
func observeResponseFailureStatus(ctx context.Context, status int) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setFailureStatus(status)
	}
}

// LoggerFields returns the structured fields populated for this request.
func (s *RequestSummary) LoggerFields() []logger.Field {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	fields := make([]logger.Field, 0, 8)
	if s.endpoint != "" {
		fields = append(fields, logger.F("endpoint", s.endpoint))
	}
	if s.model != "" {
		fields = append(fields, logger.F("model", s.model))
	}
	if s.provider != "" {
		fields = append(fields, logger.F("provider", s.provider))
	}
	if s.providerKind != "" {
		fields = append(fields, logger.F("provider_kind", s.providerKind))
	}
	if s.streamSet {
		fields = append(fields, logger.F("stream", s.stream))
	}
	if s.upstreamRequestID != "" {
		fields = append(fields, logger.F("upstream_request_id", s.upstreamRequestID))
	}
	if s.promptTokens != nil {
		fields = append(fields, logger.F("prompt_tokens", *s.promptTokens))
	}
	if s.completionTokens != nil {
		fields = append(fields, logger.F("completion_tokens", *s.completionTokens))
	}
	if s.totalTokens != nil {
		fields = append(fields, logger.F("total_tokens", *s.totalTokens))
	}
	return fields
}

func (h *ProxyHandler) observeRequestSummary(ctx context.Context, endpoint, model string, stream bool, providerEndpoint string) {
	summary := RequestSummaryFromContext(ctx)
	if summary == nil {
		return
	}
	summary.setRoute(endpoint, model, stream)
	lookupModel := strings.TrimSpace(model)
	if providerEndpoint == providerEndpointMessages {
		lookupModel = NormalizeModelName(lookupModel)
	}
	provider, _, _ := h.resolveProviderModel(lookupModel, providerEndpoint)
	if provider == nil {
		return
	}
	summary.setProvider(provider.id, string(provider.kind))
}

func observeOpenAIUsage(ctx context.Context, usage *models.OpenAIUsage) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setOpenAIUsage(usage)
	}
}

func observeUpstreamHeaders(ctx context.Context, headers http.Header) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setUpstreamRequestID(UpstreamRequestID(headers))
	}
}
