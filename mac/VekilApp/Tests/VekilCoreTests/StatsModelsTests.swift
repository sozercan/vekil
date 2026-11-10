import XCTest
@testable import VekilCore

final class StatsModelsTests: XCTestCase {
    func testEmptyFixtureDecodesZeroValueContract() throws {
        let snapshot = try decodeStatsFixture("stats-empty.json")

        XCTAssertEqual(snapshot, StatsSnapshot())
        XCTAssertEqual(snapshot.policyRouting.profiles, [])
        XCTAssertEqual(snapshot.policyRouting.totals.dropReasons, [])
    }

    func testNormalFixtureMatchesCurrentGoFields() throws {
        let snapshot = try decodeStatsFixture("stats-normal.json")

        XCTAssertEqual(snapshot.uptimeSeconds, 1_234)
        XCTAssertEqual(snapshot.inflight, 3)
        XCTAssertEqual(snapshot.totals.requests, 5_000)
        XCTAssertEqual(snapshot.totals.errors, 12)
        XCTAssertEqual(snapshot.totals.cachedTokens, 300_000)
        XCTAssertEqual(snapshot.totals.reasoningTokens, 80_000)
        XCTAssertEqual(snapshot.totals.latencyP99Milliseconds, 2_100)
        XCTAssertEqual(snapshot.status["4xx"], 8)
        XCTAssertEqual(snapshot.series.map(\.requests), [1, 2])
        XCTAssertEqual(snapshot.byRoute.first?.route, "gpt-5-4-route")
        XCTAssertEqual(snapshot.byTarget.first?.physicalUsage.totalTokens, 150_000)
        XCTAssertEqual(snapshot.physicalUsage.totalTokens, 1_516_000)
        XCTAssertEqual(snapshot.wastedUsage.totalTokens, 16_000)
        XCTAssertEqual(snapshot.upstreamAttempts, 5_012)
        XCTAssertEqual(snapshot.targetSwitches, 12)
        XCTAssertEqual(snapshot.requestsWithFailover, 10)
        XCTAssertEqual(snapshot.successfulFailovers, 8)
        XCTAssertEqual(snapshot.routeExhaustions, 2)
        XCTAssertEqual(snapshot.stateBindingHits, 120)
        XCTAssertEqual(snapshot.stateBindingMisses, 3)
        XCTAssertEqual(snapshot.stateBindingEvictions, 1)
        XCTAssertEqual(snapshot.retriesByCode.last?.label, "transport")
        XCTAssertTrue(snapshot.insightsEnabled)

        let request = try XCTUnwrap(snapshot.recent.first)
        XCTAssertEqual(request.operationID, "op-normal")
        XCTAssertEqual(request.upstreamSends, 1)
        XCTAssertEqual(request.upstreamRequestID, "req-abc")

        let attempt = try XCTUnwrap(snapshot.recentAttempts.first)
        XCTAssertEqual(attempt.sequence, 1)
        XCTAssertEqual(attempt.attemptKind, "normal")
        XCTAssertEqual(attempt.statusCode, 200)
        XCTAssertEqual(attempt.outcome, "succeeded")
        XCTAssertEqual(attempt.timeToFirstTokenMilliseconds, 42)
        XCTAssertEqual(attempt.retryAfterSeconds, 7)
        XCTAssertEqual(attempt.reportedUsage?.totalTokens, 900)
        XCTAssertFalse(attempt.isTopologyRedacted)

        let policy = try XCTUnwrap(snapshot.policyRouting.profiles.first)
        XCTAssertEqual(policy.profile, "coding-economy")
        XCTAssertEqual(policy.effectiveMode, "observe")
        XCTAssertEqual(policy.generationHashes.binary, "dddddddd")
        XCTAssertEqual(policy.totals.classifier.completion, 94)
        XCTAssertEqual(policy.totals.classifierLatency.p95Milliseconds, 70)
        XCTAssertEqual(policy.totals.classifierUsage.cachedInputTokens, 1_000)
        XCTAssertEqual(policy.trafficBuckets.first?.trafficBucket, "small/no-tools")
    }

    func testCodableRoundTripPreservesKnownContract() throws {
        let original = try decodeStatsFixture("stats-normal.json")
        let encoded = try JSONEncoder().encode(original)
        let decoded = try JSONDecoder().decode(StatsSnapshot.self, from: encoded)
        XCTAssertEqual(decoded, original)
    }

    func testTolerantDecoderDefaultsMissingNullAndWrongTypedFields() throws {
        let data = Data(#"""
        {
          "uptime_seconds": "12",
          "inflight": null,
          "totals": {"requests": "7", "errors": 2.0, "total_tokens": "99"},
          "status": {"2xx": "5", "4xx": 2, "bad": {}},
          "series": [null, {"t": "100", "req": "2", "err": 0}],
          "recent": [
            {"t": "101", "status": "429", "agent": "Codex CLI"},
            42
          ],
          "recent_attempts": null,
          "insights_enabled": "true",
          "policy_routing": {"totals": {"eligible": "3"}, "profiles": null},
          "unknown_future_field": [1, 2, 3]
        }
        """#.utf8)

        let snapshot = try JSONDecoder().decode(StatsSnapshot.self, from: data)
        XCTAssertEqual(snapshot.uptimeSeconds, 12)
        XCTAssertEqual(snapshot.inflight, 0)
        XCTAssertEqual(snapshot.totals.requests, 7)
        XCTAssertEqual(snapshot.totals.errors, 2)
        XCTAssertEqual(snapshot.totals.totalTokens, 99)
        XCTAssertEqual(snapshot.status, ["2xx": 5, "4xx": 2])
        XCTAssertEqual(snapshot.series.count, 1)
        XCTAssertEqual(snapshot.series.first?.timestamp, 100)
        XCTAssertEqual(snapshot.recent.count, 1)
        XCTAssertEqual(snapshot.recent.first?.status, 429)
        XCTAssertEqual(snapshot.recentAttempts, [])
        XCTAssertTrue(snapshot.insightsEnabled)
        XCTAssertEqual(snapshot.policyRouting.totals.eligible, 3)
        XCTAssertEqual(snapshot.policyRouting.profiles, [])
    }

    func testMalformedFixtureFailsAtJSONBoundary() throws {
        XCTAssertThrowsError(try JSONDecoder().decode(StatsSnapshot.self, from: fixtureData("stats-malformed.json")))
    }

    func testReadinessAndModelCatalogScaffoldingDecodeTolerantly() throws {
        var readiness = try JSONDecoder().decode(
            ReadinessResponse.self,
            from: Data(#"{"status":"not_ready","error":"provider validation pending","future":true}"#.utf8)
        )
        readiness.httpStatusCode = 503
        XCTAssertFalse(readiness.isReady)
        XCTAssertEqual(readiness.error, "provider validation pending")

        let modelsData = Data(#"""
        {
          "object": "list",
          "data": [
            {
              "id": "gpt-5.4",
              "object": "model",
              "created": "0",
              "owned_by": "azure-east",
              "name": "GPT-5.4",
              "supported_endpoints": ["/responses", 42],
              "capabilities": {
                "limits": {"max_context_window_tokens": "400000"},
                "supports": {"parallel_tool_calls": true, "reasoning_effort": ["low", "high"], "vision": "true"}
              },
              "model_picker_enabled": "true",
              "context_window": 272000,
              "max_context_window": 1000000,
              "auto_compact_token_limit": 244800,
              "effective_context_window_percent": 90
            },
            null
          ]
        }
        """#.utf8)
        let catalog = try JSONDecoder().decode(RuntimeModelCatalog.self, from: modelsData)
        let model = try XCTUnwrap(catalog.data.first)
        XCTAssertEqual(catalog.data.count, 1)
        XCTAssertEqual(model.id, "gpt-5.4")
        XCTAssertEqual(model.supportedEndpoints, ["/responses"])
        XCTAssertEqual(model.capabilities?.limits.maximumContextWindowTokens, 400_000)
        XCTAssertEqual(model.capabilities?.supports.reasoningEffort, ["low", "high"])
        XCTAssertEqual(model.capabilities?.supports.vision, true)
        XCTAssertEqual(model.modelPickerEnabled, true)
        XCTAssertEqual(model.effectiveContextWindowPercent, 90)
    }
}
