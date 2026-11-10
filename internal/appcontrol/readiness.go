package appcontrol

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPReadinessChecker calls the existing /readyz compatibility endpoint.
type HTTPReadinessChecker struct {
	Client *http.Client
}

// CheckReadiness implements ReadinessChecker.
func (c HTTPReadinessChecker) CheckReadiness(ctx context.Context, addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errorsNewReadiness("runtime did not report a listen address")
	}
	base := addr
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("parse runtime address: %w", err)
	}
	u.Path = "/readyz"
	u.RawQuery = ""
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("create readiness request: %w", err)
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("readiness request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("readiness returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func errorsNewReadiness(message string) error { return fmt.Errorf("readiness: %s", message) }
