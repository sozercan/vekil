package launch

import (
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestApplyEnvironmentUnsetsSecretsAndAppliesOverrides(t *testing.T) {
	got := applyEnvironment(
		[]string{
			"PATH=/usr/bin",
			"UPSTREAM_SECRET=redacted",
			"ANTHROPIC_API_KEY=decoy-token",
			"KEEP=value",
		},
		[]string{"UPSTREAM_SECRET", "ANTHROPIC_API_KEY"},
		map[string]string{
			"ANTHROPIC_API_KEY": "placeholder",
			"NEW_VALUE":         "new",
		},
	)
	want := []string{
		"ANTHROPIC_API_KEY=placeholder",
		"KEEP=value",
		"NEW_VALUE=new",
		"PATH=/usr/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applyEnvironment() = %#v, want %#v", got, want)
	}
}

func TestApplyEnvironmentLastDuplicateWins(t *testing.T) {
	got := applyEnvironment(
		[]string{"DUP=first", "DUP=second"},
		nil,
		nil,
	)
	want := []string{"DUP=second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applyEnvironment() = %#v, want %#v", got, want)
	}
}

func TestEnsureLoopbackNoProxyPreservesExistingEntries(t *testing.T) {
	got := ensureLoopbackNoProxy(
		[]string{
			"HTTP_PROXY=http://proxy.example",
			"NO_PROXY=internal.example,127.0.0.1",
			"no_proxy=other.example",
		},
		"http://127.0.0.1:43210",
	)
	values := make(map[string]string)
	spellings := make(map[string]int)
	for _, raw := range got {
		key, value := splitEnvironmentEntry(raw)
		values[canonicalEnvironmentKey(key)] = value
		if strings.EqualFold(key, "NO_PROXY") {
			spellings[key]++
		}
	}
	value := values[canonicalEnvironmentKey("NO_PROXY")]
	for _, want := range []string{"internal.example", "other.example", "127.0.0.1", "localhost", "::1"} {
		if !strings.Contains(value, want) {
			t.Fatalf("NO_PROXY = %q, missing %q", value, want)
		}
	}
	if runtime.GOOS == "windows" {
		if len(spellings) != 1 {
			t.Fatalf("Windows environment contains duplicate case-insensitive NO_PROXY keys: %#v", spellings)
		}
	} else if spellings["NO_PROXY"] != 1 || spellings["no_proxy"] != 1 {
		t.Fatalf("Unix environment missing NO_PROXY spelling: %#v", spellings)
	}
	if values[canonicalEnvironmentKey("HTTP_PROXY")] != "http://proxy.example" {
		t.Fatalf("HTTP_PROXY = %q", values[canonicalEnvironmentKey("HTTP_PROXY")])
	}
}

func TestSplitEnvironmentEntryPreservesWindowsDriveDirectory(t *testing.T) {
	key, value := splitEnvironmentEntry(`=C:=C:\work`)
	if key != "=C:" || value != `C:\work` {
		t.Fatalf("splitEnvironmentEntry() = %q, %q", key, value)
	}
	got := applyEnvironment([]string{`=C:=C:\work`, "PATH=C:\\bin"}, nil, nil)
	if !slices.Contains(got, `=C:=C:\work`) {
		t.Fatalf("rebuilt environment lost drive entry: %#v", got)
	}
}
