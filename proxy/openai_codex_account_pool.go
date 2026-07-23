package proxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultOpenAICodexPoolAffinityEntries = 4096
	defaultOpenAICodexQuotaCooldown       = time.Minute
	openAICodexPoolDiscoveryConcurrency   = 4
)

var (
	errOpenAICodexStaleCredentialGeneration = errors.New("OpenAI Codex credential generation is no longer available")
)

type openAICodexAccountSelectionReason string

const (
	openAICodexSelectionSingle     openAICodexAccountSelectionReason = "single"
	openAICodexSelectionRoundRobin openAICodexAccountSelectionReason = "round_robin"
	openAICodexSelectionFillFirst  openAICodexAccountSelectionReason = "fill_first"
	openAICodexSelectionAffinity   openAICodexAccountSelectionReason = "session_affinity"
	openAICodexSelectionHardState  openAICodexAccountSelectionReason = "hard_state"
	openAICodexSelectionRetry      openAICodexAccountSelectionReason = "same_account_retry"
	openAICodexSelectionFailover   openAICodexAccountSelectionReason = "account_failover"
)

type openAICodexAccountFailureClass uint8

const (
	openAICodexAccountFailureNone openAICodexAccountFailureClass = iota
	openAICodexAccountFailureAuth
	openAICodexAccountFailureQuota
	openAICodexAccountFailureEntitlement
)

type openAICodexAccountLease struct {
	pool            *openAICodexAccountPool
	member          *openAICodexAccountMember
	credentials     openAICodexCredentials
	credentialID    string
	sourceDigest    string
	accessMemberID  string
	selectionReason openAICodexAccountSelectionReason
	affinityKey     [sha256.Size]byte
	hasAffinityKey  bool
	halfOpen        bool
}

func (l *openAICodexAccountLease) authorize(req *http.Request) error {
	if l == nil || req == nil || strings.TrimSpace(l.credentials.accessToken) == "" {
		return fmt.Errorf("OpenAI Codex account lease is not authorized")
	}
	req.Header.Set("Authorization", "Bearer "+l.credentials.accessToken)
	if l.credentials.accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", l.credentials.accountID)
	} else {
		req.Header.Del("ChatGPT-Account-ID")
	}
	if l.credentials.fedRAMP {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	} else {
		req.Header.Del("X-OpenAI-Fedramp")
	}
	return nil
}

type openAICodexAccountCooldown struct {
	until         time.Time
	probeInFlight bool
}

type openAICodexAccountMember struct {
	id       string
	authPath string
	auth     *openAICodexAuth

	credentialID string
	sourceDigest string
	accountID    string
	fedRAMP      bool

	quarantined       bool
	quarantineSig     string
	modelCooldowns    map[string]*openAICodexAccountCooldown
	modelEntitlements map[string]bool

	catalog             []providerModel
	catalogETag         string
	catalogCredentialID string
	catalogSourceDigest string
	catalogKnown        bool
	catalogStale        bool
}

type openAICodexAccountPoolSnapshot struct {
	models      []providerModel
	eligibility map[string][]string
	etag        string
	usable      int
}

type openAICodexAffinityRecord struct {
	key          [sha256.Size]byte
	credentialID string
	expiresAt    time.Time
}

type openAICodexAccountPool struct {
	providerID string
	strategy   string
	configured bool

	maxAccountAttempts int
	sessionAffinity    bool
	affinityTTL        time.Duration
	now                func() time.Time

	mu      sync.Mutex
	members []*openAICodexAccountMember
	byID    map[string]*openAICodexAccountMember
	cursor  map[string]uint64

	affinityKey [sha256.Size]byte
	affinity    map[[sha256.Size]byte]*list.Element
	affinityLRU *list.List

	snapshot atomic.Pointer[openAICodexAccountPoolSnapshot]
}

type openAICodexPoolContextKey struct{}

func withOpenAICodexAccountLease(ctx context.Context, lease *openAICodexAccountLease) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if lease == nil {
		return ctx
	}
	return context.WithValue(ctx, openAICodexPoolContextKey{}, lease)
}

func openAICodexAccountLeaseFromContext(ctx context.Context) *openAICodexAccountLease {
	if ctx == nil {
		return nil
	}
	lease, _ := ctx.Value(openAICodexPoolContextKey{}).(*openAICodexAccountLease)
	return lease
}

func newOpenAICodexAccountPool(providerID string, configured *OpenAICodexAccountsConfig) (*openAICodexAccountPool, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, fmt.Errorf("OpenAI Codex account pool provider id is required")
	}
	pool := &openAICodexAccountPool{
		providerID:  providerID,
		strategy:    openAICodexAccountStrategyFillFirst,
		now:         time.Now,
		byID:        make(map[string]*openAICodexAccountMember),
		cursor:      make(map[string]uint64),
		affinity:    make(map[[sha256.Size]byte]*list.Element),
		affinityLRU: list.New(),
	}
	if _, err := rand.Read(pool.affinityKey[:]); err != nil {
		return nil, fmt.Errorf("initialize OpenAI Codex affinity key: %w", err)
	}

	if configured == nil {
		auth, err := newOpenAICodexAuth()
		if err != nil {
			return nil, err
		}
		member := &openAICodexAccountMember{
			id:                "default",
			authPath:          auth.path,
			auth:              auth,
			modelCooldowns:    make(map[string]*openAICodexAccountCooldown),
			modelEntitlements: make(map[string]bool),
		}
		pool.members = []*openAICodexAccountMember{member}
		pool.byID[member.id] = member
		pool.maxAccountAttempts = 1
		pool.publishSnapshotLocked()
		return pool, nil
	}

	if len(configured.Accounts) == 0 {
		return nil, fmt.Errorf("OpenAI Codex account pool must contain at least one account")
	}
	if len(configured.Accounts) > maxOpenAICodexAccounts {
		return nil, fmt.Errorf("OpenAI Codex account pool contains more than %d accounts", maxOpenAICodexAccounts)
	}
	strategy := strings.TrimSpace(configured.Strategy)
	if strategy != openAICodexAccountStrategyRoundRobin && strategy != openAICodexAccountStrategyFillFirst {
		return nil, fmt.Errorf("unsupported OpenAI Codex account strategy %q", configured.Strategy)
	}
	if configured.MaxAccountAttempts < 0 || configured.MaxAccountAttempts > len(configured.Accounts) {
		return nil, fmt.Errorf("OpenAI Codex max account attempts must be between zero and the pool size")
	}

	pool.configured = true
	pool.strategy = strategy
	pool.maxAccountAttempts = configured.MaxAccountAttempts
	if pool.maxAccountAttempts == 0 || pool.maxAccountAttempts > len(configured.Accounts) {
		pool.maxAccountAttempts = len(configured.Accounts)
	}
	pool.sessionAffinity = len(configured.Accounts) > 1
	if configured.SessionAffinity != nil {
		pool.sessionAffinity = *configured.SessionAffinity
	}
	ttlText := strings.TrimSpace(configured.SessionAffinityTTL)
	if ttlText == "" {
		pool.affinityTTL = time.Hour
	} else {
		var err error
		pool.affinityTTL, err = time.ParseDuration(ttlText)
		if err != nil || pool.affinityTTL <= 0 {
			return nil, fmt.Errorf("OpenAI Codex session affinity TTL must be a positive duration")
		}
	}

	seenPaths := make(map[string]struct{}, len(configured.Accounts))
	for _, account := range configured.Accounts {
		memberID := strings.TrimSpace(account.ID)
		if memberID == "" {
			return nil, fmt.Errorf("OpenAI Codex account id is required")
		}
		if _, duplicate := pool.byID[memberID]; duplicate {
			return nil, fmt.Errorf("OpenAI Codex account id %q is duplicated", memberID)
		}
		path, err := canonicalOpenAICodexAuthFilePath(account.AuthFile)
		if err != nil {
			return nil, fmt.Errorf("OpenAI Codex account %q: %w", account.ID, err)
		}
		if _, duplicate := seenPaths[path]; duplicate {
			return nil, fmt.Errorf("OpenAI Codex auth path is duplicated")
		}
		seenPaths[path] = struct{}{}
		member := &openAICodexAccountMember{
			id:                memberID,
			authPath:          path,
			auth:              newOpenAICodexAuthAt(path),
			modelCooldowns:    make(map[string]*openAICodexAccountCooldown),
			modelEntitlements: make(map[string]bool),
		}
		pool.members = append(pool.members, member)
		pool.byID[member.id] = member
	}
	pool.publishSnapshotLocked()
	return pool, nil
}

func (p *openAICodexAccountPool) effectiveAccountAttempts() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	attempts := p.maxAccountAttempts
	if attempts <= 0 || attempts > len(p.members) {
		attempts = len(p.members)
	}
	return attempts
}

func (p *openAICodexAccountPool) loadSnapshot() *openAICodexAccountPoolSnapshot {
	if p == nil {
		return nil
	}
	return p.snapshot.Load()
}

func openAICodexCredentialID(providerID, memberID, accountID string, fedRAMP bool) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(providerID) + "\x00" + strings.TrimSpace(memberID) + "\x00" + strings.TrimSpace(accountID) + "\x00" + strconv.FormatBool(fedRAMP)))
	return "cred_codex_" + base64.RawURLEncoding.EncodeToString(digest[:18])
}

func openAICodexAuthFileSignature(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return "error:" + err.Error()
	}
	return openAICodexSourceDigest(body)
}

type openAICodexMemberCredentialResult struct {
	member          *openAICodexAccountMember
	credentials     openAICodexCredentials
	sourceSignature string
	err             error
}

func (p *openAICodexAccountPool) refreshCredentials(ctx context.Context, client *http.Client) error {
	if p == nil {
		return fmt.Errorf("OpenAI Codex account pool is not configured")
	}
	p.mu.Lock()
	members := make([]*openAICodexAccountMember, 0, len(p.members))
	for _, member := range p.members {
		if member == nil {
			continue
		}
		if member.quarantined && openAICodexAuthFileSignature(member.authPath) == member.quarantineSig {
			continue
		}
		members = append(members, member)
	}
	p.mu.Unlock()
	results := p.loadCredentials(ctx, client, members)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.applyCredentialResults(results, false); err != nil {
		return err
	}
	if !p.hasUsableAccount() {
		return fmt.Errorf("OpenAI Codex account pool has no usable account")
	}
	return nil
}

func (p *openAICodexAccountPool) initialize(ctx context.Context, client *http.Client) error {
	if p == nil {
		return fmt.Errorf("OpenAI Codex account pool is not configured")
	}
	results := p.loadAllCredentials(ctx, client)
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.applyCredentialResults(results, true)
}

func (p *openAICodexAccountPool) loadAllCredentials(ctx context.Context, client *http.Client) []openAICodexMemberCredentialResult {
	p.mu.Lock()
	members := append([]*openAICodexAccountMember(nil), p.members...)
	p.mu.Unlock()
	return p.loadCredentials(ctx, client, members)
}

func (p *openAICodexAccountPool) loadCredentials(ctx context.Context, client *http.Client, members []*openAICodexAccountMember) []openAICodexMemberCredentialResult {
	results := make([]openAICodexMemberCredentialResult, len(members))
	sem := make(chan struct{}, openAICodexPoolDiscoveryConcurrency)
	var wg sync.WaitGroup
	for index, member := range members {
		wg.Add(1)
		go func(index int, member *openAICodexAccountMember) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[index] = openAICodexMemberCredentialResult{member: member, err: ctx.Err()}
				return
			}
			sourceSignature := openAICodexAuthFileSignature(member.authPath)
			credentials, err := member.auth.credentials(ctx, client)
			results[index] = openAICodexMemberCredentialResult{member: member, credentials: credentials, sourceSignature: sourceSignature, err: err}
		}(index, member)
	}
	wg.Wait()
	return results
}

func (p *openAICodexAccountPool) applyCredentialResults(results []openAICodexMemberCredentialResult, requireUsable bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	seenAccountIDs := make(map[string]string, len(p.members))
	fedRAMPSet := false
	fedRAMP := false
	recordIdentity := func(memberID, accountID string, memberFedRAMP bool) error {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			return nil
		}
		if prior, duplicate := seenAccountIDs[accountID]; duplicate && prior != memberID {
			return fmt.Errorf("OpenAI Codex accounts %q and %q resolve to the same ChatGPT account", prior, memberID)
		}
		seenAccountIDs[accountID] = memberID
		if !fedRAMPSet {
			fedRAMPSet = true
			fedRAMP = memberFedRAMP
		} else if fedRAMP != memberFedRAMP {
			return fmt.Errorf("OpenAI Codex account pool cannot mix FedRAMP and non-FedRAMP accounts")
		}
		return nil
	}
	refreshing := make(map[*openAICodexAccountMember]struct{}, len(results))
	for _, result := range results {
		if result.member != nil {
			refreshing[result.member] = struct{}{}
		}
	}
	for _, member := range p.members {
		if member == nil {
			continue
		}
		if _, refreshed := refreshing[member]; refreshed {
			continue
		}
		if err := recordIdentity(member.id, member.accountID, member.fedRAMP); err != nil {
			return err
		}
	}

	usable := 0
	for _, result := range results {
		member := result.member
		if member == nil {
			continue
		}
		if result.err != nil {
			currentSignature := openAICodexAuthFileSignature(member.authPath)
			if result.sourceSignature != "" && currentSignature != result.sourceSignature {
				member.quarantined = false
				continue
			}
			member.quarantined = true
			member.quarantineSig = currentSignature
			if err := recordIdentity(member.id, member.accountID, member.fedRAMP); err != nil {
				return err
			}
			continue
		}
		currentSignature := openAICodexAuthFileSignature(member.authPath)
		if result.credentials.sourceDigest == "" || currentSignature != result.credentials.sourceDigest {
			member.quarantined = false
			continue
		}
		accountID := strings.TrimSpace(result.credentials.accountID)
		if p.configured && accountID == "" {
			member.quarantined = true
			member.quarantineSig = openAICodexAuthFileSignature(member.authPath)
			continue
		}
		if err := recordIdentity(member.id, accountID, result.credentials.fedRAMP); err != nil {
			return err
		}
		p.observeCredentialsLocked(member, result.credentials)
		usable++
	}
	p.publishSnapshotLocked()
	if requireUsable && usable == 0 {
		return fmt.Errorf("OpenAI Codex account pool has no usable account")
	}
	return nil
}

func (p *openAICodexAccountPool) observeCredentialsLocked(member *openAICodexAccountMember, credentials openAICodexCredentials) {
	member.accountID = strings.TrimSpace(credentials.accountID)
	member.fedRAMP = credentials.fedRAMP
	nextCredentialID := openAICodexCredentialID(p.providerID, member.id, member.accountID, member.fedRAMP)
	if member.credentialID != "" && member.credentialID != nextCredentialID {
		member.catalog = nil
		member.catalogETag = ""
		member.catalogCredentialID = ""
		member.catalogSourceDigest = ""
		member.catalogKnown = false
		member.catalogStale = false
		member.modelEntitlements = make(map[string]bool)
		member.modelCooldowns = make(map[string]*openAICodexAccountCooldown)
	}
	previousSourceDigest := member.sourceDigest
	member.credentialID = nextCredentialID
	member.sourceDigest = strings.TrimSpace(credentials.sourceDigest)
	if member.sourceDigest == "" {
		member.sourceDigest = openAICodexAuthFileSignature(member.authPath)
	}
	if previousSourceDigest != "" && previousSourceDigest != member.sourceDigest {
		for _, cooldown := range member.modelCooldowns {
			if cooldown != nil {
				cooldown.probeInFlight = false
			}
		}
	}
	member.quarantined = false
	member.quarantineSig = ""
}

func (p *openAICodexAccountPool) credentialsForMember(ctx context.Context, client *http.Client, member *openAICodexAccountMember) (openAICodexCredentials, string, error) {
	if p == nil || member == nil {
		return openAICodexCredentials{}, "", fmt.Errorf("OpenAI Codex account is unavailable")
	}
	sourceSignature := openAICodexAuthFileSignature(member.authPath)
	credentials, err := member.auth.credentials(ctx, client)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return openAICodexCredentials{}, "", err
		}
		currentSignature := openAICodexAuthFileSignature(member.authPath)
		if sourceSignature != "" && currentSignature != sourceSignature {
			member.quarantined = false
			return openAICodexCredentials{}, "", errOpenAICodexStaleCredentialGeneration
		}
		member.quarantined = true
		member.quarantineSig = currentSignature
		p.publishSnapshotLocked()
		return openAICodexCredentials{}, "", err
	}
	currentSignature := openAICodexAuthFileSignature(member.authPath)
	if credentials.sourceDigest == "" || currentSignature != credentials.sourceDigest {
		member.quarantined = false
		return openAICodexCredentials{}, "", errOpenAICodexStaleCredentialGeneration
	}
	accountID := strings.TrimSpace(credentials.accountID)
	if p.configured && accountID == "" {
		err = fmt.Errorf("OpenAI Codex auth file does not identify a ChatGPT account")
		member.quarantined = true
		member.quarantineSig = openAICodexAuthFileSignature(member.authPath)
		p.publishSnapshotLocked()
		return openAICodexCredentials{}, "", err
	}
	for _, other := range p.members {
		if other == member || other.accountID == "" {
			continue
		}
		if accountID != "" && accountID == other.accountID {
			member.quarantined = true
			member.quarantineSig = openAICodexAuthFileSignature(member.authPath)
			p.publishSnapshotLocked()
			return openAICodexCredentials{}, "", fmt.Errorf("OpenAI Codex accounts %q and %q resolve to the same ChatGPT account", other.id, member.id)
		}
		if credentials.fedRAMP != other.fedRAMP {
			member.quarantined = true
			member.quarantineSig = openAICodexAuthFileSignature(member.authPath)
			p.publishSnapshotLocked()
			return openAICodexCredentials{}, "", fmt.Errorf("OpenAI Codex account pool cannot mix FedRAMP and non-FedRAMP accounts")
		}
	}
	p.observeCredentialsLocked(member, credentials)
	credentials.sourceDigest = member.sourceDigest
	p.publishSnapshotLocked()
	return credentials, member.credentialID, nil
}

func (p *openAICodexAccountPool) affinityDigest(headers http.Header, body []byte) ([sha256.Size]byte, bool) {
	var zero [sha256.Size]byte
	if p == nil || !p.sessionAffinity {
		return zero, false
	}
	value := openAICodexSessionAffinityValue(headers, body)
	if value == "" {
		return zero, false
	}
	mac := hmac.New(sha256.New, p.affinityKey[:])
	_, _ = io.WriteString(mac, value)
	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return digest, true
}

func openAICodexSessionAffinityValue(headers http.Header, body []byte) string {
	for _, name := range []string{"Session_id", "session-id", "X-Session-ID", "thread-id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return strings.ToLower(name) + ":" + value
		}
	}
	parent := strings.TrimSpace(headers.Get("X-Codex-Parent-Thread-Id"))
	window := strings.TrimSpace(headers.Get("X-Codex-Window-Id"))
	if parent != "" && window != "" {
		return "codex-thread-window:" + parent + "\x00" + window
	}
	if len(body) > 0 {
		var payload struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Metadata != nil {
			var userID string
			if json.Unmarshal(payload.Metadata["user_id"], &userID) == nil && strings.TrimSpace(userID) != "" {
				return "metadata.user_id:" + strings.TrimSpace(userID)
			}
		}
	}
	return ""
}

func (p *openAICodexAccountPool) lookupAffinityLocked(key [sha256.Size]byte, now time.Time) string {
	element := p.affinity[key]
	if element == nil {
		return ""
	}
	record, _ := element.Value.(*openAICodexAffinityRecord)
	if record == nil || !now.Before(record.expiresAt) {
		p.removeAffinityElementLocked(element)
		return ""
	}
	p.affinityLRU.MoveToFront(element)
	return record.credentialID
}

func (p *openAICodexAccountPool) bindAffinityLocked(key [sha256.Size]byte, credentialID string, now time.Time) {
	if !p.sessionAffinity || credentialID == "" {
		return
	}
	if element := p.affinity[key]; element != nil {
		record, _ := element.Value.(*openAICodexAffinityRecord)
		if record != nil {
			record.credentialID = credentialID
			record.expiresAt = now.Add(p.affinityTTL)
		}
		p.affinityLRU.MoveToFront(element)
		return
	}
	record := &openAICodexAffinityRecord{key: key, credentialID: credentialID, expiresAt: now.Add(p.affinityTTL)}
	p.affinity[key] = p.affinityLRU.PushFront(record)
	for p.affinityLRU.Len() > defaultOpenAICodexPoolAffinityEntries {
		p.removeAffinityElementLocked(p.affinityLRU.Back())
	}
}

func (p *openAICodexAccountPool) removeAffinityElementLocked(element *list.Element) {
	if element == nil {
		return
	}
	record, _ := element.Value.(*openAICodexAffinityRecord)
	if record != nil {
		delete(p.affinity, record.key)
	}
	p.affinityLRU.Remove(element)
}

func (p *openAICodexAccountPool) candidateOrder(model string, affinityKey [sha256.Size]byte, hasAffinity bool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	ordered := make([]string, 0, len(p.members))
	start := 0
	if p.strategy == openAICodexAccountStrategyRoundRobin && len(p.members) > 0 {
		start = int(p.cursor[model] % uint64(len(p.members)))
		p.cursor[model]++
	}
	for offset := 0; offset < len(p.members); offset++ {
		member := p.members[(start+offset)%len(p.members)]
		ordered = append(ordered, member.id)
	}
	if hasAffinity {
		credentialID := p.lookupAffinityLocked(affinityKey, now)
		if credentialID != "" {
			for index, memberID := range ordered {
				member := p.byID[memberID]
				if member != nil && member.credentialID == credentialID {
					copy(ordered[1:index+1], ordered[:index])
					ordered[0] = memberID
					break
				}
			}
		}
	}
	return ordered
}

func (p *openAICodexAccountPool) memberAvailableLocked(member *openAICodexAccountMember, model string, now time.Time) (available bool, coolingUntil time.Time, halfOpen bool) {
	if member == nil {
		return false, time.Time{}, false
	}
	if member.quarantined {
		if openAICodexAuthFileSignature(member.authPath) == member.quarantineSig {
			return false, time.Time{}, false
		}
		member.quarantined = false
	}
	if p.configured && model != "" && !member.catalogKnown {
		return false, time.Time{}, false
	}
	if member.catalogKnown {
		eligible := false
		for _, candidate := range member.catalog {
			if candidate.publicID == model {
				eligible = true
				break
			}
		}
		if !eligible || member.modelEntitlements[model] {
			return false, time.Time{}, false
		}
	}
	cooldown := member.modelCooldowns[model]
	if cooldown == nil {
		return true, time.Time{}, false
	}
	if now.Before(cooldown.until) {
		return false, cooldown.until, false
	}
	if cooldown.probeInFlight {
		return false, cooldown.until, false
	}
	cooldown.probeInFlight = true
	return true, time.Time{}, true
}

func (p *openAICodexAccountPool) leaseForMember(ctx context.Context, client *http.Client, memberID, model string, affinityKey [sha256.Size]byte, hasAffinity bool, reason openAICodexAccountSelectionReason) (*openAICodexAccountLease, time.Time, error) {
	p.mu.Lock()
	member := p.byID[memberID]
	now := p.now()
	available, coolingUntil, halfOpen := true, time.Time{}, false
	if reason != openAICodexSelectionRetry {
		available, coolingUntil, halfOpen = p.memberAvailableLocked(member, model, now)
	} else if member == nil || member.quarantined {
		available = false
	} else if cooldown := member.modelCooldowns[model]; cooldown != nil && cooldown.probeInFlight {
		halfOpen = true
	}
	p.mu.Unlock()
	if !available {
		return nil, coolingUntil, nil
	}
	credentials, credentialID, err := p.credentialsForMember(ctx, client, member)
	if err != nil {
		p.mu.Lock()
		if cooldown := member.modelCooldowns[model]; cooldown != nil && halfOpen {
			cooldown.probeInFlight = false
		}
		p.mu.Unlock()
		return nil, time.Time{}, err
	}
	if p.configured && model != "" && reason != openAICodexSelectionHardState && reason != openAICodexSelectionRetry {
		p.mu.Lock()
		eligible := member.catalogKnown && member.catalogCredentialID == credentialID && !member.modelEntitlements[model]
		if eligible {
			eligible = false
			for _, candidate := range member.catalog {
				if candidate.publicID == model {
					eligible = true
					break
				}
			}
		}
		p.mu.Unlock()
		if !eligible {
			return nil, time.Time{}, nil
		}
	}
	if reason == "" {
		reason = openAICodexSelectionFillFirst
		if p.strategy == openAICodexAccountStrategyRoundRobin {
			reason = openAICodexSelectionRoundRobin
		}
		if len(p.members) == 1 {
			reason = openAICodexSelectionSingle
		}
	}
	if hasAffinity {
		p.mu.Lock()
		if p.lookupAffinityLocked(affinityKey, p.now()) == credentialID {
			reason = openAICodexSelectionAffinity
		}
		p.mu.Unlock()
	}
	return &openAICodexAccountLease{
		pool:            p,
		member:          member,
		credentials:     credentials,
		credentialID:    credentialID,
		sourceDigest:    credentials.sourceDigest,
		accessMemberID:  member.id,
		selectionReason: reason,
		affinityKey:     affinityKey,
		hasAffinityKey:  hasAffinity,
		halfOpen:        halfOpen,
	}, time.Time{}, nil
}

func (p *openAICodexAccountPool) leaseGenerationCurrentLocked(lease *openAICodexAccountLease) bool {
	if p == nil || lease == nil || lease.member == nil {
		return false
	}
	member := lease.member
	if member.credentialID != lease.credentialID || member.sourceDigest != lease.sourceDigest {
		return false
	}
	return lease.sourceDigest != "" && openAICodexAuthFileSignature(member.authPath) == lease.sourceDigest
}

func (p *openAICodexAccountPool) releaseStaleLeaseProbeLocked(lease *openAICodexAccountLease, model string) {
	if lease == nil || lease.member == nil || !lease.halfOpen {
		return
	}
	if lease.member.credentialID != lease.credentialID || lease.member.sourceDigest != lease.sourceDigest {
		return
	}
	if cooldown := lease.member.modelCooldowns[model]; cooldown != nil {
		cooldown.probeInFlight = false
	}
}

func (p *openAICodexAccountPool) reportSuccess(lease *openAICodexAccountLease, model string) {
	if p == nil || lease == nil || lease.member == nil {
		return
	}
	p.mu.Lock()
	if !p.leaseGenerationCurrentLocked(lease) {
		p.releaseStaleLeaseProbeLocked(lease, model)
		p.mu.Unlock()
		return
	}
	delete(lease.member.modelCooldowns, model)
	delete(lease.member.modelEntitlements, model)
	if lease.hasAffinityKey {
		p.bindAffinityLocked(lease.affinityKey, lease.credentialID, p.now())
	}
	p.publishSnapshotLocked()
	p.mu.Unlock()
}

func (p *openAICodexAccountPool) reportFailure(lease *openAICodexAccountLease, model string, class openAICodexAccountFailureClass, headers http.Header) {
	if p == nil || lease == nil || lease.member == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.leaseGenerationCurrentLocked(lease) {
		p.releaseStaleLeaseProbeLocked(lease, model)
		return
	}
	member := lease.member
	switch class {
	case openAICodexAccountFailureAuth:
		member.quarantined = true
		member.quarantineSig = openAICodexAuthFileSignature(member.authPath)
	case openAICodexAccountFailureQuota:
		delay := defaultOpenAICodexQuotaCooldown
		if seconds, _ := selectResponsesRetryAfter(headers); seconds != "" {
			if parsed, err := strconv.ParseInt(seconds, 10, 64); err == nil && parsed > 0 {
				delay = retryAfterDurationFromSeconds(parsed)
			}
		}
		member.modelCooldowns[model] = &openAICodexAccountCooldown{until: p.now().Add(delay)}
	case openAICodexAccountFailureEntitlement:
		member.modelEntitlements[model] = true
	}
	if cooldown := member.modelCooldowns[model]; cooldown != nil {
		cooldown.probeInFlight = false
	}
	p.publishSnapshotLocked()
}

func (p *openAICodexAccountPool) releaseLease(lease *openAICodexAccountLease, model string) {
	if p == nil || lease == nil || lease.member == nil || !lease.halfOpen {
		return
	}
	p.mu.Lock()
	if p.leaseGenerationCurrentLocked(lease) {
		if cooldown := lease.member.modelCooldowns[model]; cooldown != nil {
			cooldown.probeInFlight = false
		}
	} else {
		p.releaseStaleLeaseProbeLocked(lease, model)
	}
	p.mu.Unlock()
}

func (p *openAICodexAccountPool) publishSnapshotLocked() {
	if p == nil {
		return
	}
	byModel := make(map[string][]providerModel)
	eligibility := make(map[string][]string)
	usable := 0
	for _, member := range p.members {
		if member == nil || member.quarantined || member.credentialID == "" {
			continue
		}
		usable++
		for _, model := range member.catalog {
			if member.modelEntitlements[model.publicID] {
				continue
			}
			byModel[model.publicID] = append(byModel[model.publicID], model)
			eligibility[model.publicID] = append(eligibility[model.publicID], member.credentialID)
		}
	}
	ids := make([]string, 0, len(byModel))
	for id := range byModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	models := make([]providerModel, 0, len(ids))
	etagHash := sha256.New()
	for _, id := range ids {
		merged := mergeOpenAICodexProviderModels(byModel[id])
		models = append(models, merged)
		_, _ = io.WriteString(etagHash, id)
		_, _ = etagHash.Write(merged.raw)
		sort.Strings(eligibility[id])
		for _, credentialID := range eligibility[id] {
			_, _ = io.WriteString(etagHash, credentialID)
		}
	}
	etag := ""
	if len(models) > 0 {
		etag = `"vekil-codex-` + base64.RawURLEncoding.EncodeToString(etagHash.Sum(nil)[:18]) + `"`
	}
	copyEligibility := make(map[string][]string, len(eligibility))
	for model, credentials := range eligibility {
		copyEligibility[model] = append([]string(nil), credentials...)
	}
	p.snapshot.Store(&openAICodexAccountPoolSnapshot{
		models:      append([]providerModel(nil), models...),
		eligibility: copyEligibility,
		etag:        etag,
		usable:      usable,
	})
}

func mergeOpenAICodexProviderModels(models []providerModel) providerModel {
	if len(models) == 0 {
		return providerModel{}
	}
	merged := models[0]
	merged.raw = append(json.RawMessage(nil), models[0].raw...)
	parallel := merged.parallelToolCalls != nil && *merged.parallelToolCalls
	for _, model := range models[1:] {
		parallel = parallel && model.parallelToolCalls != nil && *model.parallelToolCalls
		merged.raw = mergeOpenAICodexModelRawConservatively(merged.raw, model.raw)
	}
	merged.parallelToolCalls = cloneBoolPtr(&parallel)
	return merged
}

func mergeOpenAICodexModelRawConservatively(leftRaw, rightRaw json.RawMessage) json.RawMessage {
	var left, right map[string]any
	if json.Unmarshal(leftRaw, &left) != nil || json.Unmarshal(rightRaw, &right) != nil {
		return append(json.RawMessage(nil), leftRaw...)
	}
	for _, field := range []string{"context_window", "max_context_window", "auto_compact_token_limit"} {
		left[field] = minPositiveJSONNumber(left[field], right[field])
	}
	for _, field := range []string{"supports_reasoning_summaries", "support_verbosity", "supports_parallel_tool_calls", "supports_image_detail_original"} {
		left[field] = jsonBool(left[field]) && jsonBool(right[field])
	}
	leftCaps, _ := left["capabilities"].(map[string]any)
	rightCaps, _ := right["capabilities"].(map[string]any)
	leftSupports := map[string]any{}
	rightSupports := map[string]any{}
	leftLimits := map[string]any{}
	rightLimits := map[string]any{}
	if leftCaps != nil {
		if value, ok := leftCaps["supports"].(map[string]any); ok {
			leftSupports = value
		}
		if value, ok := leftCaps["limits"].(map[string]any); ok {
			leftLimits = value
		}
	}
	if rightCaps != nil {
		if value, ok := rightCaps["supports"].(map[string]any); ok {
			rightSupports = value
		}
		if value, ok := rightCaps["limits"].(map[string]any); ok {
			rightLimits = value
		}
	}
	left["capabilities"] = map[string]any{
		"supports": map[string]any{
			"parallel_tool_calls": jsonBool(leftSupports["parallel_tool_calls"]) && jsonBool(rightSupports["parallel_tool_calls"]),
			"vision":              jsonBool(leftSupports["vision"]) && jsonBool(rightSupports["vision"]),
			"reasoning_effort":    intersectJSONStrings(leftSupports["reasoning_effort"], rightSupports["reasoning_effort"]),
		},
		"limits": map[string]any{
			"max_context_window_tokens": minPositiveJSONNumber(leftLimits["max_context_window_tokens"], rightLimits["max_context_window_tokens"]),
		},
	}
	left["input_modalities"] = intersectJSONStrings(left["input_modalities"], right["input_modalities"])
	encoded, err := json.Marshal(left)
	if err != nil {
		return append(json.RawMessage(nil), leftRaw...)
	}
	return encoded
}

func jsonBool(value any) bool {
	result, _ := value.(bool)
	return result
}

func minPositiveJSONNumber(left, right any) any {
	leftNumber, leftOK := left.(float64)
	rightNumber, rightOK := right.(float64)
	if !leftOK || leftNumber <= 0 || !rightOK || rightNumber <= 0 {
		return float64(0)
	}
	if rightNumber < leftNumber {
		return rightNumber
	}
	return leftNumber
}

func intersectJSONStrings(left, right any) []string {
	leftValues := jsonStringSet(left)
	rightValues := jsonStringSet(right)
	result := make([]string, 0, len(leftValues))
	for value := range leftValues {
		if _, ok := rightValues[value]; ok {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func jsonStringSet(value any) map[string]struct{} {
	result := make(map[string]struct{})
	values, _ := value.([]any)
	for _, raw := range values {
		if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
			result[text] = struct{}{}
		}
	}
	return result
}

func classifyOpenAICodexAccountHTTPFailure(status int, body []byte) openAICodexAccountFailureClass {
	lower := strings.ToLower(string(body))
	if status == http.StatusUnauthorized {
		return openAICodexAccountFailureAuth
	}
	if status == http.StatusTooManyRequests || strings.Contains(lower, "insufficient_quota") || strings.Contains(lower, "quota_exceeded") || strings.Contains(lower, "usage limit") {
		return openAICodexAccountFailureQuota
	}
	if status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusBadRequest {
		for _, marker := range []string{"model_not_found", "not entitled", "not_entitled", "does not have access", "not available for this account", "unsupported_model"} {
			if strings.Contains(lower, marker) {
				return openAICodexAccountFailureEntitlement
			}
		}
	}
	return openAICodexAccountFailureNone
}

func openAICodexPoolExhaustionError(status int, retryAt time.Time, now time.Time) error {
	message := "OpenAI Codex account pool has no usable account"
	if status == http.StatusTooManyRequests {
		message = "all eligible OpenAI Codex accounts are cooling down"
		delay := retryAt.Sub(now)
		if delay < time.Second {
			delay = time.Second
		}
		seconds := strconv.FormatInt(durationSecondsCeil(delay), 10)
		return &upstreamError{
			statusCode: status,
			body:       []byte(`{"error":{"type":"rate_limit_error","code":"account_pool_cooldown","message":"` + message + `"}}`),
			retryAfter: seconds,
			headers:    http.Header{"Retry-After": []string{seconds}},
		}
	}
	return &providerRequestError{statusCode: status, err: errors.New(message)}
}

type openAICodexCatalogFetchOutcome struct {
	member       *openAICodexAccountMember
	credentialID string
	sourceDigest string
	models       []providerModel
	etag         string
	notModified  bool
	err          error
}

func (p *openAICodexAccountPool) catalogOutcomeCurrentLocked(outcome openAICodexCatalogFetchOutcome) bool {
	if p == nil || outcome.member == nil {
		return false
	}
	member := outcome.member
	return outcome.credentialID != "" && outcome.sourceDigest != "" &&
		member.credentialID == outcome.credentialID && member.sourceDigest == outcome.sourceDigest &&
		openAICodexAuthFileSignature(member.authPath) == outcome.sourceDigest
}

func (p *openAICodexAccountPool) invalidateMemberCatalogLocked(member *openAICodexAccountMember) {
	if member == nil {
		return
	}
	member.catalog = nil
	member.catalogETag = ""
	member.catalogCredentialID = ""
	member.catalogSourceDigest = ""
	member.catalogKnown = false
	member.catalogStale = false
}

func (p *openAICodexAccountPool) invalidateCatalogOutcomeGenerationLocked(outcome openAICodexCatalogFetchOutcome) {
	member := outcome.member
	if member == nil || !member.catalogKnown {
		return
	}
	if member.catalogCredentialID == outcome.credentialID && member.catalogSourceDigest == outcome.sourceDigest {
		p.invalidateMemberCatalogLocked(member)
	}
}

func (p *openAICodexAccountPool) fetchModels(ctx context.Context, h *ProxyHandler, provider *providerRuntime, rawQuery, ifNoneMatch string) (providerModelsFetchResult, error) {
	if p == nil || h == nil || provider == nil {
		return providerModelsFetchResult{}, fmt.Errorf("OpenAI Codex account pool discovery is not configured")
	}
	if err := p.refreshCredentials(ctx, h.client); err != nil {
		return providerModelsFetchResult{}, err
	}

	p.mu.Lock()
	members := make([]*openAICodexAccountMember, 0, len(p.members))
	for _, member := range p.members {
		if member != nil && !member.quarantined && member.credentialID != "" {
			members = append(members, member)
		}
	}
	p.mu.Unlock()
	outcomes := make([]openAICodexCatalogFetchOutcome, len(members))
	sem := make(chan struct{}, openAICodexPoolDiscoveryConcurrency)
	var wg sync.WaitGroup
	for index, member := range members {
		wg.Add(1)
		go func(index int, member *openAICodexAccountMember) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outcomes[index] = openAICodexCatalogFetchOutcome{member: member, err: ctx.Err()}
				return
			}
			outcomes[index] = p.fetchMemberModels(ctx, h, provider, member, rawQuery)
		}(index, member)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return providerModelsFetchResult{}, err
	}

	canonical := strings.TrimSpace(rawQuery) == ""
	if !canonical {
		return p.mergeVariantCatalogOutcomes(outcomes)
	}

	p.mu.Lock()
	var firstErr error
	catalogs := 0
	for _, outcome := range outcomes {
		member := outcome.member
		if member == nil {
			continue
		}
		if !p.catalogOutcomeCurrentLocked(outcome) {
			p.invalidateCatalogOutcomeGenerationLocked(outcome)
			continue
		}
		if outcome.err == nil {
			if outcome.notModified && (!member.catalogKnown || member.catalogCredentialID != outcome.credentialID) {
				outcome.err = fmt.Errorf("OpenAI Codex account %q returned 304 without a matching catalog generation", member.id)
			} else if outcome.notModified {
				member.catalogSourceDigest = outcome.sourceDigest
			} else {
				member.catalog = append([]providerModel(nil), outcome.models...)
				member.catalogETag = outcome.etag
				member.catalogCredentialID = outcome.credentialID
				member.catalogSourceDigest = outcome.sourceDigest
				member.catalogKnown = true
				member.modelEntitlements = make(map[string]bool)
			}
		}
		if outcome.err != nil {
			if firstErr == nil {
				firstErr = outcome.err
			}
			if member.catalogKnown && member.catalogCredentialID == member.credentialID {
				member.catalogStale = true
			}
		} else {
			member.catalogStale = false
		}
		if member.catalogKnown && !member.quarantined {
			catalogs++
		}
	}
	p.publishSnapshotLocked()
	snapshot := p.snapshot.Load()
	p.mu.Unlock()

	if catalogs == 0 || snapshot == nil || len(snapshot.models) == 0 {
		if firstErr != nil {
			if p.configured {
				return providerModelsFetchResult{}, fmt.Errorf("OpenAI Codex account pool has no usable model catalog")
			}
			return providerModelsFetchResult{}, firstErr
		}
		return providerModelsFetchResult{}, fmt.Errorf("OpenAI Codex account pool has no usable model catalog")
	}
	if strings.TrimSpace(ifNoneMatch) != "" && strings.TrimSpace(ifNoneMatch) == snapshot.etag {
		return providerModelsFetchResult{etag: snapshot.etag, notModified: true}, nil
	}
	return providerModelsFetchResult{models: append([]providerModel(nil), snapshot.models...), etag: snapshot.etag}, nil
}

func (p *openAICodexAccountPool) fetchMemberModels(ctx context.Context, h *ProxyHandler, provider *providerRuntime, member *openAICodexAccountMember, rawQuery string) openAICodexCatalogFetchOutcome {
	outcome := openAICodexCatalogFetchOutcome{member: member}
	p.mu.Lock()
	outcome.credentialID = member.credentialID
	outcome.sourceDigest = member.sourceDigest
	p.mu.Unlock()
	credentials, credentialID, err := p.credentialsForMember(ctx, h.client, member)
	if err != nil {
		outcome.err = err
		return outcome
	}
	lease := &openAICodexAccountLease{
		pool:            p,
		member:          member,
		credentials:     credentials,
		credentialID:    credentialID,
		sourceDigest:    credentials.sourceDigest,
		accessMemberID:  member.id,
		selectionReason: openAICodexSelectionFillFirst,
	}
	outcome.credentialID = credentialID
	outcome.sourceDigest = lease.sourceDigest

	p.mu.Lock()
	cachedETag := ""
	if strings.TrimSpace(rawQuery) == "" {
		cachedETag = member.catalogETag
	}
	p.mu.Unlock()
	modelsQuery := openAICodexModelsRawQuery(rawQuery)
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		requestCtx := withOpenAICodexAccountLease(ctx, lease)
		req, requestErr := h.newProviderJSONRequest(requestCtx, provider, http.MethodGet, providerEndpointModels, nil, nil, modelsQuery)
		if requestErr != nil {
			return nil, requestErr
		}
		if cachedETag != "" {
			req.Header.Set("If-None-Match", cachedETag)
		}
		return req, nil
	})
	if err != nil {
		outcome.err = err
		return outcome
	}
	defer drainAndClose(resp.Body)
	outcome.etag = resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		outcome.notModified = true
		return outcome
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		class := classifyOpenAICodexAccountHTTPFailure(resp.StatusCode, body)
		if class != openAICodexAccountFailureNone {
			p.reportFailure(lease, "", class, resp.Header)
		}
		outcome.err = &providerRequestError{
			statusCode: resp.StatusCode,
			err:        fmt.Errorf("unexpected /models status %d", resp.StatusCode),
		}
		return outcome
	}
	body, err := readProviderModelCatalogBody(resp.Body)
	if err != nil {
		outcome.err = err
		return outcome
	}
	outcome.models, err = decodeOpenAICodexModelsFromBody(provider, body)
	if err != nil {
		outcome.err = err
		return outcome
	}
	return outcome
}

func (p *openAICodexAccountPool) needsCredentialRefresh() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, member := range p.members {
		if member == nil {
			continue
		}
		if !member.quarantined {
			return true
		}
		if openAICodexAuthFileSignature(member.authPath) != member.quarantineSig {
			return true
		}
	}
	return false
}

func (p *openAICodexAccountPool) hasUsableAccount() bool {
	snapshot := p.loadSnapshot()
	return snapshot != nil && snapshot.usable > 0
}

func (p *openAICodexAccountPool) authorizeRequest(req *http.Request, client *http.Client) error {
	if p == nil || req == nil {
		return fmt.Errorf("OpenAI Codex account pool is not configured")
	}
	lease := openAICodexAccountLeaseFromContext(req.Context())
	if lease == nil {
		p.mu.Lock()
		if len(p.members) != 1 {
			p.mu.Unlock()
			return fmt.Errorf("OpenAI Codex pooled request is missing an account lease")
		}
		member := p.members[0]
		p.mu.Unlock()
		credentials, credentialID, err := p.credentialsForMember(req.Context(), client, member)
		if err != nil {
			return err
		}
		lease = &openAICodexAccountLease{
			pool:            p,
			member:          member,
			credentials:     credentials,
			credentialID:    credentialID,
			sourceDigest:    credentials.sourceDigest,
			accessMemberID:  member.id,
			selectionReason: openAICodexSelectionSingle,
		}
	}
	if lease.pool != p {
		return fmt.Errorf("OpenAI Codex account lease belongs to a different provider")
	}
	return lease.authorize(req)
}

func (p *openAICodexAccountPool) memberIDForCredential(credentialID string) string {
	if p == nil || credentialID == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, member := range p.members {
		if member != nil && member.credentialID == credentialID {
			return member.id
		}
	}
	return ""
}

func (p *openAICodexAccountPool) acquireForOperation(ctx context.Context, client *http.Client, operation *routeOperation, model string, headers http.Header, body []byte) (*openAICodexAccountLease, error) {
	if p == nil || operation == nil {
		return nil, fmt.Errorf("OpenAI Codex account pool operation is not configured")
	}
	if !p.hasUsableAccount() {
		if !p.needsCredentialRefresh() {
			return nil, openAICodexPoolExhaustionError(http.StatusServiceUnavailable, time.Time{}, p.now())
		}
		if err := p.refreshCredentials(ctx, client); err != nil {
			return nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: err}
		}
	}
	operation.mu.Lock()
	pinnedCredential := operation.pinnedCredentialID
	retryMember := operation.codexRetryMemberID
	initialized := operation.codexCandidateInitialized
	operation.mu.Unlock()
	if pinnedCredential == "" && retryMember == "" && !initialized {
		operation.initializeCodexCandidates(p, model, headers, body)
	}

	operation.mu.Lock()
	order := append([]string(nil), operation.codexCandidateOrder...)
	pinnedCredential = operation.pinnedCredentialID
	retryMember = operation.codexRetryMemberID
	expectedRetryCredential := operation.codexLastCredentialID
	affinityKey := operation.codexAffinityKey
	hasAffinity := operation.codexHasAffinityKey
	attempted := make(map[string]struct{}, len(operation.codexAttemptedMembers))
	for memberID := range operation.codexAttemptedMembers {
		attempted[memberID] = struct{}{}
	}
	attempts := operation.codexAccountAttempts
	operation.mu.Unlock()

	if retryMember != "" {
		lease, _, err := p.leaseForMember(ctx, client, retryMember, model, affinityKey, hasAffinity, openAICodexSelectionRetry)
		if err != nil {
			if errors.Is(err, errOpenAICodexStaleCredentialGeneration) {
				return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: errOpenAICodexStaleCredentialGeneration}
			}
			if p.configured {
				return nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: fmt.Errorf("OpenAI Codex account is unavailable")}
			}
			return nil, err
		}
		if lease == nil {
			return nil, openAICodexPoolExhaustionError(http.StatusServiceUnavailable, time.Time{}, p.now())
		}
		if pinnedCredential != "" && lease.credentialID != pinnedCredential {
			p.releaseLease(lease, model)
			return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: errOpenAICodexStaleCredentialGeneration}
		}
		if expectedRetryCredential != "" && lease.credentialID != expectedRetryCredential {
			p.releaseLease(lease, model)
			return nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: errOpenAICodexStaleCredentialGeneration}
		}
		operation.markCodexLeaseSelected(lease)
		return lease, nil
	}

	if pinnedCredential != "" {
		memberID := p.memberIDForCredential(pinnedCredential)
		if memberID == "" {
			return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: errOpenAICodexStaleCredentialGeneration}
		}
		lease, coolingUntil, err := p.leaseForMember(ctx, client, memberID, model, affinityKey, hasAffinity, openAICodexSelectionHardState)
		if err != nil {
			if errors.Is(err, errOpenAICodexStaleCredentialGeneration) {
				return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: errOpenAICodexStaleCredentialGeneration}
			}
			if p.configured {
				return nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: fmt.Errorf("provider-bound OpenAI Codex account is unavailable")}
			}
			return nil, err
		}
		if lease == nil {
			if !coolingUntil.IsZero() {
				return nil, openAICodexPoolExhaustionError(http.StatusTooManyRequests, coolingUntil, p.now())
			}
			return nil, openAICodexPoolExhaustionError(http.StatusServiceUnavailable, time.Time{}, p.now())
		}
		if lease.credentialID != pinnedCredential {
			p.releaseLease(lease, model)
			return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: errOpenAICodexStaleCredentialGeneration}
		}
		operation.markCodexLeaseSelected(lease)
		return lease, nil
	}

	maxAttempts := p.effectiveAccountAttempts()
	var earliestCooldown time.Time
	sawUnavailable := false
	for _, memberID := range order {
		if attempts >= maxAttempts {
			break
		}
		if _, alreadyAttempted := attempted[memberID]; alreadyAttempted {
			continue
		}
		lease, coolingUntil, err := p.leaseForMember(ctx, client, memberID, model, affinityKey, hasAffinity, "")
		if lease == nil && err == nil {
			if coolingUntil.IsZero() {
				sawUnavailable = true
				operation.markCodexMemberConsidered(memberID)
				attempted[memberID] = struct{}{}
			} else if earliestCooldown.IsZero() || coolingUntil.Before(earliestCooldown) {
				earliestCooldown = coolingUntil
			}
			continue
		}
		if err != nil {
			operation.markCodexMemberConsidered(memberID)
			attempted[memberID] = struct{}{}
			sawUnavailable = true
			continue
		}
		if operation.codexLeaseWouldSwitch(lease) {
			lease.selectionReason = openAICodexSelectionFailover
		}
		operation.markCodexLeaseSelected(lease)
		return lease, nil
	}
	if !earliestCooldown.IsZero() && !sawUnavailable {
		return nil, openAICodexPoolExhaustionError(http.StatusTooManyRequests, earliestCooldown, p.now())
	}
	status, retryAt := p.exhaustionStatus(model)
	return nil, openAICodexPoolExhaustionError(status, retryAt, p.now())
}

func openAICodexFailureDetails(err error) (http.Header, []byte) {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) && upstreamErr != nil {
		return upstreamErr.headers.Clone(), append([]byte(nil), upstreamErr.body...)
	}
	return nil, nil
}

func (p *openAICodexAccountPool) exhaustionStatus(model string) (int, time.Time) {
	if p == nil {
		return http.StatusServiceUnavailable, time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var earliest time.Time
	sawCooling := false
	sawOther := false
	for _, member := range p.members {
		if member == nil {
			continue
		}
		if model != "" {
			if !member.catalogKnown || member.modelEntitlements[model] {
				continue
			}
			advertised := false
			for _, candidate := range member.catalog {
				if candidate.publicID == model {
					advertised = true
					break
				}
			}
			if !advertised {
				continue
			}
		}
		if member.quarantined {
			sawOther = true
			continue
		}
		cooldown := member.modelCooldowns[model]
		if cooldown != nil && now.Before(cooldown.until) {
			sawCooling = true
			if earliest.IsZero() || cooldown.until.Before(earliest) {
				earliest = cooldown.until
			}
			continue
		}
		sawOther = true
	}
	if sawCooling && !sawOther {
		return http.StatusTooManyRequests, earliest
	}
	return http.StatusServiceUnavailable, time.Time{}
}

func (p *openAICodexAccountPool) mergeVariantCatalogOutcomes(outcomes []openAICodexCatalogFetchOutcome) (providerModelsFetchResult, error) {
	byModel := make(map[string][]providerModel)
	credentials := make(map[string][]string)
	var firstErr error
	p.mu.Lock()
	catalogInvalidated := false
	for _, outcome := range outcomes {
		if outcome.member != nil && !p.catalogOutcomeCurrentLocked(outcome) {
			p.invalidateCatalogOutcomeGenerationLocked(outcome)
			catalogInvalidated = true
			continue
		}
		if outcome.err != nil {
			if firstErr == nil {
				firstErr = outcome.err
			}
			continue
		}
		member := outcome.member
		if member == nil {
			continue
		}
		for _, model := range outcome.models {
			byModel[model.publicID] = append(byModel[model.publicID], model)
			credentials[model.publicID] = append(credentials[model.publicID], member.credentialID)
		}
	}
	if catalogInvalidated {
		p.publishSnapshotLocked()
	}
	fallback := p.snapshot.Load()
	p.mu.Unlock()
	if len(byModel) == 0 {
		if fallback != nil && len(fallback.models) > 0 {
			return providerModelsFetchResult{models: append([]providerModel(nil), fallback.models...), etag: fallback.etag}, nil
		}
		if firstErr != nil {
			return providerModelsFetchResult{}, firstErr
		}
		return providerModelsFetchResult{}, fmt.Errorf("OpenAI Codex account pool has no usable model catalog")
	}
	ids := make([]string, 0, len(byModel))
	for id := range byModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	models := make([]providerModel, 0, len(ids))
	hash := sha256.New()
	for _, id := range ids {
		model := mergeOpenAICodexProviderModels(byModel[id])
		models = append(models, model)
		_, _ = io.WriteString(hash, id)
		_, _ = hash.Write(model.raw)
		sort.Strings(credentials[id])
		for _, credentialID := range credentials[id] {
			_, _ = io.WriteString(hash, credentialID)
		}
	}
	etag := `"vekil-codex-variant-` + base64.RawURLEncoding.EncodeToString(hash.Sum(nil)[:18]) + `"`
	return providerModelsFetchResult{models: models, etag: etag}, nil
}

func sanitizeOpenAICodexAccountFailureBody(body []byte, lease *openAICodexAccountLease) []byte {
	if len(body) == 0 || lease == nil {
		return body
	}
	redacted := body
	for _, sensitive := range []string{lease.credentials.accountID, lease.credentials.email} {
		sensitive = strings.TrimSpace(sensitive)
		if sensitive != "" && bytes.Contains(redacted, []byte(sensitive)) {
			redacted = bytes.ReplaceAll(redacted, []byte(sensitive), []byte("[redacted-account]"))
		}
	}
	return redacted
}

func sanitizeOpenAICodexAccountFailureError(err error, lease *openAICodexAccountLease) {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) && upstreamErr != nil {
		upstreamErr.body = sanitizeOpenAICodexAccountFailureBody(upstreamErr.body, lease)
	}
}
