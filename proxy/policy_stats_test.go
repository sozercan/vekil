package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPolicyStatsCollectorRecordsObservation(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.setProfileState("coding-economy-policy", policyStatsProfileState{
		EffectiveMode:            policyStatsModeObserve,
		PreflightState:           policyStatsPreflightReady,
		BreakerState:             policyStatsBreakerClosed,
		ConfigGenerationHash:     "AAAAAAAAAAAAAAAA",
		ProfileGenerationHash:    "BBBBBBBBBBBBBBBB",
		ClassifierGenerationHash: "CCCCCCCCCCCCCCCC",
		BinaryGenerationHash:     "DDDDDDDDDDDDDDDD",
	})
	collector.record(policyStatsObservation{
		Profile:                 "coding-economy-policy",
		TrafficBucket:           "bytes_0_4095_tools_0",
		Eligible:                true,
		Sampled:                 true,
		Admitted:                true,
		ClassifierOutcome:       policyStatsClassifierCompletion,
		ActualTier:              policyStatsTierLightweight,
		ShadowTier:              policyStatsTierPowerful,
		ClassifierLatency:       25 * time.Millisecond,
		ClassifierUsage:         policyStatsTokenUsage{InputTokens: 11, OutputTokens: 3},
		PhysicalClassifierSends: 1,
	})

	snapshot := collector.snapshot()
	if got, want := snapshot.Totals.Eligible, int64(1); got != want {
		t.Fatalf("eligible = %d, want %d", got, want)
	}
	if len(snapshot.Profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(snapshot.Profiles))
	}
	profile := snapshot.Profiles[0]
	if profile.Profile != "coding-economy-policy" {
		t.Fatalf("profile = %q", profile.Profile)
	}
	if profile.EffectiveMode != policyStatsModeObserve || profile.PreflightState != policyStatsPreflightReady || profile.BreakerState != policyStatsBreakerClosed {
		t.Fatalf("profile state = %+v", profile)
	}
	if profile.GenerationHashes.Config != "aaaaaaaaaaaaaaaa" || profile.GenerationHashes.Profile != "bbbbbbbbbbbbbbbb" || profile.GenerationHashes.Classifier != "cccccccccccccccc" || profile.GenerationHashes.Binary != "dddddddddddddddd" {
		t.Fatalf("generation hashes = %+v", profile.GenerationHashes)
	}
	if len(profile.TrafficBuckets) != 1 {
		t.Fatalf("traffic buckets = %d, want 1", len(profile.TrafficBuckets))
	}
	metrics := profile.TrafficBuckets[0].Metrics
	if metrics.Eligible != 1 || metrics.Sampled != 1 || metrics.Admitted != 1 {
		t.Fatalf("request counters = %+v", metrics)
	}
	if metrics.Classifier.Completion != 1 || metrics.Classifier.Unavailable != 0 || metrics.Classifier.Uncertain != 0 || metrics.Classifier.Abstain != 0 {
		t.Fatalf("classifier counters = %+v", metrics.Classifier)
	}
	if metrics.ActualTiers.Lightweight != 1 || metrics.ShadowTiers.Powerful != 1 {
		t.Fatalf("tier counters: actual=%+v shadow=%+v", metrics.ActualTiers, metrics.ShadowTiers)
	}
	if metrics.Latency.Count != 1 || metrics.Latency.AvgMs != 25 || metrics.Latency.P95Ms != 25 {
		t.Fatalf("latency = %+v", metrics.Latency)
	}
	if metrics.ClassifierUsage.InputTokens != 11 || metrics.ClassifierUsage.OutputTokens != 3 || metrics.ClassifierUsage.TotalTokens != 14 {
		t.Fatalf("usage = %+v", metrics.ClassifierUsage)
	}
	if metrics.PhysicalClassifierSends != 1 {
		t.Fatalf("physical sends = %d, want 1", metrics.PhysicalClassifierSends)
	}
}

func TestPolicyStatsCollectorAggregatesAndSortsProfilesAndBuckets(t *testing.T) {
	collector := newPolicyStatsCollector()
	observations := []policyStatsObservation{
		{Profile: "profile-z", TrafficBucket: "tools_1_4", Eligible: true, Sampled: true},
		{Profile: "profile-a", TrafficBucket: "tools_5_plus", Eligible: true, Admitted: true},
		{Profile: "profile-a", TrafficBucket: "tools_0", Eligible: true, Sampled: true, Admitted: true},
		{Profile: "profile-a", TrafficBucket: "tools_0", Eligible: true},
	}
	for _, observation := range observations {
		collector.record(observation)
	}

	snapshot := collector.snapshot()
	if snapshot.Totals.Eligible != 4 || snapshot.Totals.Sampled != 2 || snapshot.Totals.Admitted != 2 {
		t.Fatalf("global totals = %+v", snapshot.Totals)
	}
	if len(snapshot.Profiles) != 2 || snapshot.Profiles[0].Profile != "profile-a" || snapshot.Profiles[1].Profile != "profile-z" {
		t.Fatalf("profiles not sorted: %+v", snapshot.Profiles)
	}
	profile := snapshot.Profiles[0]
	if profile.Totals.Eligible != 3 || profile.Totals.Sampled != 1 || profile.Totals.Admitted != 2 {
		t.Fatalf("profile totals = %+v", profile.Totals)
	}
	if len(profile.TrafficBuckets) != 2 || profile.TrafficBuckets[0].TrafficBucket != "tools_0" || profile.TrafficBuckets[1].TrafficBucket != "tools_5_plus" {
		t.Fatalf("traffic buckets not sorted: %+v", profile.TrafficBuckets)
	}
	if got := profile.TrafficBuckets[0].Metrics.Eligible; got != 2 {
		t.Fatalf("tools_0 eligible = %d, want 2", got)
	}
}

func TestPolicyStatsCollectorUsesFixedDropClassifierAndTierEnums(t *testing.T) {
	collector := newPolicyStatsCollector()
	dropReasons := []string{
		policyStatsDropReasonNotSampled,
		policyStatsDropReasonProfileCapacity,
		policyStatsDropReasonGlobalCapacity,
		policyStatsDropReasonBreakerOpen,
		policyStatsDropReasonTransport,
		policyStatsDropReasonTimeout,
		policyStatsDropReasonCanceled,
		policyStatsDropReasonRateLimited,
		policyStatsDropReasonUpstream5xx,
		policyStatsDropReasonUpstreamRejected,
		policyStatsDropReasonMissingToolCall,
		policyStatsDropReasonInvalidOutput,
		policyStatsDropReasonAbstained,
		policyStatsDropReasonInternal,
	}
	for _, reason := range dropReasons {
		collector.record(policyStatsObservation{Profile: "p", TrafficBucket: "b", DropReason: reason})
	}
	collector.record(policyStatsObservation{Profile: "p", TrafficBucket: "b", DropReason: "attacker-controlled-reason"})

	for _, outcome := range []string{
		policyStatsClassifierCompletion,
		policyStatsClassifierUnavailable,
		policyStatsClassifierUncertain,
		policyStatsClassifierAbstain,
		"attacker-controlled-outcome",
	} {
		collector.record(policyStatsObservation{Profile: "p", TrafficBucket: "b", ClassifierOutcome: outcome})
	}
	for _, tier := range []string{policyStatsTierLightweight, policyStatsTierPowerful, "attacker-controlled-tier"} {
		collector.record(policyStatsObservation{Profile: "p", TrafficBucket: "b", ActualTier: tier, ShadowTier: tier})
	}

	metrics := collector.snapshot().Profiles[0].TrafficBuckets[0].Metrics
	if len(metrics.DropReasons) != len(dropReasons)+1 {
		t.Fatalf("drop reasons = %+v", metrics.DropReasons)
	}
	for _, reason := range dropReasons {
		if got := policyStatsCountForLabel(metrics.DropReasons, reason); got != 1 {
			t.Errorf("drop reason %q = %d, want 1", reason, got)
		}
	}
	if got := policyStatsCountForLabel(metrics.DropReasons, policyStatsDropReasonOther); got != 1 {
		t.Errorf("other drop reason = %d, want 1", got)
	}
	if metrics.Classifier.Completion != 1 || metrics.Classifier.Unavailable != 1 || metrics.Classifier.Uncertain != 2 || metrics.Classifier.Abstain != 1 {
		t.Fatalf("classifier counters = %+v", metrics.Classifier)
	}
	if metrics.ActualTiers.Lightweight != 1 || metrics.ActualTiers.Powerful != 1 || metrics.ActualTiers.Unknown != 1 {
		t.Fatalf("actual tiers = %+v", metrics.ActualTiers)
	}
	if metrics.ShadowTiers != metrics.ActualTiers {
		t.Fatalf("shadow tiers = %+v, want %+v", metrics.ShadowTiers, metrics.ActualTiers)
	}
}

func TestPolicyStatsCollectorValidatesProfileStateAndGenerationHashes(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.setProfileState(" p ", policyStatsProfileState{
		EffectiveMode:            "invalid-mode",
		PreflightState:           "invalid-preflight",
		BreakerState:             "invalid-breaker",
		ConfigGenerationHash:     " ABCDEF12 ",
		ProfileGenerationHash:    "not-a-hash",
		ClassifierGenerationHash: "0123456789abcdef0123456789ABCDEF",
		BinaryGenerationHash:     "deadbeef",
	})

	snapshot := collector.snapshot()
	if len(snapshot.Profiles) != 1 || snapshot.Profiles[0].Profile != "p" {
		t.Fatalf("profiles = %+v", snapshot.Profiles)
	}
	profile := snapshot.Profiles[0]
	if profile.EffectiveMode != policyStatsModeUnknown || profile.PreflightState != policyStatsPreflightUnknown || profile.BreakerState != policyStatsBreakerUnknown {
		t.Fatalf("invalid state was retained: %+v", profile)
	}
	if profile.GenerationHashes.Config != "abcdef12" || profile.GenerationHashes.Profile != "" || profile.GenerationHashes.Classifier != "0123456789abcdef0123456789abcdef" || profile.GenerationHashes.Binary != "deadbeef" {
		t.Fatalf("generation hashes = %+v", profile.GenerationHashes)
	}

	validModes := []string{policyStatsModeOff, policyStatsModeObserve, policyStatsModeEnforce}
	validPreflight := []string{policyStatsPreflightNotRequired, policyStatsPreflightPending, policyStatsPreflightReady, policyStatsPreflightFailed}
	validBreakers := []string{policyStatsBreakerClosed, policyStatsBreakerOpen, policyStatsBreakerHalfOpen}
	for index, mode := range validModes {
		state := policyStatsProfileState{
			EffectiveMode:  mode,
			PreflightState: validPreflight[index],
			BreakerState:   validBreakers[index],
		}
		collector.setProfileState("state-"+mode, state)
	}
	byProfile := policyStatsProfilesByName(collector.snapshot().Profiles)
	for index, mode := range validModes {
		profile, ok := byProfile["state-"+mode]
		if !ok {
			t.Fatalf("missing state profile %q", mode)
		}
		if profile.EffectiveMode != mode || profile.PreflightState != validPreflight[index] || profile.BreakerState != validBreakers[index] {
			t.Errorf("state profile %q = %+v", mode, profile)
		}
	}
}

func policyStatsCountForLabel(rows []policyStatsCountSnapshot, label string) int64 {
	for _, row := range rows {
		if row.Label == label {
			return row.Count
		}
	}
	return 0
}

func policyStatsProfilesByName(rows []policyStatsProfileSnapshot) map[string]policyStatsProfileSnapshot {
	result := make(map[string]policyStatsProfileSnapshot, len(rows))
	for _, row := range rows {
		result[row.Profile] = row
	}
	return result
}

func TestPolicyStatsCollectorBoundsProfileAndTrafficBucketCardinality(t *testing.T) {
	t.Run("profiles", func(t *testing.T) {
		collector := newPolicyStatsCollector()
		total := policyStatsMaxProfiles + 9
		for index := 0; index < total; index++ {
			collector.record(policyStatsObservation{
				Profile:       fmt.Sprintf("profile-%03d", index),
				TrafficBucket: "default",
				Eligible:      true,
			})
		}
		snapshot := collector.snapshot()
		if got, wantMax := len(snapshot.Profiles), policyStatsMaxProfiles+1; got > wantMax {
			t.Fatalf("profile cardinality = %d, want <= %d", got, wantMax)
		}
		profiles := policyStatsProfilesByName(snapshot.Profiles)
		overflow, ok := profiles[policyStatsOtherProfile]
		if !ok {
			t.Fatalf("missing %q profile: %+v", policyStatsOtherProfile, snapshot.Profiles)
		}
		if got, want := overflow.Totals.Eligible, int64(total-policyStatsMaxProfiles); got != want {
			t.Fatalf("overflow eligible = %d, want %d", got, want)
		}
		if snapshot.Totals.Eligible != int64(total) {
			t.Fatalf("global eligible = %d, want %d", snapshot.Totals.Eligible, total)
		}
	})

	t.Run("traffic buckets", func(t *testing.T) {
		collector := newPolicyStatsCollector()
		total := policyStatsMaxTrafficBuckets + 7
		for index := 0; index < total; index++ {
			collector.record(policyStatsObservation{
				Profile:       "profile",
				TrafficBucket: fmt.Sprintf("bucket-%03d", index),
				Sampled:       true,
			})
		}
		profile := collector.snapshot().Profiles[0]
		if got, wantMax := len(profile.TrafficBuckets), policyStatsMaxTrafficBuckets+1; got > wantMax {
			t.Fatalf("bucket cardinality = %d, want <= %d", got, wantMax)
		}
		buckets := policyStatsBucketsByName(profile.TrafficBuckets)
		overflow, ok := buckets[policyStatsOtherTrafficBucket]
		if !ok {
			t.Fatalf("missing %q traffic bucket: %+v", policyStatsOtherTrafficBucket, profile.TrafficBuckets)
		}
		if got, want := overflow.Metrics.Sampled, int64(total-policyStatsMaxTrafficBuckets); got != want {
			t.Fatalf("overflow sampled = %d, want %d", got, want)
		}
	})
}

func TestPolicyStatsCollectorValidatesAndBoundsLabels(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.record(policyStatsObservation{
		Profile:       "profile with spaces and RAW-CONTENT-SENTINEL",
		TrafficBucket: "bucket\nRAW-OUTPUT",
		Eligible:      true,
	})
	collector.record(policyStatsObservation{
		Profile:       strings.Repeat("p", policyStatsLabelMaxLen+1),
		TrafficBucket: strings.Repeat("b", policyStatsLabelMaxLen+1),
		Sampled:       true,
	})
	collector.record(policyStatsObservation{
		Profile:       "valid.profile:1",
		TrafficBucket: "bytes=0-4095|tools=1+",
		Admitted:      true,
	})

	snapshot := collector.snapshot()
	profiles := policyStatsProfilesByName(snapshot.Profiles)
	invalid, ok := profiles[policyStatsInvalidLabel]
	if !ok {
		t.Fatalf("missing invalid-label profile: %+v", snapshot.Profiles)
	}
	if invalid.Totals.Eligible != 1 || invalid.Totals.Sampled != 1 {
		t.Fatalf("invalid-label totals = %+v", invalid.Totals)
	}
	if _, ok := policyStatsBucketsByName(invalid.TrafficBuckets)[policyStatsInvalidLabel]; !ok {
		t.Fatalf("invalid bucket missing: %+v", invalid.TrafficBuckets)
	}
	valid, ok := profiles["valid.profile:1"]
	if !ok {
		t.Fatalf("valid profile missing: %+v", snapshot.Profiles)
	}
	if _, ok := policyStatsBucketsByName(valid.TrafficBuckets)["bytes=0-4095|tools=1+"]; !ok {
		t.Fatalf("valid declared bucket missing: %+v", valid.TrafficBuckets)
	}
}

func TestPolicyStatsCollectorLatencySummaryUsesBoundedRecentReservoir(t *testing.T) {
	collector := newPolicyStatsCollector()
	total := policyStatsLatencySamples + 20
	for milliseconds := 1; milliseconds <= total; milliseconds++ {
		collector.record(policyStatsObservation{
			Profile:           "profile",
			TrafficBucket:     "bucket",
			ClassifierLatency: time.Duration(milliseconds) * time.Millisecond,
		})
	}

	latency := collector.snapshot().Profiles[0].TrafficBuckets[0].Metrics.Latency
	if latency.Count != int64(total) || latency.RecentSamples != policyStatsLatencySamples {
		t.Fatalf("latency counts = %+v", latency)
	}
	if latency.AvgMs != 74 || latency.MinMs != 1 || latency.MaxMs != int64(total) {
		t.Fatalf("cumulative latency = %+v", latency)
	}
	if latency.P50Ms != 85 || latency.P95Ms != 142 || latency.P99Ms != 147 {
		t.Fatalf("recent latency percentiles = %+v", latency)
	}
}

func TestPolicyStatsCollectorNormalizesLatencyUsageAndSaturatesCounters(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.record(policyStatsObservation{
		Profile:           "profile",
		TrafficBucket:     policyStatsTrafficBucketPreflight,
		ClassifierLatency: time.Nanosecond,
		ClassifierUsage: policyStatsTokenUsage{
			InputTokens:       -1,
			OutputTokens:      4,
			TotalTokens:       -2,
			CachedInputTokens: -3,
			ReasoningTokens:   2,
		},
		PhysicalClassifierSends: -1,
	})
	collector.record(policyStatsObservation{
		Profile:           "profile",
		TrafficBucket:     policyStatsTrafficBucketPreflight,
		ClassifierLatency: 2 * time.Hour,
		ClassifierUsage: policyStatsTokenUsage{
			InputTokens:  int64(^uint64(0) >> 1),
			OutputTokens: 1,
		},
		PhysicalClassifierSends: int64(^uint64(0) >> 1),
	})
	collector.record(policyStatsObservation{
		Profile:                 "profile",
		TrafficBucket:           policyStatsTrafficBucketPreflight,
		PhysicalClassifierSends: 1,
	})

	metrics := collector.snapshot().Profiles[0].TrafficBuckets[0].Metrics
	const maxInt64 = int64(^uint64(0) >> 1)
	if metrics.Latency.Count != 2 || metrics.Latency.MinMs != 1 || metrics.Latency.MaxMs != policyStatsMaxClassifierLatencyMs {
		t.Fatalf("latency = %+v", metrics.Latency)
	}
	if metrics.ClassifierUsage.InputTokens != maxInt64 || metrics.ClassifierUsage.OutputTokens != 5 || metrics.ClassifierUsage.TotalTokens != maxInt64 || metrics.ClassifierUsage.CachedInputTokens != 0 || metrics.ClassifierUsage.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", metrics.ClassifierUsage)
	}
	if metrics.PhysicalClassifierSends != maxInt64 {
		t.Fatalf("physical sends = %d, want saturation at %d", metrics.PhysicalClassifierSends, maxInt64)
	}
}

func TestPolicyStatsCollectorSnapshotIsDetached(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.record(policyStatsObservation{
		Profile:       "profile",
		TrafficBucket: "bucket",
		Eligible:      true,
		DropReason:    policyStatsDropReasonNotSampled,
	})

	first := collector.snapshot()
	first.Profiles[0].Profile = "mutated"
	first.Profiles[0].TrafficBuckets[0].TrafficBucket = "mutated"
	first.Profiles[0].TrafficBuckets[0].Metrics.DropReasons[0].Label = "mutated"
	first.Profiles = append(first.Profiles, policyStatsProfileSnapshot{Profile: "injected"})

	second := collector.snapshot()
	if len(second.Profiles) != 1 || second.Profiles[0].Profile != "profile" {
		t.Fatalf("collector snapshot was aliased: %+v", second.Profiles)
	}
	bucket := second.Profiles[0].TrafficBuckets[0]
	if bucket.TrafficBucket != "bucket" || bucket.Metrics.DropReasons[0].Label != policyStatsDropReasonNotSampled {
		t.Fatalf("nested snapshot was aliased: %+v", bucket)
	}
}

func TestPolicyStatsCollectorNilZeroValueAndEmptyObservationAreSafe(t *testing.T) {
	var nilCollector *policyStatsCollector
	nilCollector.record(policyStatsObservation{Eligible: true})
	nilCollector.setProfileState("profile", policyStatsProfileState{EffectiveMode: policyStatsModeEnforce})
	if snapshot := nilCollector.snapshot(); snapshot.Profiles == nil || len(snapshot.Profiles) != 0 {
		t.Fatalf("nil snapshot = %+v", snapshot)
	}

	zeroValue := &policyStatsCollector{}
	zeroValue.record(policyStatsObservation{})
	if snapshot := zeroValue.snapshot(); len(snapshot.Profiles) != 0 {
		t.Fatalf("empty observation created rows: %+v", snapshot.Profiles)
	}
	zeroValue.record(policyStatsObservation{Eligible: true})
	snapshot := zeroValue.snapshot()
	if len(snapshot.Profiles) != 1 || snapshot.Profiles[0].Profile != policyStatsUnknownProfile || snapshot.Profiles[0].TrafficBuckets[0].TrafficBucket != policyStatsUnspecifiedBucket {
		t.Fatalf("zero-value collector snapshot = %+v", snapshot)
	}
}

func TestPolicyStatsCollectorSnapshotNeverRetainsContentFieldsOrInvalidValues(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.setProfileState("profile RAW_CONTENT_SENTINEL", policyStatsProfileState{
		EffectiveMode:            "rationale from model",
		ConfigGenerationHash:     "invalid-generation-value",
		ProfileGenerationHash:    "raw classifier output",
		ClassifierGenerationHash: "tool(arguments)",
		BinaryGenerationHash:     "facts from user",
	})
	collector.record(policyStatsObservation{
		Profile:           "profile RAW_CONTENT_SENTINEL",
		TrafficBucket:     "bucket RAW_OUTPUT",
		DropReason:        "rationale-from-model",
		ClassifierOutcome: "classifier-output: powerful",
		ActualTier:        "arguments-from-tool",
	})

	encoded, err := json.Marshal(collector.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(encoded))
	for _, forbidden := range []string{
		"raw_content_sentinel",
		"raw_output",
		"invalid-generation-value",
		"raw classifier output",
		"tool(arguments)",
		"facts from user",
		"rationale-from-model",
		"classifier-output: powerful",
		"arguments-from-tool",
	} {
		if strings.Contains(text, strings.ToLower(forbidden)) {
			t.Errorf("snapshot retained forbidden value %q: %s", forbidden, encoded)
		}
	}
	for _, forbiddenField := range []string{"prompt", "facts", "raw_output", "arguments", "rationale"} {
		if strings.Contains(text, `"`+forbiddenField+`"`) {
			t.Errorf("snapshot exposes forbidden field %q: %s", forbiddenField, encoded)
		}
	}

	observationType := reflect.TypeOf(policyStatsObservation{})
	for index := 0; index < observationType.NumField(); index++ {
		name := strings.ToLower(observationType.Field(index).Name)
		for _, forbidden := range []string{"prompt", "fact", "raw", "argument", "rationale"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("observation exposes content field %q", observationType.Field(index).Name)
			}
		}
	}
}

func TestPolicyStatsCollectorConcurrentUpdatesAndSnapshots(t *testing.T) {
	collector := newPolicyStatsCollector()
	const (
		workers    = 24
		iterations = 500
		profiles   = 6
	)
	const generation = "0123456789abcdef0123456789abcdef"

	stopSnapshots := make(chan struct{})
	snapshotsDone := make(chan struct{})
	go func() {
		defer close(snapshotsDone)
		for {
			select {
			case <-stopSnapshots:
				return
			default:
				_ = collector.snapshot()
				runtime.Gosched()
			}
		}
	}()

	var writers sync.WaitGroup
	writers.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer writers.Done()
			profile := fmt.Sprintf("profile-%d", worker%profiles)
			for iteration := 0; iteration < iterations; iteration++ {
				collector.record(policyStatsObservation{
					Profile:                 profile,
					TrafficBucket:           fmt.Sprintf("bucket-%d", iteration%4),
					Eligible:                true,
					Sampled:                 true,
					Admitted:                true,
					ClassifierOutcome:       policyStatsClassifierCompletion,
					ActualTier:              policyStatsTierLightweight,
					ShadowTier:              policyStatsTierPowerful,
					ClassifierLatency:       2 * time.Millisecond,
					ClassifierUsage:         policyStatsTokenUsage{InputTokens: 2, OutputTokens: 1},
					PhysicalClassifierSends: 1,
				})
				if iteration%25 == 0 {
					collector.setProfileState(profile, policyStatsProfileState{
						EffectiveMode:            policyStatsModeObserve,
						PreflightState:           policyStatsPreflightReady,
						BreakerState:             policyStatsBreakerClosed,
						ConfigGenerationHash:     generation,
						ProfileGenerationHash:    generation,
						ClassifierGenerationHash: generation,
						BinaryGenerationHash:     generation,
					})
				}
			}
		}(worker)
	}
	writers.Wait()
	close(stopSnapshots)
	<-snapshotsDone

	snapshot := collector.snapshot()
	want := int64(workers * iterations)
	if snapshot.Totals.Eligible != want || snapshot.Totals.Sampled != want || snapshot.Totals.Admitted != want {
		t.Fatalf("request totals = %+v, want %d each", snapshot.Totals, want)
	}
	if snapshot.Totals.Classifier.Completion != want || snapshot.Totals.ActualTiers.Lightweight != want || snapshot.Totals.ShadowTiers.Powerful != want {
		t.Fatalf("classifier/tier totals = %+v", snapshot.Totals)
	}
	if snapshot.Totals.Latency.Count != want || snapshot.Totals.Latency.RecentSamples != policyStatsLatencySamples || snapshot.Totals.Latency.AvgMs != 2 {
		t.Fatalf("latency totals = %+v", snapshot.Totals.Latency)
	}
	if snapshot.Totals.ClassifierUsage.InputTokens != 2*want || snapshot.Totals.ClassifierUsage.OutputTokens != want || snapshot.Totals.ClassifierUsage.TotalTokens != 3*want || snapshot.Totals.PhysicalClassifierSends != want {
		t.Fatalf("usage/sends totals = %+v", snapshot.Totals)
	}
	if len(snapshot.Profiles) != profiles {
		t.Fatalf("profiles = %d, want %d", len(snapshot.Profiles), profiles)
	}
	var profileEligible int64
	for _, profile := range snapshot.Profiles {
		profileEligible += profile.Totals.Eligible
		if profile.EffectiveMode != policyStatsModeObserve || profile.PreflightState != policyStatsPreflightReady || profile.BreakerState != policyStatsBreakerClosed {
			t.Errorf("profile state = %+v", profile)
		}
		if len(profile.TrafficBuckets) != 4 {
			t.Errorf("profile %q buckets = %d, want 4", profile.Profile, len(profile.TrafficBuckets))
		}
	}
	if profileEligible != want {
		t.Fatalf("sum(profile eligible) = %d, want %d", profileEligible, want)
	}
}

func policyStatsBucketsByName(rows []policyStatsTrafficBucketSnapshot) map[string]policyStatsTrafficBucketSnapshot {
	result := make(map[string]policyStatsTrafficBucketSnapshot, len(rows))
	for _, row := range rows {
		result[row.TrafficBucket] = row
	}
	return result
}

func TestHandleStatsJSONWithoutPolicyUsesEmptyProfilesArray(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	recorder := httptest.NewRecorder()
	h.HandleStatsJSON(recorder, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
	var payload struct {
		PolicyRouting policyStatsSnapshot `json:"policy_routing"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PolicyRouting.Profiles == nil || len(payload.PolicyRouting.Profiles) != 0 {
		t.Fatalf("policy profiles = %#v, want non-nil empty", payload.PolicyRouting.Profiles)
	}
	if payload.PolicyRouting.Totals.DropReasons == nil || len(payload.PolicyRouting.Totals.DropReasons) != 0 {
		t.Fatalf("policy drop reasons = %#v, want non-nil empty", payload.PolicyRouting.Totals.DropReasons)
	}
}

func TestPolicyStatsZeroValueSetBreakerStateIsSafe(t *testing.T) {
	collector := &policyStatsCollector{}
	collector.setBreakerState("profile", policyStatsBreakerOpen)
	snapshot := collector.snapshot()
	if len(snapshot.Profiles) != 1 || snapshot.Profiles[0].BreakerState != policyStatsBreakerOpen {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestPolicyStatsPreflightBucketDoesNotConsumeTrafficQuota(t *testing.T) {
	collector := newPolicyStatsCollector()
	collector.record(policyStatsObservation{Profile: "profile", TrafficBucket: policyStatsTrafficBucketPreflight, ClassifierOutcome: policyStatsClassifierCompletion})
	for index := 0; index < policyStatsMaxTrafficBuckets; index++ {
		collector.record(policyStatsObservation{Profile: "profile", TrafficBucket: fmt.Sprintf("bucket-%02d", index), Eligible: true})
	}
	snapshot := collector.snapshot()
	if len(snapshot.Profiles) != 1 || len(snapshot.Profiles[0].TrafficBuckets) != policyStatsMaxTrafficBuckets+1 {
		t.Fatalf("traffic buckets = %+v", snapshot.Profiles)
	}
	for _, row := range snapshot.Profiles[0].TrafficBuckets {
		if row.TrafficBucket == policyStatsOtherTrafficBucket {
			t.Fatal("declared bucket was folded into other")
		}
	}
}
