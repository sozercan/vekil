package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	policyDefaultGlobalConcurrency  = 32
	policyDefaultProfileConcurrency = 4
	policyBreakerFailureThreshold   = 5
	policyBreakerCooldown           = 30 * time.Second
	policyBreakerMaxRetryAfter      = 60 * time.Second
)

type policyAdmissionPool struct {
	slots chan struct{}
}

func newPolicyAdmissionPool(capacity int) *policyAdmissionPool {
	return &policyAdmissionPool{slots: make(chan struct{}, capacity)}
}

func (p *policyAdmissionPool) tryAcquire() bool {
	if p == nil {
		return false
	}
	select {
	case p.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *policyAdmissionPool) release() {
	if p == nil {
		return
	}
	select {
	case <-p.slots:
	default:
		panic("policy admission release without acquisition")
	}
}

// policyAdmission is scoped to one profile while its global pool may be shared
// by any number of profile-scoped admission values.
type policyAdmission struct {
	global  *policyAdmissionPool
	profile *policyAdmissionPool
}

// newPolicyAdmission creates the first profile-scoped admission controller.
// Additional profiles should use forProfile so they share the same process-wide
// global pool while retaining independent profile capacity.
func newPolicyAdmission(globalCap, profileCap int) *policyAdmission {
	if globalCap <= 0 {
		globalCap = policyDefaultGlobalConcurrency
	}
	if profileCap <= 0 {
		profileCap = policyDefaultProfileConcurrency
	}
	return &policyAdmission{
		global:  newPolicyAdmissionPool(globalCap),
		profile: newPolicyAdmissionPool(profileCap),
	}
}

func (a *policyAdmission) forProfile(profileCap int) *policyAdmission {
	if profileCap <= 0 {
		profileCap = policyDefaultProfileConcurrency
	}
	if a == nil || a.global == nil {
		return newPolicyAdmission(policyDefaultGlobalConcurrency, profileCap)
	}
	return &policyAdmission{global: a.global, profile: newPolicyAdmissionPool(profileCap)}
}

type policyAdmissionLease struct {
	once      sync.Once
	admission *policyAdmission
}

func (a *policyAdmission) tryAcquire() (*policyAdmissionLease, policyClassifierFailureCategory) {
	if a == nil || a.profile == nil || a.global == nil {
		return nil, policyClassifierFailureInternal
	}
	// Acquire the profile slot first as required by the fairness contract. If
	// global admission fails, release the partial acquisition immediately.
	if !a.profile.tryAcquire() {
		return nil, policyClassifierFailureProfileCapacity
	}
	if !a.global.tryAcquire() {
		a.profile.release()
		return nil, policyClassifierFailureGlobalCapacity
	}
	return &policyAdmissionLease{admission: a}, policyClassifierFailureNone
}

func (l *policyAdmissionLease) release() {
	if l == nil || l.admission == nil {
		return
	}
	l.once.Do(func() {
		l.admission.global.release()
		l.admission.profile.release()
	})
}

func deterministicPolicySample(operationID, policyID string, rate float64) bool {
	if math.IsNaN(rate) || rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}

	hash := sha256.New()
	writePolicySamplePart(hash, operationID)
	writePolicySamplePart(hash, policyID)
	sum := hash.Sum(nil)
	// Use the top 53 bits so conversion to float64 is exact and the result is
	// stable across architectures and Go versions.
	value := binary.BigEndian.Uint64(sum[:8]) >> 11
	const denominator = float64(uint64(1) << 53)
	return float64(value)/denominator < rate
}

type policySampleHashWriter interface {
	Write([]byte) (int, error)
}

func writePolicySamplePart(hash policySampleHashWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}

type policyBreaker struct {
	mu sync.Mutex

	now func() time.Time

	consecutiveInfrastructureFailures int
	openUntil                         time.Time
	halfOpenInFlight                  bool
	generation                        uint64
}

func newPolicyBreaker(now func() time.Time) *policyBreaker {
	if now == nil {
		now = time.Now
	}
	return &policyBreaker{now: now}
}

type policyBreakerPermit struct {
	once       sync.Once
	breaker    *policyBreaker
	halfOpen   bool
	generation uint64
}

func (b *policyBreaker) tryAcquire() (*policyBreakerPermit, bool) {
	if b == nil {
		return &policyBreakerPermit{}, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if !b.openUntil.IsZero() {
		if now.Before(b.openUntil) {
			return nil, false
		}
		if b.halfOpenInFlight {
			return nil, false
		}
		b.halfOpenInFlight = true
		return &policyBreakerPermit{breaker: b, halfOpen: true, generation: b.generation}, true
	}
	return &policyBreakerPermit{breaker: b, generation: b.generation}, true
}

func (p *policyBreakerPermit) recordSuccess() {
	if p == nil || p.breaker == nil {
		return
	}
	p.once.Do(func() {
		b := p.breaker
		b.mu.Lock()
		defer b.mu.Unlock()
		if p.generation != b.generation {
			return
		}
		wasOpen := p.halfOpen || !b.openUntil.IsZero()
		b.consecutiveInfrastructureFailures = 0
		b.openUntil = time.Time{}
		b.halfOpenInFlight = false
		if wasOpen {
			b.generation++
		}
	})
}

func (p *policyBreakerPermit) recordFailure(failure policyClassifierFailure) {
	if p == nil || p.breaker == nil {
		return
	}
	if !failure.AffectsBreaker {
		p.releaseNeutral()
		return
	}
	p.once.Do(func() {
		b := p.breaker
		b.mu.Lock()
		defer b.mu.Unlock()

		if p.generation != b.generation {
			return
		}
		now := b.now()
		b.halfOpenInFlight = false
		if failure.Category == policyClassifierFailureRateLimited {
			if delay, ok := parsePolicyBreakerRetryAfter(failure.RetryAfter, now); ok {
				b.openLocked(now.Add(delay))
				return
			}
		}
		if p.halfOpen {
			b.openLocked(now.Add(policyBreakerCooldown))
			return
		}
		b.consecutiveInfrastructureFailures++
		if b.consecutiveInfrastructureFailures >= policyBreakerFailureThreshold {
			b.openLocked(now.Add(policyBreakerCooldown))
		}
	})
}

func (p *policyBreakerPermit) releaseNeutral() {
	if p == nil || p.breaker == nil {
		return
	}
	p.once.Do(func() {
		if !p.halfOpen {
			return
		}
		p.breaker.mu.Lock()
		if p.generation == p.breaker.generation {
			p.breaker.halfOpenInFlight = false
		}
		p.breaker.mu.Unlock()
	})
}

func (b *policyBreaker) openLocked(until time.Time) {
	if b == nil {
		return
	}
	if b.openUntil.IsZero() || until.After(b.openUntil) {
		b.openUntil = until
	}
	b.consecutiveInfrastructureFailures = policyBreakerFailureThreshold
	b.halfOpenInFlight = false
	b.generation++
}

func parsePolicyBreakerRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, ok := parsePolicyRetryAfterSeconds(value); ok {
		delay := time.Duration(seconds) * time.Second
		if delay > policyBreakerMaxRetryAfter {
			delay = policyBreakerMaxRetryAfter
		}
		return delay, delay > 0
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := retryAt.Sub(now)
	if delay <= 0 {
		return 0, false
	}
	if delay > policyBreakerMaxRetryAfter {
		delay = policyBreakerMaxRetryAfter
	}
	return delay, true
}

func parsePolicyRetryAfterSeconds(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	const maxSeconds = int64(policyBreakerMaxRetryAfter / time.Second)
	var parsed int64
	clamped := false
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
		if clamped {
			continue
		}
		digit := int64(value[index] - '0')
		if parsed > (maxSeconds-digit)/10 {
			parsed = maxSeconds
			clamped = true
			continue
		}
		parsed = parsed*10 + digit
		if parsed > maxSeconds {
			parsed = maxSeconds
			clamped = true
		}
	}
	return parsed, parsed > 0
}

type policyClassifierRuntime struct {
	classifier policyClassifier
	admission  *policyAdmission
	breaker    *policyBreaker
}

func newPolicyClassifierRuntime(classifier policyClassifier, admission *policyAdmission, breaker *policyBreaker) *policyClassifierRuntime {
	return &policyClassifierRuntime{classifier: classifier, admission: admission, breaker: breaker}
}

// classify performs queue-free admission and breaker accounting around exactly
// one classifier invocation. Only explicitly typed infrastructure failures may
// affect shared breaker state.
func (r *policyClassifierRuntime) classify(ctx context.Context, facts policyClassifierFacts) policyClassifierResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return newPolicyClassifierResult(policyClassifierSignals{}, err)
	}
	if r == nil || r.classifier == nil {
		return policyClassifierResult{
			Category: policyClassifierResultUnavailable,
			Failure:  policyClassifierFailure{Category: policyClassifierFailureInternal},
		}
	}

	lease, admissionFailure := r.admission.tryAcquire()
	if admissionFailure != policyClassifierFailureNone {
		return policyClassifierResult{
			Category: policyClassifierResultUnavailable,
			Failure:  policyClassifierFailure{Category: admissionFailure},
		}
	}
	defer lease.release()

	permit, ok := r.breaker.tryAcquire()
	if !ok {
		return policyClassifierResult{
			Category: policyClassifierResultUnavailable,
			Failure:  policyClassifierFailure{Category: policyClassifierFailureBreakerOpen},
		}
	}

	signals, err := r.classifier.Classify(ctx, facts)
	result := newPolicyClassifierResult(signals, err)
	result.Admitted = true
	failure := result.Failure
	switch {
	case err == nil:
		permit.recordSuccess()
	case failure.HTTPAccepted:
		// Any successful 2xx classifier exchange proves route health even when
		// the model payload is missing, malformed, or explicitly abstains.
		permit.recordSuccess()
	case failure.AffectsBreaker:
		permit.recordFailure(failure)
	default:
		permit.releaseNeutral()
	}
	return result
}

func (b *policyBreaker) state() string {
	if b == nil {
		return policyStatsBreakerUnknown
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.halfOpenInFlight {
		return policyStatsBreakerHalfOpen
	}
	if !b.openUntil.IsZero() {
		if b.now().Before(b.openUntil) {
			return policyStatsBreakerOpen
		}
		return policyStatsBreakerHalfOpen
	}
	return policyStatsBreakerClosed
}
