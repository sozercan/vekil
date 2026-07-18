package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeterministicPolicySample(t *testing.T) {
	if deterministicPolicySample("operation", "policy", 0) {
		t.Fatal("rate 0 sampled")
	}
	if deterministicPolicySample("operation", "policy", -1) {
		t.Fatal("negative rate sampled")
	}
	if deterministicPolicySample("operation", "policy", math.NaN()) {
		t.Fatal("NaN rate sampled")
	}
	if !deterministicPolicySample("operation", "policy", 1) || !deterministicPolicySample("operation", "policy", 2) {
		t.Fatal("rate >=1 did not sample")
	}
	first := deterministicPolicySample("operation-123", "policy-a", 0.37)
	for range 100 {
		if got := deterministicPolicySample("operation-123", "policy-a", 0.37); got != first {
			t.Fatalf("deterministicPolicySample() changed from %v to %v", first, got)
		}
	}

	// Length-prefixing makes ambiguous concatenations distinct inputs.
	ambiguousA := deterministicPolicySample("ab", "c", 0.5)
	ambiguousB := deterministicPolicySample("a", "bc", 0.5)
	if ambiguousA == ambiguousB {
		// A single threshold can collide by chance, so compare a set of rates.
		sameAtAllRates := true
		for _, rate := range []float64{0.1, 0.2, 0.3, 0.4, 0.6, 0.7, 0.8, 0.9} {
			if deterministicPolicySample("ab", "c", rate) != deterministicPolicySample("a", "bc", rate) {
				sameAtAllRates = false
				break
			}
		}
		if sameAtAllRates {
			t.Fatal("length-distinct operation/policy pairs produced the same sample position")
		}
	}

	const total = 20_000
	var sampled int
	for index := 0; index < total; index++ {
		if deterministicPolicySample(fmt.Sprintf("operation-%d", index), "policy", 0.25) {
			sampled++
		}
	}
	if sampled < 4_700 || sampled > 5_300 {
		t.Fatalf("sampled %d/%d at rate .25, want a stable approximately uniform bucket", sampled, total)
	}
}

func TestPolicyAdmissionNonBlockingProfileAndGlobalCaps(t *testing.T) {
	admission := newPolicyAdmission(2, 1)
	first, failure := admission.tryAcquire()
	if failure != policyClassifierFailureNone {
		t.Fatalf("first admission failure = %q", failure)
	}
	start := time.Now()
	if lease, failure := admission.tryAcquire(); lease != nil || failure != policyClassifierFailureProfileCapacity {
		t.Fatalf("second admission = (%v, %q), want profile capacity", lease, failure)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("saturated admission blocked for %v", elapsed)
	}
	first.release()
	if lease, failure := admission.tryAcquire(); failure != policyClassifierFailureNone {
		t.Fatalf("admission after release failure = %q", failure)
	} else {
		lease.release()
	}

	globalOne := newPolicyAdmission(1, 1)
	otherProfile := globalOne.forProfile(1)
	profileOneLease, failure := globalOne.tryAcquire()
	if failure != policyClassifierFailureNone {
		t.Fatalf("profile one admission failure = %q", failure)
	}
	if lease, failure := otherProfile.tryAcquire(); lease != nil || failure != policyClassifierFailureGlobalCapacity {
		t.Fatalf("other profile admission = (%v, %q), want global capacity", lease, failure)
	}
	profileOneLease.release()
	// The failed global acquisition must have released its partial profile slot.
	if lease, failure := otherProfile.tryAcquire(); failure != policyClassifierFailureNone {
		t.Fatalf("other profile admission after global release = %q", failure)
	} else {
		lease.release()
	}
}

func TestPolicyAdmissionConcurrentNeverExceedsCaps(t *testing.T) {
	admission := newPolicyAdmission(3, 2)
	otherProfile := admission.forProfile(2)
	var activeGlobal atomic.Int32
	var maxGlobal atomic.Int32
	var activeA atomic.Int32
	var maxA atomic.Int32
	var activeB atomic.Int32
	var maxB atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	run := func(controller *policyAdmission, active, maximum *atomic.Int32) {
		defer wg.Done()
		<-start
		deadline := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(deadline) {
			lease, failure := controller.tryAcquire()
			if failure != policyClassifierFailureNone {
				time.Sleep(time.Microsecond)
				continue
			}
			profileNow := active.Add(1)
			updatePolicyMax(maximum, profileNow)
			globalNow := activeGlobal.Add(1)
			updatePolicyMax(&maxGlobal, globalNow)
			time.Sleep(10 * time.Microsecond)
			activeGlobal.Add(-1)
			active.Add(-1)
			lease.release()
		}
	}
	for range 12 {
		wg.Add(2)
		go run(admission, &activeA, &maxA)
		go run(otherProfile, &activeB, &maxB)
	}
	close(start)
	wg.Wait()
	if maxGlobal.Load() > 3 || maxA.Load() > 2 || maxB.Load() > 2 {
		t.Fatalf("observed max global/A/B = %d/%d/%d", maxGlobal.Load(), maxA.Load(), maxB.Load())
	}
}

func TestPolicyBreakerThresholdCooldownAndHalfOpen(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	breaker := newPolicyBreaker(func() time.Time { return now })
	infrastructure := policyClassifierFailure{Category: policyClassifierFailureTransport, AffectsBreaker: true}

	for attempt := 1; attempt <= policyBreakerFailureThreshold; attempt++ {
		permit, ok := breaker.tryAcquire()
		if !ok {
			t.Fatalf("attempt %d rejected before threshold", attempt)
		}
		permit.recordFailure(infrastructure)
	}
	if permit, ok := breaker.tryAcquire(); ok || permit != nil {
		t.Fatal("breaker admitted while open")
	}

	now = now.Add(policyBreakerCooldown)
	halfOpen, ok := breaker.tryAcquire()
	if !ok || !halfOpen.halfOpen {
		t.Fatalf("half-open probe = (%#v, %v)", halfOpen, ok)
	}
	if permit, ok := breaker.tryAcquire(); ok || permit != nil {
		t.Fatal("second half-open probe admitted")
	}

	// A neutral/content-dependent outcome does not change health; it only
	// releases the one-probe gate so another probe may run.
	halfOpen.releaseNeutral()
	halfOpen, ok = breaker.tryAcquire()
	if !ok || !halfOpen.halfOpen {
		t.Fatal("neutral half-open result did not release probe slot")
	}
	halfOpen.recordFailure(infrastructure)
	if _, ok := breaker.tryAcquire(); ok {
		t.Fatal("failed half-open probe did not reopen breaker")
	}

	now = now.Add(policyBreakerCooldown)
	halfOpen, ok = breaker.tryAcquire()
	if !ok {
		t.Fatal("half-open probe not admitted after second cooldown")
	}
	halfOpen.recordSuccess()
	for range 10 {
		permit, ok := breaker.tryAcquire()
		if !ok {
			t.Fatal("success did not close breaker")
		}
		permit.releaseNeutral()
	}
}

func TestPolicyBreakerRetryAfterImmediateAndCapped(t *testing.T) {
	start := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	now := start
	breaker := newPolicyBreaker(func() time.Time { return now })
	permit, ok := breaker.tryAcquire()
	if !ok {
		t.Fatal("initial permit rejected")
	}
	permit.recordFailure(policyClassifierFailure{
		Category:       policyClassifierFailureRateLimited,
		RetryAfter:     "120",
		AffectsBreaker: true,
	})
	if _, ok := breaker.tryAcquire(); ok {
		t.Fatal("429 Retry-After did not open immediately")
	}
	now = start.Add(59 * time.Second)
	if _, ok := breaker.tryAcquire(); ok {
		t.Fatal("Retry-After cap expired too early")
	}
	now = start.Add(60 * time.Second)
	halfOpen, ok := breaker.tryAcquire()
	if !ok || !halfOpen.halfOpen {
		t.Fatal("Retry-After was not capped at 60 seconds")
	}
	halfOpen.recordSuccess()

	now = start
	breaker = newPolicyBreaker(func() time.Time { return now })
	retryAt := start.Add(45 * time.Second).Format(http.TimeFormat)
	permit, _ = breaker.tryAcquire()
	permit.recordFailure(policyClassifierFailure{Category: policyClassifierFailureRateLimited, RetryAfter: retryAt, AffectsBreaker: true})
	now = start.Add(44 * time.Second)
	if _, ok := breaker.tryAcquire(); ok {
		t.Fatal("HTTP-date Retry-After expired too early")
	}
	now = start.Add(45 * time.Second)
	if probe, ok := breaker.tryAcquire(); !ok {
		t.Fatal("HTTP-date Retry-After did not expire")
	} else {
		probe.recordSuccess()
	}

	for _, invalid := range []string{"", "0", "-1", "1.5", "999x"} {
		if delay, ok := parsePolicyBreakerRetryAfter(invalid, start); ok || delay != 0 {
			t.Errorf("parsePolicyBreakerRetryAfter(%q) = (%v, %v), want invalid", invalid, delay, ok)
		}
	}
}

func TestPolicyBreakerOnlyInfrastructureFailuresAffectState(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	breaker := newPolicyBreaker(func() time.Time { return now })
	infra := policyClassifierFailure{Category: policyClassifierFailureUpstream5xx, AffectsBreaker: true}
	for range policyBreakerFailureThreshold - 1 {
		permit, _ := breaker.tryAcquire()
		permit.recordFailure(infra)
	}
	// Timeout, malformed output, abstention, and user validation are neutral and
	// do not reset or increment the consecutive infrastructure counter.
	for _, neutral := range []policyClassifierFailure{
		{Category: policyClassifierFailureTimeout},
		{Category: policyClassifierFailureInvalidOutput, HTTPAccepted: false},
		{Category: policyClassifierFailureAbstained, HTTPAccepted: true},
		{Category: policyClassifierFailureInternal},
	} {
		permit, ok := breaker.tryAcquire()
		if !ok {
			t.Fatal("neutral outcome unexpectedly found breaker open")
		}
		permit.recordFailure(neutral)
	}
	permit, _ := breaker.tryAcquire()
	permit.recordFailure(infra)
	if _, ok := breaker.tryAcquire(); ok {
		t.Fatal("fifth infrastructure failure did not open after neutral outcomes")
	}

	// A healthy HTTP exchange closes even when the semantic payload was invalid.
	now = now.Add(policyBreakerCooldown)
	probe, ok := breaker.tryAcquire()
	if !ok {
		t.Fatal("half-open probe not admitted")
	}
	probe.recordSuccess()
	permit, ok = breaker.tryAcquire()
	if !ok {
		t.Fatal("successful exchange did not close breaker")
	}
	permit.releaseNeutral()
}

func TestPolicyClassifierRuntimeAdmissionBreakerAndCategories(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	classifier := &policyRuntimeTestClassifier{signals: policyClassifierSignals{
		TurnType: policyTurnTypeExecution, CodeScope: policyCodeScopeFile, RiskLevel: policyRiskLevelLow,
	}}
	admission := newPolicyAdmission(1, 1)
	breaker := newPolicyBreaker(func() time.Time { return now })
	runtime := newPolicyClassifierRuntime(classifier, admission, breaker)

	result := runtime.classify(context.Background(), policyClassifierFacts{})
	if result.Category != policyClassifierResultClassified || !result.Admitted || classifier.calls.Load() != 1 {
		t.Fatalf("first classify = %#v, calls=%d", result, classifier.calls.Load())
	}

	lease, failure := admission.tryAcquire()
	if failure != policyClassifierFailureNone {
		t.Fatalf("test admission setup failure = %q", failure)
	}
	result = runtime.classify(context.Background(), policyClassifierFacts{})
	if result.Category != policyClassifierResultUnavailable || result.Admitted || result.Failure.Category != policyClassifierFailureProfileCapacity || classifier.calls.Load() != 1 {
		t.Fatalf("capacity classify = %#v, calls=%d", result, classifier.calls.Load())
	}
	lease.release()

	classifier.err = invalidPolicyClassifierOutput(errors.New("malformed"))
	result = runtime.classify(context.Background(), policyClassifierFacts{})
	if result.Category != policyClassifierResultUncertain || result.Failure.Category != policyClassifierFailureInvalidOutput {
		t.Fatalf("semantic classify = %#v", result)
	}
	// Semantic uncertainty counted as a healthy exchange and did not open.
	classifier.err = nil
	result = runtime.classify(context.Background(), policyClassifierFacts{})
	if result.Category != policyClassifierResultClassified {
		t.Fatalf("classify after semantic uncertainty = %#v", result)
	}

	classifier.err = newPolicyClassifierError(policyClassifierFailure{Category: policyClassifierFailureTransport, AffectsBreaker: true}, errors.New("dial"))
	for range policyBreakerFailureThreshold {
		result = runtime.classify(context.Background(), policyClassifierFacts{})
		if result.Category != policyClassifierResultUnavailable || result.Failure.Category != policyClassifierFailureTransport {
			t.Fatalf("transport classify = %#v", result)
		}
	}
	callsAtOpen := classifier.calls.Load()
	result = runtime.classify(context.Background(), policyClassifierFacts{})
	if result.Failure.Category != policyClassifierFailureBreakerOpen || classifier.calls.Load() != callsAtOpen {
		t.Fatalf("open breaker classify = %#v, calls=%d/%d", result, classifier.calls.Load(), callsAtOpen)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	result = runtime.classify(canceled, policyClassifierFacts{})
	if result.Category != policyClassifierResultCanceled || result.Admitted || result.Failure.Category != policyClassifierFailureCanceled || classifier.calls.Load() != callsAtOpen {
		t.Fatalf("canceled classify = %#v, calls=%d/%d", result, classifier.calls.Load(), callsAtOpen)
	}
}

func TestPolicyClassifierRuntimeTimeoutDoesNotOpenBreaker(t *testing.T) {
	classifier := &policyRuntimeTestClassifier{err: context.DeadlineExceeded}
	runtime := newPolicyClassifierRuntime(classifier, newPolicyAdmission(1, 1), newPolicyBreaker(nil))
	for attempt := 0; attempt < policyBreakerFailureThreshold*3; attempt++ {
		result := runtime.classify(context.Background(), policyClassifierFacts{})
		if result.Category != policyClassifierResultUnavailable || result.Failure.Category != policyClassifierFailureTimeout {
			t.Fatalf("attempt %d result = %#v", attempt, result)
		}
	}
	if got := classifier.calls.Load(); got != policyBreakerFailureThreshold*3 {
		t.Fatalf("classifier calls = %d, want %d; timeout must not open breaker", got, policyBreakerFailureThreshold*3)
	}
}

type policyRuntimeTestClassifier struct {
	calls   atomic.Int32
	signals policyClassifierSignals
	err     error
}

func (c *policyRuntimeTestClassifier) Classify(context.Context, policyClassifierFacts) (policyClassifierSignals, error) {
	c.calls.Add(1)
	return c.signals, c.err
}

func updatePolicyMax(maximum *atomic.Int32, value int32) {
	for {
		current := maximum.Load()
		if value <= current || maximum.CompareAndSwap(current, value) {
			return
		}
	}
}

func TestPolicyBreakerStalePermitsCannotClearOrShortenCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	breaker := newPolicyBreaker(func() time.Time { return now })
	first, ok := breaker.tryAcquire()
	if !ok {
		t.Fatal("first permit rejected")
	}
	staleSuccess, ok := breaker.tryAcquire()
	if !ok {
		t.Fatal("stale success permit rejected")
	}
	staleFailure, ok := breaker.tryAcquire()
	if !ok {
		t.Fatal("stale failure permit rejected")
	}
	first.recordFailure(policyClassifierFailure{Category: policyClassifierFailureRateLimited, RetryAfter: "60", AffectsBreaker: true})
	staleSuccess.recordSuccess()
	staleFailure.recordFailure(policyClassifierFailure{Category: policyClassifierFailureTransport, AffectsBreaker: true})
	if breaker.state() != policyStatsBreakerOpen {
		t.Fatalf("breaker state = %q, want open", breaker.state())
	}
	now = now.Add(31 * time.Second)
	if _, ok := breaker.tryAcquire(); ok {
		t.Fatal("stale completion shortened 60-second cooldown")
	}
	now = now.Add(30 * time.Second)
	halfOpen, ok := breaker.tryAcquire()
	if !ok || !halfOpen.halfOpen {
		t.Fatal("half-open probe was not admitted after cooldown")
	}
	halfOpen.recordSuccess()
	if breaker.state() != policyStatsBreakerClosed {
		t.Fatalf("breaker state after probe = %q", breaker.state())
	}
}
