package macosruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/proxy"
)

func TestRuntimeFactoryRejectsManagedSecretGenerationWithoutStore(t *testing.T) {
	factory, err := NewRuntimeFactory(RuntimeFactoryOptions{
		Authenticator: auth.NewTestAuthenticator("token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory.NewRuntime(context.Background(), appcontrol.Configuration{
		Revision:         "rev1",
		SecretGeneration: 1,
		Value:            proxy.ProvidersConfig{},
	})
	if err == nil || !strings.Contains(err.Error(), "secret projection store is required for managed secret generation") {
		t.Fatalf("expected managed secret generation store error, got %v", err)
	}
}
