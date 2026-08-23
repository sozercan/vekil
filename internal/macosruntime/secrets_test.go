package macosruntime

import (
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
