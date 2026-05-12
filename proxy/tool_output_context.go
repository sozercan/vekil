package proxy

import (
	"sync"
	"time"
)

const (
	defaultToolExecutionContextTTL        = 2 * time.Hour
	defaultToolExecutionContextMaxEntries = 10000
)

type ToolExecutionContext struct {
	CallID           string
	ToolName         string
	OriginalCommand  string
	RewrittenCommand string
	RewriteProvider  string
	FilterHint       string
	CreatedAt        time.Time
}

type toolExecutionContextKey struct {
	Scope  string
	CallID string
}

type ToolExecutionContextStore struct {
	mu         sync.Mutex
	entries    map[toolExecutionContextKey]ToolExecutionContext
	ttl        time.Duration
	maxEntries int
}

func NewToolExecutionContextStore() *ToolExecutionContextStore {
	return NewToolExecutionContextStoreWithLimits(defaultToolExecutionContextTTL, defaultToolExecutionContextMaxEntries)
}

func NewToolExecutionContextStoreWithLimits(ttl time.Duration, maxEntries int) *ToolExecutionContextStore {
	if ttl <= 0 {
		ttl = defaultToolExecutionContextTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultToolExecutionContextMaxEntries
	}
	return &ToolExecutionContextStore{
		entries:    make(map[toolExecutionContextKey]ToolExecutionContext),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

func (s *ToolExecutionContextStore) Put(scope string, ctx ToolExecutionContext) {
	if s == nil || scope == "" || ctx.CallID == "" {
		return
	}
	if ctx.CreatedAt.IsZero() {
		ctx.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[toolExecutionContextKey]ToolExecutionContext)
	}
	s.expireLocked(time.Now())
	s.entries[toolExecutionContextKey{Scope: scope, CallID: ctx.CallID}] = ctx
	s.enforceMaxEntriesLocked()
}

func (s *ToolExecutionContextStore) Get(scope, callID string) (ToolExecutionContext, bool) {
	if s == nil || scope == "" || callID == "" {
		return ToolExecutionContext{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		return ToolExecutionContext{}, false
	}
	key := toolExecutionContextKey{Scope: scope, CallID: callID}
	ctx, ok := s.entries[key]
	if !ok {
		return ToolExecutionContext{}, false
	}
	if s.ttl > 0 && time.Since(ctx.CreatedAt) > s.ttl {
		delete(s.entries, key)
		return ToolExecutionContext{}, false
	}
	return ctx, true
}

func (s *ToolExecutionContextStore) Delete(scope, callID string) {
	if s == nil || scope == "" || callID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, toolExecutionContextKey{Scope: scope, CallID: callID})
}

func (s *ToolExecutionContextStore) expireLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}
	for key, ctx := range s.entries {
		if now.Sub(ctx.CreatedAt) > s.ttl {
			delete(s.entries, key)
		}
	}
}

func (s *ToolExecutionContextStore) enforceMaxEntriesLocked() {
	if s.maxEntries <= 0 || len(s.entries) <= s.maxEntries {
		return
	}
	for len(s.entries) > s.maxEntries {
		var oldestKey toolExecutionContextKey
		var oldest time.Time
		first := true
		for key, ctx := range s.entries {
			if first || ctx.CreatedAt.Before(oldest) {
				first = false
				oldest = ctx.CreatedAt
				oldestKey = key
			}
		}
		delete(s.entries, oldestKey)
	}
}
