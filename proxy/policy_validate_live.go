package proxy

import (
	"context"
	"fmt"
	"os"

	"github.com/sozercan/vekil/auth"
)

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
		WithStateBindingsConfig(StateBindingsConfig{Mode: "memory"}),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
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
