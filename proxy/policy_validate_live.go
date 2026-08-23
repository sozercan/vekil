package proxy

import (
	"context"
	"fmt"
	"os"

	"github.com/sozercan/vekil/auth"
)

// ValidateProvidersConfigFileLive performs ordinary structural validation and
// then executes the same classifier capability preflight used by server startup.
// Profiles configured off remain offline; observe/enforce profiles are probed.
func ValidateProvidersConfigFileLive(ctx context.Context, path string) error {
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		return err
	}
	authenticator, err := auth.NewAuthenticator(os.Getenv("TOKEN_DIR"))
	if err != nil {
		return fmt.Errorf("initialize live-validation authentication: %w", err)
	}
	h, err := NewProxyHandler(authenticator, nil,
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
		// Live validation is deliberately limited to classifier protocol
		// preflight; unrelated dynamic provider catalogs remain offline.
		WithDeferredDynamicProviderModelValidation(true),
	)
	if err != nil {
		return err
	}
	defer h.BeginShutdown()
	if err := h.InitializePolicyRouting(ctx); err != nil {
		return err
	}
	if diagnostic := h.PolicyRoutingReadinessDiagnostic(); diagnostic != "" {
		return fmt.Errorf("policy routing live validation failed: %s", diagnostic)
	}
	return nil
}
