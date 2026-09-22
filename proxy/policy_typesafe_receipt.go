package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"github.com/sozercan/vekil/logger"
)

type policyClassifierOperationIDContextKey struct{}

var policyTypeSafeGenerationIDPattern = regexp.MustCompile(`^gen_[A-Za-z0-9]{26}$`)

// Receipts describe completed physical HTTP exchanges, including failures. They
// do not imply that classification succeeded or that missing usage was free.
func (h *ProxyHandler) logPolicyTypeSafeReceipt(ctx context.Context, policyID, model string, resp *http.Response, body []byte) {
	if resp == nil || !h.log.Enabled(logger.LevelInfo) {
		return
	}
	bucket := "request"
	if policyStatsBucketFromContext(ctx) == policyStatsTrafficBucketPreflight {
		bucket = policyStatsTrafficBucketPreflight
	}
	fields := []logger.Field{
		logger.F("traffic_bucket", bucket),
		logger.F("policy_id", policyID),
		logger.F("model", model),
		logger.F("status_code", resp.StatusCode),
		logger.F("reported_usage", policyTypeSafeReportedUsage(resp.Body)),
	}
	if ctx != nil {
		if operationID, _ := ctx.Value(policyClassifierOperationIDContextKey{}).(string); operationID != "" {
			fields = append(fields, logger.F("operation_id", operationID))
		}
	}
	if generationID := policyTypeSafeGenerationID(body); generationID != "" {
		fields = append(fields, logger.F("generation_id", generationID))
	}
	h.log.Info("policy classifier request completed", fields...)
}

// Read the existing physical ledger after readPolicyClassifierHTTPResponse has
// closed the body. Reparsing JSON would disagree with partial-body accounting.
func policyTypeSafeReportedUsage(body io.ReadCloser) bool {
	if transport, ok := body.(*routeAttemptTransportBody); ok {
		body = transport.inner
	}
	if usage, ok := body.(*taskUsageBody); ok {
		usage.mu.Lock()
		defer usage.mu.Unlock()
		return usage.reported
	}
	return false
}

// Vercel's TypeSafe adapter uses camelCase metadata for errors and snake_case
// metadata for evaluated responses. Accept only these explicit paths and the
// bounded generation-ID syntax; never log arbitrary upstream metadata.
func policyTypeSafeGenerationID(body []byte) string {
	if len(body) > policyClassifierResponseLimit {
		return ""
	}
	root, err := decodePolicyClassifierObject(body)
	if err != nil {
		return ""
	}
	var generationID string
	for _, key := range []string{"providerMetadata", "provider_metadata"} {
		raw, present := root[key]
		if !present {
			continue
		}
		metadata, err := decodePolicyClassifierObject(raw)
		if err != nil {
			return ""
		}
		gateway, err := decodePolicyClassifierObject(metadata["gateway"])
		if err != nil {
			return ""
		}
		var candidate string
		if json.Unmarshal(gateway["generationId"], &candidate) != nil || !policyTypeSafeGenerationIDPattern.MatchString(candidate) {
			return ""
		}
		if generationID != "" && generationID != candidate {
			return ""
		}
		generationID = candidate
	}
	return generationID
}
