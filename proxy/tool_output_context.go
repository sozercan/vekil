package proxy

import (
	"container/list"
	"sync"
	"time"
)

const (
	defaultToolExecutionContextTTL        = 2 * time.Hour
	defaultToolExecutionContextMaxEntries = 10000
	maxToolExecutionContextExpireInterval = time.Minute
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

type toolExecutionContextEntry struct {
	ctx     ToolExecutionContext
	element *list.Element
}

type ToolExecutionContextStore struct {
	mu           sync.Mutex
	entries      map[toolExecutionContextKey]*toolExecutionContextEntry
	order        *list.List
	ttl          time.Duration
	maxEntries   int
	nextExpireAt time.Time
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
		entries:    make(map[toolExecutionContextKey]*toolExecutionContextEntry),
		order:      list.New(),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

func (s *ToolExecutionContextStore) Put(scope string, ctx ToolExecutionContext) {
	if s == nil || scope == "" || ctx.CallID == "" {
		return
	}
	now := time.Now()
	if ctx.CreatedAt.IsZero() {
		ctx.CreatedAt = now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureInitializedLocked()
	s.maybeExpireLocked(now)
	key := toolExecutionContextKey{Scope: scope, CallID: ctx.CallID}
	if existing, ok := s.entries[key]; ok {
		if existing.element != nil {
			s.order.Remove(existing.element)
		}
		existing.ctx = ctx
		existing.element = s.order.PushBack(key)
	} else {
		s.entries[key] = &toolExecutionContextEntry{
			ctx:     ctx,
			element: s.order.PushBack(key),
		}
	}
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
	entry, ok := s.entries[key]
	if !ok {
		return ToolExecutionContext{}, false
	}
	ctx := entry.ctx
	if s.ttl > 0 && time.Since(ctx.CreatedAt) > s.ttl {
		s.removeEntryLocked(key)
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
	s.removeEntryLocked(toolExecutionContextKey{Scope: scope, CallID: callID})
}

func (s *ToolExecutionContextStore) ensureInitializedLocked() {
	if s.entries == nil {
		s.entries = make(map[toolExecutionContextKey]*toolExecutionContextEntry)
	}
	if s.order == nil {
		s.order = list.New()
		for key, entry := range s.entries {
			if entry == nil {
				delete(s.entries, key)
				continue
			}
			entry.element = s.order.PushBack(key)
		}
	}
}

func (s *ToolExecutionContextStore) removeEntryLocked(key toolExecutionContextKey) {
	if s.entries == nil {
		return
	}
	entry, ok := s.entries[key]
	if !ok {
		return
	}
	delete(s.entries, key)
	if s.order != nil && entry != nil && entry.element != nil {
		s.order.Remove(entry.element)
	}
}

func (s *ToolExecutionContextStore) maybeExpireLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}
	if !s.nextExpireAt.IsZero() && now.Before(s.nextExpireAt) {
		return
	}
	s.expireLocked(now)
	interval := s.ttl / 10
	if interval <= 0 {
		interval = s.ttl
	}
	if interval > maxToolExecutionContextExpireInterval {
		interval = maxToolExecutionContextExpireInterval
	}
	s.nextExpireAt = now.Add(interval)
}

func (s *ToolExecutionContextStore) expireLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}
	for key, entry := range s.entries {
		if entry == nil || now.Sub(entry.ctx.CreatedAt) > s.ttl {
			s.removeEntryLocked(key)
		}
	}
}

func (s *ToolExecutionContextStore) enforceMaxEntriesLocked() {
	if s.maxEntries <= 0 || len(s.entries) <= s.maxEntries {
		return
	}
	s.ensureInitializedLocked()
	for len(s.entries) > s.maxEntries {
		front := s.order.Front()
		if front == nil {
			for key := range s.entries {
				delete(s.entries, key)
				break
			}
			continue
		}
		key, ok := front.Value.(toolExecutionContextKey)
		s.order.Remove(front)
		if !ok {
			continue
		}
		delete(s.entries, key)
	}
}
