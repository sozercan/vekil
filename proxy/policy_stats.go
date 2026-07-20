package proxy

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// policyStatsMaxProfiles matches the schema-v2 policy-profile limit. One
	// additional fixed "other" row receives observations whose profile label
	// arrives after the cap, so arbitrary labels cannot grow memory.
	policyStatsMaxProfiles = 128
	// policyStatsMaxTrafficBuckets bounds declared request-size/tool-count
	// buckets independently for every retained profile. One additional fixed
	// "other" bucket receives overflow.
	policyStatsMaxTrafficBuckets = 32
	// policyStatsLatencySamples bounds the recent classifier-latency reservoir at
	// the collector, profile, and traffic-bucket levels.
	policyStatsLatencySamples = 128
	// Policy IDs and declared traffic-bucket labels are configuration-derived,
	// but remain bounded and syntax-checked before retention.
	policyStatsLabelMaxLen = 128

	policyStatsOtherProfile           = "other"
	policyStatsOtherTrafficBucket     = "other"
	policyStatsUnknownProfile         = "unknown"
	policyStatsUnspecifiedBucket      = "unspecified"
	policyStatsInvalidLabel           = "invalid"
	policyStatsTrafficBucketPreflight = "preflight"

	policyStatsModeUnknown = "unknown"
	policyStatsModeOff     = "off"
	policyStatsModeObserve = "observe"
	policyStatsModeEnforce = "enforce"

	policyStatsPreflightUnknown     = "unknown"
	policyStatsPreflightNotRequired = "not_required"
	policyStatsPreflightPending     = "pending"
	policyStatsPreflightReady       = "ready"
	policyStatsPreflightFailed      = "failed"

	policyStatsBreakerUnknown  = "unknown"
	policyStatsBreakerClosed   = "closed"
	policyStatsBreakerOpen     = "open"
	policyStatsBreakerHalfOpen = "half_open"

	policyStatsClassifierCompletion  = "completion"
	policyStatsClassifierUnavailable = "unavailable"
	policyStatsClassifierUncertain   = "uncertain"
	policyStatsClassifierAbstain     = "abstain"

	policyStatsTierLightweight = "lightweight"
	policyStatsTierPowerful    = "powerful"

	policyStatsDropReasonNotSampled       = "not_sampled"
	policyStatsDropReasonProfileCapacity  = "profile_capacity"
	policyStatsDropReasonGlobalCapacity   = "global_capacity"
	policyStatsDropReasonBreakerOpen      = "breaker_open"
	policyStatsDropReasonTransport        = "transport"
	policyStatsDropReasonTimeout          = "timeout"
	policyStatsDropReasonCanceled         = "canceled"
	policyStatsDropReasonRateLimited      = "rate_limited"
	policyStatsDropReasonUpstream5xx      = "upstream_5xx"
	policyStatsDropReasonUpstreamRejected = "upstream_rejected"
	policyStatsDropReasonMissingToolCall  = "missing_tool_call"
	policyStatsDropReasonInvalidOutput    = "invalid_output"
	policyStatsDropReasonAbstained        = "abstained"
	policyStatsDropReasonInternal         = "internal"
	policyStatsDropReasonOther            = "other"
)

const policyStatsMaxClassifierLatencyMs = int64(time.Hour / time.Millisecond)

// policyStatsTokenUsage is classifier-only accounting. It contains numbers
// reported by the classifier transport and deliberately has no request text,
// facts, tool arguments, raw output, or rationale fields.
type policyStatsTokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
}

func (u policyStatsTokenUsage) normalized() policyStatsTokenUsage {
	u.InputTokens = policyStatsNonnegative(u.InputTokens)
	u.OutputTokens = policyStatsNonnegative(u.OutputTokens)
	u.TotalTokens = policyStatsNonnegative(u.TotalTokens)
	u.CachedInputTokens = policyStatsNonnegative(u.CachedInputTokens)
	u.ReasoningTokens = policyStatsNonnegative(u.ReasoningTokens)
	if u.TotalTokens == 0 {
		u.TotalTokens = policyStatsSaturatingAdd(u.InputTokens, u.OutputTokens)
	}
	return u
}

func (u policyStatsTokenUsage) isZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 &&
		u.CachedInputTokens == 0 && u.ReasoningTokens == 0
}

func (u *policyStatsTokenUsage) add(other policyStatsTokenUsage) {
	if u == nil {
		return
	}
	other = other.normalized()
	u.InputTokens = policyStatsSaturatingAdd(u.InputTokens, other.InputTokens)
	u.OutputTokens = policyStatsSaturatingAdd(u.OutputTokens, other.OutputTokens)
	u.TotalTokens = policyStatsSaturatingAdd(u.TotalTokens, other.TotalTokens)
	u.CachedInputTokens = policyStatsSaturatingAdd(u.CachedInputTokens, other.CachedInputTokens)
	u.ReasoningTokens = policyStatsSaturatingAdd(u.ReasoningTokens, other.ReasoningTokens)
}

// policyStatsObservation is one additive, content-free metrics update. Callers
// may populate a complete request lifecycle in one observation or emit partial
// observations as stages finish. Every string field is either a bounded label
// or a fixed enum; unknown enum values never become aggregate keys.
type policyStatsObservation struct {
	Profile       string
	TrafficBucket string

	Eligible bool
	Sampled  bool
	Admitted bool

	DropReason        string
	ClassifierOutcome string
	ActualTier        string
	ShadowTier        string

	ClassifierLatency       time.Duration
	ClassifierUsage         policyStatsTokenUsage
	PhysicalClassifierSends int64
}

// policyStatsProfileState is a full replacement of the current operational
// state for one profile. Generation values are retained only when they are
// bounded hexadecimal hashes; invalid values are discarded rather than being
// echoed through a future /stats.json response.
type policyStatsProfileState struct {
	EffectiveMode  string
	PreflightState string
	BreakerState   string

	ConfigGenerationHash     string
	ProfileGenerationHash    string
	ClassifierGenerationHash string
	BinaryGenerationHash     string
}

type policyStatsSnapshot struct {
	Totals   policyStatsMetricsSnapshot   `json:"totals"`
	Profiles []policyStatsProfileSnapshot `json:"profiles"`
}

type policyStatsProfileSnapshot struct {
	Profile          string                              `json:"profile"`
	EffectiveMode    string                              `json:"effective_mode"`
	PreflightState   string                              `json:"preflight_state"`
	BreakerState     string                              `json:"breaker_state"`
	GenerationHashes policyStatsGenerationHashesSnapshot `json:"generation_hashes"`
	Totals           policyStatsMetricsSnapshot          `json:"totals"`
	TrafficBuckets   []policyStatsTrafficBucketSnapshot  `json:"traffic_buckets"`
}

type policyStatsGenerationHashesSnapshot struct {
	Config     string `json:"config_generation,omitempty"`
	Profile    string `json:"profile_generation,omitempty"`
	Classifier string `json:"classifier_generation,omitempty"`
	Binary     string `json:"binary_generation,omitempty"`
}

type policyStatsTrafficBucketSnapshot struct {
	TrafficBucket string                     `json:"traffic_bucket"`
	Metrics       policyStatsMetricsSnapshot `json:"metrics"`
}

type policyStatsMetricsSnapshot struct {
	Eligible int64 `json:"eligible"`
	Sampled  int64 `json:"sampled"`
	Admitted int64 `json:"admitted"`

	DropReasons []policyStatsCountSnapshot    `json:"drop_reasons"`
	Classifier  policyStatsClassifierSnapshot `json:"classifier"`
	ActualTiers policyStatsTierCountsSnapshot `json:"actual_tiers"`
	ShadowTiers policyStatsTierCountsSnapshot `json:"shadow_tiers"`
	Latency     policyStatsLatencySnapshot    `json:"classifier_latency"`

	ClassifierUsage         policyStatsTokenUsage `json:"classifier_usage"`
	PhysicalClassifierSends int64                 `json:"physical_classifier_sends"`
}

type policyStatsCountSnapshot struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// Abstain is a subset of Uncertain: an explicit classifier abstention selects
// the uncertain fallback and increments both fields.
type policyStatsClassifierSnapshot struct {
	Completion  int64 `json:"completion"`
	Unavailable int64 `json:"unavailable"`
	Uncertain   int64 `json:"uncertain"`
	Abstain     int64 `json:"abstain"`
}

type policyStatsTierCountsSnapshot struct {
	Lightweight int64 `json:"lightweight"`
	Powerful    int64 `json:"powerful"`
	Unknown     int64 `json:"unknown"`
}

// Count is cumulative for the process lifetime. Percentiles use only the
// bounded RecentSamples reservoir, while average/min/max are cumulative.
type policyStatsLatencySnapshot struct {
	Count         int64 `json:"count"`
	RecentSamples int   `json:"recent_samples"`
	AvgMs         int64 `json:"avg_ms"`
	MinMs         int64 `json:"min_ms"`
	MaxMs         int64 `json:"max_ms"`
	P50Ms         int64 `json:"p50_ms"`
	P95Ms         int64 `json:"p95_ms"`
	P99Ms         int64 `json:"p99_ms"`
}

// policyStatsCollector is an in-memory, process-lifetime policy metrics
// collector. A single mutex makes a complete observation atomic across global,
// profile, and traffic-bucket views and makes snapshots race-safe.
type policyStatsCollector struct {
	mu       sync.Mutex
	totals   policyStatsMetricsCounter
	profiles map[string]*policyStatsProfileCounter
}

type policyStatsProfileCounter struct {
	state   policyStatsProfileState
	totals  policyStatsMetricsCounter
	buckets map[string]*policyStatsMetricsCounter
}

type policyStatsMetricsCounter struct {
	eligible int64
	sampled  int64
	admitted int64

	dropReasons [policyStatsDropReasonCount]int64
	classifier  policyStatsClassifierSnapshot
	actualTiers policyStatsTierCountsSnapshot
	shadowTiers policyStatsTierCountsSnapshot
	latency     policyStatsLatencyCounter

	classifierUsage         policyStatsTokenUsage
	physicalClassifierSends int64
}

type policyStatsLatencyCounter struct {
	count   int64
	totalMs int64
	minMs   int64
	maxMs   int64

	recent []int64
	next   int
	size   int
}

type policyStatsNormalizedObservation struct {
	profile       string
	trafficBucket string

	eligible bool
	sampled  bool
	admitted bool

	dropReason        int
	classifierOutcome int
	actualTier        int
	shadowTier        int

	latencyMs               int64
	classifierUsage         policyStatsTokenUsage
	physicalClassifierSends int64
}

const (
	policyStatsDropReasonNotSampledIndex = iota
	policyStatsDropReasonProfileCapacityIndex
	policyStatsDropReasonGlobalCapacityIndex
	policyStatsDropReasonBreakerOpenIndex
	policyStatsDropReasonTransportIndex
	policyStatsDropReasonTimeoutIndex
	policyStatsDropReasonCanceledIndex
	policyStatsDropReasonRateLimitedIndex
	policyStatsDropReasonUpstream5xxIndex
	policyStatsDropReasonUpstreamRejectedIndex
	policyStatsDropReasonMissingToolCallIndex
	policyStatsDropReasonInvalidOutputIndex
	policyStatsDropReasonAbstainedIndex
	policyStatsDropReasonInternalIndex
	policyStatsDropReasonOtherIndex
	policyStatsDropReasonCount
)

const (
	policyStatsClassifierOutcomeNone = iota
	policyStatsClassifierOutcomeCompletion
	policyStatsClassifierOutcomeUnavailable
	policyStatsClassifierOutcomeUncertain
	policyStatsClassifierOutcomeAbstain
)

const (
	policyStatsTierNone = iota
	policyStatsTierLightweightIndex
	policyStatsTierPowerfulIndex
	policyStatsTierUnknownIndex
)

func emptyPolicyStatsSnapshot() policyStatsSnapshot {
	return policyStatsSnapshot{
		Totals:   (policyStatsMetricsCounter{}).snapshot(),
		Profiles: make([]policyStatsProfileSnapshot, 0),
	}
}

func newPolicyStatsCollector() *policyStatsCollector {
	return &policyStatsCollector{profiles: make(map[string]*policyStatsProfileCounter)}
}

// record atomically adds one content-free observation at the global, profile,
// and traffic-bucket levels. Empty observations are ignored. Invalid enum
// strings are ignored or folded into a fixed "other"/"unknown" counter; they
// are never retained as labels.
func (c *policyStatsCollector) record(observation policyStatsObservation) {
	if c == nil {
		return
	}
	normalized, ok := normalizePolicyStatsObservation(observation)
	if !ok {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profiles == nil {
		c.profiles = make(map[string]*policyStatsProfileCounter)
	}
	profile := c.profileLocked(normalized.profile)
	bucket := profile.bucketLocked(normalized.trafficBucket)
	c.totals.add(normalized)
	profile.totals.add(normalized)
	bucket.add(normalized)
}

// setProfileState replaces the current bounded state for a profile. It may be
// called before the profile has traffic so preflight and breaker health can be
// surfaced independently from request metrics.
func (c *policyStatsCollector) setProfileState(profile string, state policyStatsProfileState) {
	if c == nil {
		return
	}
	profile = normalizePolicyStatsProfileLabel(profile, policyStatsUnknownProfile)
	state = normalizePolicyStatsProfileState(state)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profiles == nil {
		c.profiles = make(map[string]*policyStatsProfileCounter)
	}
	c.profileLocked(profile).state = state
}

// snapshot returns a detached, deterministically ordered view suitable for
// embedding below /stats.json. No returned slice or value aliases collector
// state.
func (c *policyStatsCollector) snapshot() policyStatsSnapshot {
	if c == nil {
		return emptyPolicyStatsSnapshot()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	result := policyStatsSnapshot{
		Totals:   c.totals.snapshot(),
		Profiles: make([]policyStatsProfileSnapshot, 0, len(c.profiles)),
	}
	profileNames := make([]string, 0, len(c.profiles))
	for profile := range c.profiles {
		profileNames = append(profileNames, profile)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		profile := c.profiles[profileName]
		if profile == nil {
			continue
		}
		row := policyStatsProfileSnapshot{
			Profile:          profileName,
			EffectiveMode:    profile.state.EffectiveMode,
			PreflightState:   profile.state.PreflightState,
			BreakerState:     profile.state.BreakerState,
			GenerationHashes: profile.state.generationHashesSnapshot(),
			Totals:           profile.totals.snapshot(),
			TrafficBuckets:   make([]policyStatsTrafficBucketSnapshot, 0, len(profile.buckets)),
		}
		bucketNames := make([]string, 0, len(profile.buckets))
		for bucket := range profile.buckets {
			bucketNames = append(bucketNames, bucket)
		}
		sort.Strings(bucketNames)
		for _, bucketName := range bucketNames {
			metrics := profile.buckets[bucketName]
			if metrics == nil {
				continue
			}
			row.TrafficBuckets = append(row.TrafficBuckets, policyStatsTrafficBucketSnapshot{
				TrafficBucket: bucketName,
				Metrics:       metrics.snapshot(),
			})
		}
		result.Profiles = append(result.Profiles, row)
	}
	return result
}

func (c *policyStatsCollector) profileLocked(profile string) *policyStatsProfileCounter {
	if existing := c.profiles[profile]; existing != nil {
		return existing
	}
	if len(c.profiles) >= policyStatsMaxProfiles && profile != policyStatsOtherProfile {
		profile = policyStatsOtherProfile
		if existing := c.profiles[profile]; existing != nil {
			return existing
		}
	}
	created := &policyStatsProfileCounter{
		state:   normalizePolicyStatsProfileState(policyStatsProfileState{}),
		buckets: make(map[string]*policyStatsMetricsCounter),
	}
	c.profiles[profile] = created
	return created
}

func (p *policyStatsProfileCounter) bucketLocked(bucket string) *policyStatsMetricsCounter {
	if p.buckets == nil {
		p.buckets = make(map[string]*policyStatsMetricsCounter)
	}
	if existing := p.buckets[bucket]; existing != nil {
		return existing
	}
	countedBuckets := len(p.buckets)
	if _, hasPreflight := p.buckets[policyStatsTrafficBucketPreflight]; hasPreflight {
		countedBuckets--
	}
	if bucket != policyStatsTrafficBucketPreflight && countedBuckets >= policyStatsMaxTrafficBuckets && bucket != policyStatsOtherTrafficBucket {
		bucket = policyStatsOtherTrafficBucket
		if existing := p.buckets[bucket]; existing != nil {
			return existing
		}
	}
	created := &policyStatsMetricsCounter{}
	p.buckets[bucket] = created
	return created
}

func (m *policyStatsMetricsCounter) add(observation policyStatsNormalizedObservation) {
	if m == nil {
		return
	}
	if observation.eligible {
		m.eligible = policyStatsIncrement(m.eligible)
	}
	if observation.sampled {
		m.sampled = policyStatsIncrement(m.sampled)
	}
	if observation.admitted {
		m.admitted = policyStatsIncrement(m.admitted)
	}
	if observation.dropReason >= 0 && observation.dropReason < len(m.dropReasons) {
		m.dropReasons[observation.dropReason] = policyStatsIncrement(m.dropReasons[observation.dropReason])
	}
	switch observation.classifierOutcome {
	case policyStatsClassifierOutcomeCompletion:
		m.classifier.Completion = policyStatsIncrement(m.classifier.Completion)
	case policyStatsClassifierOutcomeUnavailable:
		m.classifier.Unavailable = policyStatsIncrement(m.classifier.Unavailable)
	case policyStatsClassifierOutcomeUncertain:
		m.classifier.Uncertain = policyStatsIncrement(m.classifier.Uncertain)
	case policyStatsClassifierOutcomeAbstain:
		m.classifier.Uncertain = policyStatsIncrement(m.classifier.Uncertain)
		m.classifier.Abstain = policyStatsIncrement(m.classifier.Abstain)
	}
	m.actualTiers.add(observation.actualTier)
	m.shadowTiers.add(observation.shadowTier)
	if observation.latencyMs > 0 {
		m.latency.add(observation.latencyMs)
	}
	m.classifierUsage.add(observation.classifierUsage)
	m.physicalClassifierSends = policyStatsSaturatingAdd(m.physicalClassifierSends, observation.physicalClassifierSends)
}

func (m policyStatsMetricsCounter) snapshot() policyStatsMetricsSnapshot {
	dropReasons := make([]policyStatsCountSnapshot, 0, policyStatsDropReasonCount)
	for index, count := range m.dropReasons {
		if count == 0 {
			continue
		}
		dropReasons = append(dropReasons, policyStatsCountSnapshot{
			Label: policyStatsDropReasonLabel(index),
			Count: count,
		})
	}
	return policyStatsMetricsSnapshot{
		Eligible:                m.eligible,
		Sampled:                 m.sampled,
		Admitted:                m.admitted,
		DropReasons:             dropReasons,
		Classifier:              m.classifier,
		ActualTiers:             m.actualTiers,
		ShadowTiers:             m.shadowTiers,
		Latency:                 m.latency.snapshot(),
		ClassifierUsage:         m.classifierUsage,
		PhysicalClassifierSends: m.physicalClassifierSends,
	}
}

func (t *policyStatsTierCountsSnapshot) add(tier int) {
	if t == nil {
		return
	}
	switch tier {
	case policyStatsTierLightweightIndex:
		t.Lightweight = policyStatsIncrement(t.Lightweight)
	case policyStatsTierPowerfulIndex:
		t.Powerful = policyStatsIncrement(t.Powerful)
	case policyStatsTierUnknownIndex:
		t.Unknown = policyStatsIncrement(t.Unknown)
	}
}

func (l *policyStatsLatencyCounter) add(milliseconds int64) {
	if l == nil || milliseconds <= 0 {
		return
	}
	if milliseconds > policyStatsMaxClassifierLatencyMs {
		milliseconds = policyStatsMaxClassifierLatencyMs
	}
	l.count = policyStatsIncrement(l.count)
	l.totalMs = policyStatsSaturatingAdd(l.totalMs, milliseconds)
	if l.minMs == 0 || milliseconds < l.minMs {
		l.minMs = milliseconds
	}
	if milliseconds > l.maxMs {
		l.maxMs = milliseconds
	}
	if l.recent == nil {
		l.recent = make([]int64, policyStatsLatencySamples)
	}
	l.recent[l.next] = milliseconds
	l.next = (l.next + 1) % len(l.recent)
	if l.size < len(l.recent) {
		l.size++
	}
}

func (l policyStatsLatencyCounter) snapshot() policyStatsLatencySnapshot {
	result := policyStatsLatencySnapshot{
		Count:         l.count,
		RecentSamples: l.size,
		MinMs:         l.minMs,
		MaxMs:         l.maxMs,
	}
	if l.count > 0 {
		result.AvgMs = l.totalMs / l.count
	}
	if l.size == 0 {
		return result
	}
	samples := make([]int64, l.size)
	copy(samples, l.recent[:l.size])
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	result.P50Ms = policyStatsPercentile(samples, 0.50)
	result.P95Ms = policyStatsPercentile(samples, 0.95)
	result.P99Ms = policyStatsPercentile(samples, 0.99)
	return result
}

func normalizePolicyStatsObservation(observation policyStatsObservation) (policyStatsNormalizedObservation, bool) {
	normalized := policyStatsNormalizedObservation{
		profile:                 normalizePolicyStatsProfileLabel(observation.Profile, policyStatsUnknownProfile),
		trafficBucket:           normalizePolicyStatsLabel(observation.TrafficBucket, policyStatsUnspecifiedBucket),
		eligible:                observation.Eligible,
		sampled:                 observation.Sampled,
		admitted:                observation.Admitted,
		dropReason:              -1,
		classifierOutcome:       normalizePolicyStatsClassifierOutcome(observation.ClassifierOutcome),
		actualTier:              normalizePolicyStatsTier(observation.ActualTier),
		shadowTier:              normalizePolicyStatsTier(observation.ShadowTier),
		latencyMs:               normalizePolicyStatsLatency(observation.ClassifierLatency),
		classifierUsage:         observation.ClassifierUsage.normalized(),
		physicalClassifierSends: policyStatsNonnegative(observation.PhysicalClassifierSends),
	}
	if strings.TrimSpace(observation.DropReason) != "" {
		normalized.dropReason = normalizePolicyStatsDropReason(observation.DropReason)
	}
	ok := normalized.eligible || normalized.sampled || normalized.admitted ||
		normalized.dropReason >= 0 || normalized.classifierOutcome != policyStatsClassifierOutcomeNone ||
		normalized.actualTier != policyStatsTierNone || normalized.shadowTier != policyStatsTierNone ||
		normalized.latencyMs > 0 || !normalized.classifierUsage.isZero() || normalized.physicalClassifierSends > 0
	return normalized, ok
}

func normalizePolicyStatsProfileState(state policyStatsProfileState) policyStatsProfileState {
	state.EffectiveMode = normalizePolicyStatsMode(state.EffectiveMode)
	state.PreflightState = normalizePolicyStatsPreflightState(state.PreflightState)
	state.BreakerState = normalizePolicyStatsBreakerState(state.BreakerState)
	state.ConfigGenerationHash = normalizePolicyStatsGenerationHash(state.ConfigGenerationHash)
	state.ProfileGenerationHash = normalizePolicyStatsGenerationHash(state.ProfileGenerationHash)
	state.ClassifierGenerationHash = normalizePolicyStatsGenerationHash(state.ClassifierGenerationHash)
	state.BinaryGenerationHash = normalizePolicyStatsGenerationHash(state.BinaryGenerationHash)
	return state
}

func (s policyStatsProfileState) generationHashesSnapshot() policyStatsGenerationHashesSnapshot {
	return policyStatsGenerationHashesSnapshot{
		Config:     s.ConfigGenerationHash,
		Profile:    s.ProfileGenerationHash,
		Classifier: s.ClassifierGenerationHash,
		Binary:     s.BinaryGenerationHash,
	}
}

func normalizePolicyStatsMode(value string) string {
	switch strings.TrimSpace(value) {
	case policyStatsModeOff:
		return policyStatsModeOff
	case policyStatsModeObserve:
		return policyStatsModeObserve
	case policyStatsModeEnforce:
		return policyStatsModeEnforce
	default:
		return policyStatsModeUnknown
	}
}

func normalizePolicyStatsPreflightState(value string) string {
	switch strings.TrimSpace(value) {
	case policyStatsPreflightNotRequired:
		return policyStatsPreflightNotRequired
	case policyStatsPreflightPending:
		return policyStatsPreflightPending
	case policyStatsPreflightReady:
		return policyStatsPreflightReady
	case policyStatsPreflightFailed:
		return policyStatsPreflightFailed
	default:
		return policyStatsPreflightUnknown
	}
}

func normalizePolicyStatsBreakerState(value string) string {
	switch strings.TrimSpace(value) {
	case policyStatsBreakerClosed:
		return policyStatsBreakerClosed
	case policyStatsBreakerOpen:
		return policyStatsBreakerOpen
	case policyStatsBreakerHalfOpen:
		return policyStatsBreakerHalfOpen
	default:
		return policyStatsBreakerUnknown
	}
}

func normalizePolicyStatsClassifierOutcome(value string) int {
	switch strings.TrimSpace(value) {
	case policyStatsClassifierCompletion:
		return policyStatsClassifierOutcomeCompletion
	case policyStatsClassifierUnavailable:
		return policyStatsClassifierOutcomeUnavailable
	case policyStatsClassifierUncertain:
		return policyStatsClassifierOutcomeUncertain
	case policyStatsClassifierAbstain:
		return policyStatsClassifierOutcomeAbstain
	default:
		return policyStatsClassifierOutcomeNone
	}
}

func normalizePolicyStatsTier(value string) int {
	value = strings.TrimSpace(value)
	switch value {
	case "":
		return policyStatsTierNone
	case policyStatsTierLightweight:
		return policyStatsTierLightweightIndex
	case policyStatsTierPowerful:
		return policyStatsTierPowerfulIndex
	default:
		return policyStatsTierUnknownIndex
	}
}

func normalizePolicyStatsDropReason(value string) int {
	switch strings.TrimSpace(value) {
	case policyStatsDropReasonNotSampled:
		return policyStatsDropReasonNotSampledIndex
	case policyStatsDropReasonProfileCapacity:
		return policyStatsDropReasonProfileCapacityIndex
	case policyStatsDropReasonGlobalCapacity:
		return policyStatsDropReasonGlobalCapacityIndex
	case policyStatsDropReasonBreakerOpen:
		return policyStatsDropReasonBreakerOpenIndex
	case policyStatsDropReasonTransport:
		return policyStatsDropReasonTransportIndex
	case policyStatsDropReasonTimeout:
		return policyStatsDropReasonTimeoutIndex
	case policyStatsDropReasonCanceled:
		return policyStatsDropReasonCanceledIndex
	case policyStatsDropReasonRateLimited:
		return policyStatsDropReasonRateLimitedIndex
	case policyStatsDropReasonUpstream5xx:
		return policyStatsDropReasonUpstream5xxIndex
	case policyStatsDropReasonUpstreamRejected:
		return policyStatsDropReasonUpstreamRejectedIndex
	case policyStatsDropReasonMissingToolCall:
		return policyStatsDropReasonMissingToolCallIndex
	case policyStatsDropReasonInvalidOutput:
		return policyStatsDropReasonInvalidOutputIndex
	case policyStatsDropReasonAbstained:
		return policyStatsDropReasonAbstainedIndex
	case policyStatsDropReasonInternal:
		return policyStatsDropReasonInternalIndex
	default:
		return policyStatsDropReasonOtherIndex
	}
}

func policyStatsDropReasonLabel(index int) string {
	switch index {
	case policyStatsDropReasonNotSampledIndex:
		return policyStatsDropReasonNotSampled
	case policyStatsDropReasonProfileCapacityIndex:
		return policyStatsDropReasonProfileCapacity
	case policyStatsDropReasonGlobalCapacityIndex:
		return policyStatsDropReasonGlobalCapacity
	case policyStatsDropReasonBreakerOpenIndex:
		return policyStatsDropReasonBreakerOpen
	case policyStatsDropReasonTransportIndex:
		return policyStatsDropReasonTransport
	case policyStatsDropReasonTimeoutIndex:
		return policyStatsDropReasonTimeout
	case policyStatsDropReasonCanceledIndex:
		return policyStatsDropReasonCanceled
	case policyStatsDropReasonRateLimitedIndex:
		return policyStatsDropReasonRateLimited
	case policyStatsDropReasonUpstream5xxIndex:
		return policyStatsDropReasonUpstream5xx
	case policyStatsDropReasonUpstreamRejectedIndex:
		return policyStatsDropReasonUpstreamRejected
	case policyStatsDropReasonMissingToolCallIndex:
		return policyStatsDropReasonMissingToolCall
	case policyStatsDropReasonInvalidOutputIndex:
		return policyStatsDropReasonInvalidOutput
	case policyStatsDropReasonAbstainedIndex:
		return policyStatsDropReasonAbstained
	case policyStatsDropReasonInternalIndex:
		return policyStatsDropReasonInternal
	default:
		return policyStatsDropReasonOther
	}
}

func normalizePolicyStatsLatency(latency time.Duration) int64 {
	if latency <= 0 {
		return 0
	}
	milliseconds := latency.Milliseconds()
	if milliseconds == 0 {
		milliseconds = 1
	}
	if milliseconds > policyStatsMaxClassifierLatencyMs {
		return policyStatsMaxClassifierLatencyMs
	}
	return milliseconds
}

func normalizePolicyStatsGenerationHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 8 || len(value) > policyStatsLabelMaxLen {
		return ""
	}
	for index := 0; index < len(value); index++ {
		if !policyStatsIsHex(value[index]) {
			return ""
		}
	}
	return strings.ToLower(value)
}

func normalizePolicyStatsProfileLabel(value, empty string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return empty
	}
	if len(value) > policyStatsLabelMaxLen {
		return policyStatsInvalidLabel
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return policyStatsInvalidLabel
		}
	}
	return value
}

func normalizePolicyStatsLabel(value, empty string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return empty
	}
	if len(value) > policyStatsLabelMaxLen || !policyStatsLabelStart(value[0]) {
		return policyStatsInvalidLabel
	}
	for index := 1; index < len(value); index++ {
		if !policyStatsLabelByte(value[index]) {
			return policyStatsInvalidLabel
		}
	}
	return value
}

func policyStatsLabelStart(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func policyStatsLabelByte(value byte) bool {
	if policyStatsLabelStart(value) {
		return true
	}
	switch value {
	case '.', '_', '-', ':', '/', '+', '=', '|', ',':
		return true
	default:
		return false
	}
}

func policyStatsIsHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func policyStatsPercentile(sorted []int64, quantile float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(quantile*float64(len(sorted)-1) + 0.5)
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func policyStatsIncrement(value int64) int64 {
	return policyStatsSaturatingAdd(value, 1)
}

func policyStatsSaturatingAdd(left, right int64) int64 {
	left = policyStatsNonnegative(left)
	right = policyStatsNonnegative(right)
	const maxInt64 = int64(^uint64(0) >> 1)
	if right > maxInt64-left {
		return maxInt64
	}
	return left + right
}

func policyStatsNonnegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func (c *policyStatsCollector) setBreakerState(profile, state string) {
	if c == nil {
		return
	}
	profile = normalizePolicyStatsProfileLabel(profile, policyStatsUnknownProfile)
	state = normalizePolicyStatsBreakerState(state)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profiles == nil {
		c.profiles = make(map[string]*policyStatsProfileCounter)
	}
	counter := c.profileLocked(profile)
	counter.state.BreakerState = state
}
