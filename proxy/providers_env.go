package proxy

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envVarPattern matches ${env:VAR_NAME} references in config content.
var envVarPattern = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvVars replaces all ${env:VAR} references in raw config bytes with
// their environment variable values. It returns an error if any referenced
// variable is not set. Backslash-escaped references (\${env:...}) are
// preserved as literal ${env:...} without expansion.
func expandEnvVars(content []byte) ([]byte, error) {
	s := string(content)

	// Collect all unescaped references and verify they are set.
	var missing []string
	seen := make(map[string]struct{})
	remainder := s
	for {
		loc := envVarPattern.FindStringIndex(remainder)
		if loc == nil {
			break
		}
		start := loc[0]
		// Check if preceded by backslash (escaped).
		if start > 0 && remainder[start-1] == '\\' {
			remainder = remainder[loc[1]:]
			continue
		}
		varName := envVarPattern.FindStringSubmatch(remainder[start:])[1]
		if _, ok := os.LookupEnv(varName); !ok {
			if _, dup := seen[varName]; !dup {
				missing = append(missing, varName)
				seen[varName] = struct{}{}
			}
		}
		remainder = remainder[loc[1]:]
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("provider config references undefined environment variable(s): %s", strings.Join(missing, ", "))
	}

	// Replace unescaped ${env:VAR} with the environment variable value.
	// Process the string character by character to properly handle escaping.
	var result strings.Builder
	result.Grow(len(s))
	i := 0
	for i < len(s) {
		loc := envVarPattern.FindStringIndex(s[i:])
		if loc == nil {
			result.WriteString(s[i:])
			break
		}
		absStart := i + loc[0]
		absEnd := i + loc[1]

		// Check if the match is preceded by a backslash.
		if absStart > 0 && s[absStart-1] == '\\' {
			// Write everything up to (but not including) the backslash.
			result.WriteString(s[i : absStart-1])
			// Write the literal ${env:...} without the backslash.
			result.WriteString(s[absStart:absEnd])
			i = absEnd
			continue
		}

		// Write everything before the match.
		result.WriteString(s[i:absStart])
		// Extract the variable name and substitute.
		varName := envVarPattern.FindStringSubmatch(s[absStart:absEnd])[1]
		result.WriteString(os.Getenv(varName))
		i = absEnd
	}

	return []byte(result.String()), nil
}
