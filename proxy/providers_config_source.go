package proxy

import (
	"context"
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

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, configURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch providers config %q: %w", source, err)
	}
	request.Header.Set("Accept", "application/json, application/yaml, text/yaml, text/plain")

	client := &http.Client{Timeout: remoteProvidersConfigTimeout}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch providers config %q: %w", source, err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch providers config %q: unexpected HTTP status %s", source, response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxRemoteProvidersConfigBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("fetch providers config %q: read response: %w", source, err)
	}
	if len(body) > maxRemoteProvidersConfigBodySize {
		return nil, fmt.Errorf("fetch providers config %q: response exceeds %d bytes", source, maxRemoteProvidersConfigBodySize)
	}
	return body, nil
}

func parseProvidersConfigURL(source string) (*url.URL, bool, error) {
	if !strings.Contains(source, "://") {
		return nil, false, nil
	}

	parsed, err := url.Parse(source)
	if err != nil {
		return nil, false, fmt.Errorf("parse providers config URL %q: %w", source, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
	default:
		return nil, false, fmt.Errorf("providers config URL %q uses unsupported scheme %q", source, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, false, fmt.Errorf("providers config URL %q has no host", source)
	}
	parsed.Scheme = scheme
	return parsed, true, nil
}

func providersConfigSourceExtension(source string) string {
	if parsed, remote, err := parseProvidersConfigURL(source); err == nil && remote {
		source = parsed.Path
	}
	return strings.ToLower(filepath.Ext(source))
}
