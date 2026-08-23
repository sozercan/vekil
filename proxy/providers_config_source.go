package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// ProvidersConfigRevision returns the stable revision used to bind one exact
// provider-configuration byte snapshot across proxy frontends.
func ProvidersConfigRevision(body []byte) string {
	digest := sha256.Sum256(body)
	return "cfg_" + base64.RawURLEncoding.EncodeToString(digest[:16])
}

func readProvidersConfigSource(ctx context.Context, source string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL.String(), nil)
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
		status := fmt.Sprintf("%d", response.StatusCode)
		if statusText := http.StatusText(response.StatusCode); statusText != "" {
			status += " " + statusText
		}
		return nil, fmt.Errorf("fetch providers config %q: unexpected HTTP status %s", displaySource, status)
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

// IsRemoteProvidersConfigSource reports whether source uses URL syntax. The
// loader still validates the scheme and host before making a request.
func IsRemoteProvidersConfigSource(source string) bool {
	_, remote := providersConfigSourceScheme(strings.TrimSpace(source))
	return remote
}

func parseProvidersConfigURL(source string) (*url.URL, bool, error) {
	if _, remote := providersConfigSourceScheme(source); !remote {
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
	scheme, remote := providersConfigSourceScheme(source)
	if !remote {
		return source
	}

	parsed, err := url.Parse(source)
	if err != nil || parsed.Host == "" {
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

func providersConfigSourceScheme(source string) (string, bool) {
	if filepath.VolumeName(source) != "" || hasWindowsDrivePathPrefix(source) {
		return "", false
	}

	colon := strings.IndexByte(source, ':')
	if colon <= 0 {
		return "", false
	}
	scheme := source[:colon]
	schemeURL, err := url.Parse(scheme + ":")
	if err != nil || schemeURL.Scheme == "" {
		return "", false
	}
	if !strings.EqualFold(scheme, "http") &&
		!strings.EqualFold(scheme, "https") &&
		!strings.HasPrefix(source[colon+1:], "//") {
		return "", false
	}
	return scheme, true
}

func hasWindowsDrivePathPrefix(source string) bool {
	return len(source) >= 3 &&
		(source[0] >= 'a' && source[0] <= 'z' || source[0] >= 'A' && source[0] <= 'Z') &&
		source[1] == ':' &&
		(source[2] == '/' || source[2] == '\\')
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
