package macosruntime

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSecretProjectionSetIsIdempotentButImmutable(t *testing.T) {
	store := NewSecretProjectionStore()
	projection := SecretProjection{
		ConfigRevision:   " cfg_test ",
		SecretGeneration: 7,
		Secrets: []SecretValue{{
			ProviderID: " provider ",
			Reference:  " VEKIL_MANAGED_PROVIDER_API_KEY_7 ",
			Value:      "first-secret",
		}},
	}

	if err := store.Set(projection); err != nil {
		t.Fatalf("Set(first) error = %v", err)
	}
	if err := store.Set(projection); err != nil {
		t.Fatalf("Set(idempotent) error = %v", err)
	}

	conflicting := projection
	conflicting.Secrets = append([]SecretValue(nil), projection.Secrets...)
	conflicting.Secrets[0].Value = "second-secret"
	err := store.Set(conflicting)
	if err == nil || !strings.Contains(err.Error(), "immutable generation") {
		t.Fatalf("Set(conflicting) error = %v, want immutable-generation rejection", err)
	}
	if strings.Contains(err.Error(), "first-secret") || strings.Contains(err.Error(), "second-secret") {
		t.Fatalf("Set(conflicting) leaked a secret: %v", err)
	}

	value, ok := store.Resolver("cfg_test", 7).ResolveProviderSecret(
		"provider", "VEKIL_MANAGED_PROVIDER_API_KEY_7",
	)
	if !ok || value != "first-secret" {
		t.Fatalf("resolver = (%q, %v), want original immutable value", value, ok)
	}
}

func TestSecretProjectionStoreBoundsAggregateGenerationsAndBytes(t *testing.T) {
	store := NewSecretProjectionStore()
	for generation := uint64(1); generation <= maxSecretProjectionGenerations; generation++ {
		if err := store.Set(SecretProjection{
			ConfigRevision:   fmt.Sprintf("cfg_%d", generation),
			SecretGeneration: generation,
		}); err != nil {
			t.Fatalf("Set(generation %d): %v", generation, err)
		}
	}
	if err := store.Set(SecretProjection{
		ConfigRevision:   "cfg_overflow",
		SecretGeneration: maxSecretProjectionGenerations + 1,
	}); err == nil || !strings.Contains(err.Error(), "generations") {
		t.Fatalf("generation overflow error = %v", err)
	}

	store = NewSecretProjectionStore()
	value := strings.Repeat("s", (maxSecretProjectionStoreBytes/4)-128)
	for generation := uint64(1); generation <= 4; generation++ {
		if err := store.Set(SecretProjection{
			ConfigRevision:   fmt.Sprintf("cfg_bytes_%d", generation),
			SecretGeneration: generation,
			Secrets: []SecretValue{{
				ProviderID: "provider",
				Reference:  fmt.Sprintf("SECRET_%d", generation),
				Value:      value,
			}},
		}); err != nil {
			t.Fatalf("Set(byte generation %d): %v", generation, err)
		}
	}
	if err := store.Set(SecretProjection{
		ConfigRevision:   "cfg_bytes_overflow",
		SecretGeneration: 5,
		Secrets: []SecretValue{{
			ProviderID: "provider",
			Reference:  "SECRET_5",
			Value:      value,
		}},
	}); err == nil || !strings.Contains(err.Error(), "store exceeds") {
		t.Fatalf("aggregate byte overflow error = %v", err)
	}
	store.DeleteGeneration(1)
	if err := store.Set(SecretProjection{
		ConfigRevision:   "cfg_bytes_replacement",
		SecretGeneration: 6,
		Secrets: []SecretValue{{
			ProviderID: "provider",
			Reference:  "SECRET_6",
			Value:      value,
		}},
	}); err != nil {
		t.Fatalf("Set() after DeleteGeneration: %v", err)
	}
}

func TestManagedSecretProjectionRequirementsDescribeOwnedGenerationOutsideManagedMode(t *testing.T) {
	state := PersistentState{
		ConfigMode:              ConfigModeExternal,
		ManagedOwnershipID:      "owner",
		CommittedConfigRevision: "cfg_managed",
		SecretGeneration:        9,
		Providers: []ProviderIdentity{
			{UUID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", ProviderID: "zeta", SecretRoles: []string{"api_key"}},
			{UUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", ProviderID: "alpha", SecretRoles: []string{" bearer_token ", "api_key", "api_key"}},
		},
	}

	got := managedSecretProjectionRequirements(state)
	want := []SecretProjectionRequirement{{
		ConfigRevision:   "cfg_managed",
		SecretGeneration: 9,
		Secrets: []ManagedSecretRequirement{
			{
				ProviderID:   "alpha",
				ProviderUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
				Role:         "api_key",
				Reference:    "VEKIL_MANAGED_AAAAAAAA_AAAA_AAAA_AAAA_AAAAAAAAAAAA_API_KEY_9",
			},
			{
				ProviderID:   "alpha",
				ProviderUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
				Role:         "bearer_token",
				Reference:    "VEKIL_MANAGED_AAAAAAAA_AAAA_AAAA_AAAA_AAAAAAAAAAAA_BEARER_TOKEN_9",
			},
			{
				ProviderID:   "zeta",
				ProviderUUID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
				Role:         "api_key",
				Reference:    "VEKIL_MANAGED_BBBBBBBB_BBBB_BBBB_BBBB_BBBBBBBBBBBB_API_KEY_9",
			},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managedSecretProjectionRequirements() = %#v, want %#v", got, want)
	}

	state.SecretGeneration = 0
	if got := managedSecretProjectionRequirements(state); got != nil {
		t.Fatalf("zero-generation requirements = %#v, want nil", got)
	}
}

func TestRecoveryDescriptionRetainsBothSecretGenerations(t *testing.T) {
	manager := newManagerForTest(t, "owner", "copilot-uuid")
	description, err := manager.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	oldProviders := []ProviderIdentity{{
		UUID:        "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		ProviderID:  "old",
		SecretRoles: []string{"api_key"},
	}}
	newProviders := []ProviderIdentity{{
		UUID:        "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		ProviderID:  "new",
		SecretRoles: []string{"bearer_token"},
	}}
	journal := ApplyJournal{
		Version:             ApplyJournalVersion,
		OperationID:         "op_recovery",
		Phase:               ApplyPhaseRollbackFailed,
		OldRevision:         description.SelectedRevision,
		NewRevision:         "cfg_candidate",
		OldSecretGeneration: 4,
		NewSecretGeneration: 5,
		OldProviders:        oldProviders,
		NewProviders:        newProviders,
	}
	if err := writeApplyJournal(manager.paths.Journal, journal); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.state.SecretGeneration = 4
	manager.state.Providers = oldProviders
	manager.state.RecoveryState = string(ApplyPhaseRollbackFailed)
	manager.mu.Unlock()

	recovered, err := manager.Describe()
	if err != nil {
		t.Fatal(err)
	}
	want := []SecretProjectionRequirement{
		{
			ConfigRevision:   "cfg_candidate",
			SecretGeneration: 5,
			Secrets: []ManagedSecretRequirement{{
				ProviderID:   "new",
				ProviderUUID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
				Role:         "bearer_token",
				Reference:    "VEKIL_MANAGED_BBBBBBBB_BBBB_BBBB_BBBB_BBBBBBBBBBBB_BEARER_TOKEN_5",
			}},
		},
		{
			ConfigRevision:   description.SelectedRevision,
			SecretGeneration: 4,
			Secrets: []ManagedSecretRequirement{{
				ProviderID:   "old",
				ProviderUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
				Role:         "api_key",
				Reference:    "VEKIL_MANAGED_AAAAAAAA_AAAA_AAAA_AAAA_AAAAAAAAAAAA_API_KEY_4",
			}},
		},
	}
	if !reflect.DeepEqual(recovered.SecretProjections, want) {
		t.Fatalf("recovery secret projections = %#v, want %#v", recovered.SecretProjections, want)
	}
}
