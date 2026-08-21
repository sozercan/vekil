package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxRemoteProvidersConfigBodySize = 4 << 20
	remoteProvidersConfigTimeout     = 15 * time.Second
)

func readProvidersConfigSource(source string) ([]byte, error) {
	configURL, remote, err := parseProvidersConfigURL(source)
	if err != nil {
		return nil, err
	}
	if !remote {
		body, readErr := os.ReadFile(source)
		if readErr != nil {
			return nil, fmt.Errorf("read providers config %q: %w", source, readErr)
		}
		return body, nil
	}
	displaySource := ProvidersConfigSourceDisplay(source)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, configURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch providers config %q: %w", displaySource, providersConfigRequestError(err))
	}
	request.Header.Set("Accept", "*/*")

	client := &http.Client{
		Timeout: remoteProvidersConfigTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch providers config %q: %w", displaySource, providersConfigRequestError(err))
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch providers config %q: unexpected HTTP status %s", displaySource, response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxRemoteProvidersConfigBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("fetch providers config %q: read response: %w", displaySource, err)
	}
	if len(body) > maxRemoteProvidersConfigBodySize {
		return nil, fmt.Errorf("fetch providers config %q: response exceeds %d bytes", displaySource, maxRemoteProvidersConfigBodySize)
	}
	return body, nil
}

func parseProvidersConfigURL(source string) (*url.URL, bool, error) {
	if !strings.Contains(source, "://") {
		return nil, false, nil
	}

	parsed, err := url.Parse(source)
	if err != nil {
		return nil, false, fmt.Errorf("parse providers config URL %q: invalid URL", ProvidersConfigSourceDisplay(source))
	}
	displaySource := ProvidersConfigSourceDisplay(source)
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
	default:
		return nil, false, fmt.Errorf("providers config URL %q uses unsupported scheme %q", displaySource, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, false, fmt.Errorf("providers config URL %q has no host", displaySource)
	}
	parsed.Scheme = scheme
	return parsed, true, nil
}

// ProvidersConfigSourceDisplay returns a source suitable for user-visible
// output. Local paths are unchanged; URL credentials, queries, and fragments
// are omitted so signed URLs and userinfo are not exposed in logs or errors.
func ProvidersConfigSourceDisplay(source string) string {
	source = strings.TrimSpace(source)
	if !strings.Contains(source, "://") {
		return source
	}

	parsed, err := url.Parse(source)
	if err != nil {
		scheme, _, _ := strings.Cut(source, "://")
		return strings.ToLower(scheme) + "://<invalid>"
	}
	parsed.User = nil
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

func providersConfigRequestError(err error) error {
	var requestErr *url.Error
	if errors.As(err, &requestErr) && requestErr.Err != nil {
		return requestErr.Err
	}
	return err
}

func providersConfigSourceExtension(source string) string {
	if parsed, remote, err := parseProvidersConfigURL(source); err == nil && remote {
		source = parsed.Path
	}
	return strings.ToLower(filepath.Ext(source))
}
