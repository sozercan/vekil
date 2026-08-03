package server

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestNewContextRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewContext(ctx, auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), "127.0.0.1", "0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewContext() error = %v, want context canceled", err)
	}
}
