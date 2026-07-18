package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"runtime/debug"
	"sort"
	"strings"
)

const (
	policyFactGenerationVersion     = "coding-agent-facts-v1"
	policyFunctionGenerationVersion = "emit-policy-signals-v1"
	policyMapperGenerationVersion   = "coding-agent-mapper-v1"
)

func policyConfigGeneration(cfg ProvidersConfig) string {
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err == nil {
		cfg = validated.config
	}
	cfg = sanitizedPolicyGenerationConfig(cfg)
	return policyHashValue(cfg)
}

func policyProfileGeneration(profile PolicyProfileConfig, contract publicModelContract) string {
	return policyHashValue(struct {
		Profile  PolicyProfileConfig `json:"profile"`
		Contract json.RawMessage     `json:"contract"`
		Routes   []string            `json:"routes"`
	}{
		Profile:  clonePolicyProfileConfig(profile),
		Contract: append(json.RawMessage(nil), contract.raw...),
		Routes:   []string{profile.LightweightRoute, profile.PowerfulRoute},
	})
}

func policyClassifierGeneration(route *modelRoute) string {
	var target struct {
		ID            string `json:"id"`
		Provider      string `json:"provider"`
		UpstreamModel string `json:"upstream_model"`
	}
	routeID := ""
	if route != nil {
		routeID = route.public.routeID
		if first, ok := route.primaryTarget(); ok {
			target.ID = first.id
			if first.provider != nil {
				target.Provider = first.provider.id
			}
			target.UpstreamModel = first.upstreamModel
		}
	}
	return policyHashValue(struct {
		RouteID        string      `json:"route_id"`
		Target         interface{} `json:"target"`
		FactSchema     string      `json:"fact_schema"`
		FunctionSchema string      `json:"function_schema"`
		Mapper         string      `json:"mapper"`
	}{routeID, target, policyFactGenerationVersion, policyFunctionGenerationVersion, policyMapperGenerationVersion})
}

func policyBinaryGeneration() string {
	version := "devel"
	revision := ""
	if info, ok := debug.ReadBuildInfo(); ok && info != nil {
		if strings.TrimSpace(info.Main.Version) != "" {
			version = strings.TrimSpace(info.Main.Version)
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = strings.TrimSpace(setting.Value)
				break
			}
		}
	}
	return policyHashValue(struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
	}{version, revision})
}

func sanitizedPolicyGenerationConfig(cfg ProvidersConfig) ProvidersConfig {
	cfg = cloneProvidersConfigForValidation(cfg)
	for providerIndex := range cfg.Providers {
		provider := &cfg.Providers[providerIndex]
		provider.APIKey = ""
		provider.BaseURL = sanitizedPolicyGenerationBaseURL(provider.BaseURL)
		if provider.ExtraHeaders != nil {
			redacted := make(map[string]string, len(provider.ExtraHeaders))
			for key := range provider.ExtraHeaders {
				redacted[key] = "<redacted>"
			}
			provider.ExtraHeaders = redacted
		}
		sort.Strings(provider.IncludeModels)
		sort.Strings(provider.ExcludeModels)
		for modelIndex := range provider.Models {
			sort.Strings(provider.Models[modelIndex].Endpoints)
		}
	}
	for routeIndex := range cfg.ModelRoutes {
		sort.Strings(cfg.ModelRoutes[routeIndex].Endpoints)
	}
	return cfg
}

func sanitizedPolicyGenerationBaseURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return baseURL
	}
	parsed.User = nil
	return parsed.String()
}

func policyHashValue(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
