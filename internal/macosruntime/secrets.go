package macosruntime

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/sozercan/vekil/proxy"
)

const (
	maxSecretProjectionEntries = 256
	maxSecretProjectionBytes   = 1 << 20
)

// SecretValue is accepted only on the write-only set_secret_projection path.
type SecretValue struct {
	ProviderID string `json:"provider_id"`
	Reference  string `json:"reference"`
	Value      string `json:"value"`
}

// SecretProjection is one complete immutable generation.
type SecretProjection struct {
	ConfigRevision   string        `json:"config_revision"`
	SecretGeneration uint64        `json:"secret_generation"`
	Secrets          []SecretValue `json:"secrets"`
}

type projectionKey struct {
	revision   string
	generation uint64
}

// SecretProjectionStore retains complete generations needed by the selected
// configuration and rollback transaction. It never writes secrets to disk or
// os.Environ.
type SecretProjectionStore struct {
	mu          sync.RWMutex
	projections map[projectionKey]map[string]string
}

// NewSecretProjectionStore creates an empty store.
func NewSecretProjectionStore() *SecretProjectionStore {
	return &SecretProjectionStore{projections: make(map[projectionKey]map[string]string)}
}

// Set atomically installs one complete immutable generation. Re-sending an
// identical projection is idempotent; conflicting values are rejected. Errors
// mention only field identity, never secret values.
func (s *SecretProjectionStore) Set(projection SecretProjection) error {
	if s == nil {
		return errors.New("secret projection store is unavailable")
	}
	revision := strings.TrimSpace(projection.ConfigRevision)
	if revision == "" {
		return errors.New("config_revision is required")
	}
	if projection.SecretGeneration == 0 {
		return errors.New("secret_generation must be greater than zero")
	}
	if len(projection.Secrets) > maxSecretProjectionEntries {
		return fmt.Errorf("secret projection contains more than %d entries", maxSecretProjectionEntries)
	}
	values := make(map[string]string, len(projection.Secrets))
	totalBytes := 0
	for index, secret := range projection.Secrets {
		providerID := strings.TrimSpace(secret.ProviderID)
		reference := strings.TrimSpace(secret.Reference)
		if providerID == "" {
			return fmt.Errorf("secrets[%d].provider_id is required", index)
		}
		if reference == "" {
			return fmt.Errorf("secrets[%d].reference is required", index)
		}
		if secret.Value == "" {
			return fmt.Errorf("secrets[%d].value is empty", index)
		}
		key := secretLookupKey(providerID, reference)
		if _, duplicate := values[key]; duplicate {
			return fmt.Errorf("secrets[%d] duplicates provider/reference", index)
		}
		totalBytes += len(providerID) + len(reference) + len(secret.Value)
		if totalBytes > maxSecretProjectionBytes {
			return fmt.Errorf("secret projection exceeds %d bytes", maxSecretProjectionBytes)
		}
		values[key] = secret.Value
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projections == nil {
		s.projections = make(map[projectionKey]map[string]string)
	}
	key := projectionKey{revision: revision, generation: projection.SecretGeneration}
	if existing, ok := s.projections[key]; ok {
		if maps.Equal(existing, values) {
			return nil
		}
		return errors.New("secret projection conflicts with existing immutable generation")
	}
	s.projections[key] = values
	return nil
}

// Resolver returns an immutable provider-scoped resolver. A missing projection
// still returns an authoritative empty resolver, preventing env fallback.
func (s *SecretProjectionStore) Resolver(configRevision string, generation uint64) proxy.ProviderSecretResolver {
	values := map[string]string{}
	if s != nil {
		s.mu.RLock()
		stored := s.projections[projectionKey{revision: strings.TrimSpace(configRevision), generation: generation}]
		values = make(map[string]string, len(stored))
		for key, value := range stored {
			values[key] = value
		}
		s.mu.RUnlock()
	}
	return immutableSecretResolver{values: values}
}

// Delete removes an obsolete generation after commit or successful rollback.
func (s *SecretProjectionStore) Delete(configRevision string, generation uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.projections, projectionKey{revision: strings.TrimSpace(configRevision), generation: generation})
	s.mu.Unlock()
}

// Has reports metadata only.
func (s *SecretProjectionStore) Has(configRevision string, generation uint64) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	_, ok := s.projections[projectionKey{revision: strings.TrimSpace(configRevision), generation: generation}]
	s.mu.RUnlock()
	return ok
}

type immutableSecretResolver struct{ values map[string]string }

func (r immutableSecretResolver) ResolveProviderSecret(providerID, reference string) (string, bool) {
	value, ok := r.values[secretLookupKey(providerID, reference)]
	return value, ok
}

func secretLookupKey(providerID, reference string) string {
	return strings.TrimSpace(providerID) + "\x00" + strings.TrimSpace(reference)
}
