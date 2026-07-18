package launch

import (
	"net/url"
	"runtime"
	"sort"
	"strings"
)

type environmentEntry struct {
	key   string
	value string
}

func canonicalEnvironmentKey(key string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func splitEnvironmentEntry(entry string) (string, string) {
	if strings.HasPrefix(entry, "=") {
		if separator := strings.IndexByte(entry[1:], '='); separator >= 0 {
			separator++
			return entry[:separator], entry[separator+1:]
		}
	}
	key, value, ok := strings.Cut(entry, "=")
	if !ok {
		return entry, ""
	}
	return key, value
}

func applyEnvironment(base []string, unset []string, set map[string]string) []string {
	entries := make(map[string]environmentEntry, len(base)+len(set))
	for _, raw := range base {
		key, value := splitEnvironmentEntry(raw)
		if key == "" {
			continue
		}
		entries[canonicalEnvironmentKey(key)] = environmentEntry{key: key, value: value}
	}
	for _, key := range unset {
		delete(entries, canonicalEnvironmentKey(strings.TrimSpace(key)))
	}
	for key, value := range set {
		if strings.TrimSpace(key) == "" {
			continue
		}
		entries[canonicalEnvironmentKey(key)] = environmentEntry{key: key, value: value}
	}

	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		entry := entries[key]
		out = append(out, entry.key+"="+entry.value)
	}
	return out
}

func loopbackNoProxyValue(environment []string, baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return ""
	}

	values := make([]string, 0, 8)
	for _, raw := range environment {
		key, value := splitEnvironmentEntry(raw)
		if strings.EqualFold(key, "NO_PROXY") {
			values = append(values, strings.Split(value, ",")...)
		}
	}
	values = append(values, host, "localhost", "127.0.0.1", "::1")

	seen := make(map[string]struct{}, len(values))
	merged := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, value)
	}
	return strings.Join(merged, ",")
}

func ensureLoopbackNoProxy(environment []string, baseURL string) []string {
	joined := loopbackNoProxyValue(environment, baseURL)
	if joined == "" {
		return environment
	}
	return applyEnvironment(environment, nil, map[string]string{
		"NO_PROXY": joined,
		"no_proxy": joined,
	})
}
