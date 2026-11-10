import XCTest
@testable import VekilCore

final class StatsRequestProjectionTests: XCTestCase {
    func testFailoverFixtureJoinsAndSortsAttemptsWithinOneCapture() throws {
        let capture = try makeCapture(fixture: "stats-failover.json", generation: 7, sequence: 11)
        let rows = StatsRequestProjector.project(capture: capture)

        let row = try XCTUnwrap(rows.first)
        XCTAssertEqual(rows.count, 1)
        XCTAssertEqual(row.scope.runtimeGeneration, 7)
        XCTAssertEqual(row.attempts.map(\.sequence), [1, 2])
        XCTAssertEqual(row.attempts.map(\.targetID), ["primary", "secondary"])
        XCTAssertEqual(row.attemptCoverage, .complete)
        XCTAssertEqual(row.completenessLabel, "Complete")
    }

    func testAllErrorsAndFailoversFiltersUseDocumentedDefinitions() throws {
        let errors = try makeCapture(fixture: "stats-error-heavy.json", generation: 1, sequence: 1)
        XCTAssertEqual(StatsRequestProjector.project(capture: errors, filter: .all).count, 4)
        XCTAssertEqual(
            StatsRequestProjector.project(capture: errors, filter: .errors).map(\.request.status),
            [503, 429, 400]
        )
        XCTAssertEqual(StatsRequestProjector.project(capture: errors, filter: .failovers).count, 0)

        let failover = try makeCapture(fixture: "stats-failover.json", generation: 1, sequence: 2)
        XCTAssertEqual(StatsRequestProjector.project(capture: failover, filter: .failovers).count, 1)
        XCTAssertEqual(StatsRequestFilter.all.title, "All")
        XCTAssertEqual(StatsRequestFilter.errors.title, "Errors")
        XCTAssertEqual(StatsRequestFilter.failovers.title, "Failovers")
    }

    func testPartialFixtureLabelsAbsentFewerRedactedAndWebSocketLimitedRows() throws {
        let capture = try makeCapture(fixture: "stats-partial.json", generation: 3, sequence: 9)
        let rows = StatsRequestProjector.project(capture: capture)
        let byOperation = Dictionary(uniqueKeysWithValues: rows.map { ($0.request.operationID, $0) })

        XCTAssertEqual(
            byOperation["op-absent"]?.attemptCoverage,
            .partial([.attemptsAbsent(expected: 2)])
        )
        XCTAssertEqual(
            byOperation["op-fewer"]?.attemptCoverage,
            .partial([.fewerAttempts(expected: 3, observed: 1)])
        )
        XCTAssertEqual(
            byOperation["op-redacted"]?.attemptCoverage,
            .partial([.redactedAttemptDetails])
        )
        XCTAssertEqual(
            byOperation["op-ws"]?.attemptCoverage,
            .partial([.webSocketLimited])
        )
        XCTAssertEqual(byOperation["op-ws"]?.completenessLabel, "Partial")
    }

    func testAttemptJoinNeverCrossesRuntimeGenerationOrSnapshotIdentity() throws {
        let requests = try makeCapture(fixture: "stats-stale.json", generation: 2, sequence: 10)
        let priorGeneration = try makeCapture(fixture: "stats-prior-generation.json", generation: 1, sequence: 10)

        var row = try XCTUnwrap(StatsRequestProjector.project(
            requestsFrom: requests,
            attemptsFrom: priorGeneration
        ).first)
        XCTAssertEqual(row.attempts, [])
        XCTAssertEqual(
            row.attemptCoverage,
            .partial([.snapshotOrGenerationMismatch, .attemptsAbsent(expected: 1)])
        )

        let otherSnapshot = StatsCapture(
            scope: StatsSnapshotScope(runtimeGeneration: 2, runOrdinal: 1, snapshotSequence: 11),
            snapshot: priorGeneration.snapshot,
            capturedAt: priorGeneration.capturedAt
        )
        row = try XCTUnwrap(StatsRequestProjector.project(
            requestsFrom: requests,
            attemptsFrom: otherSnapshot
        ).first)
        XCTAssertEqual(row.attempts, [])
        XCTAssertTrue(row.isPartial)
    }

    func testRequestOrderingIsNewestFirstWithStableSourceOrderForTies() {
        let snapshot = StatsSnapshot(recent: [
            StatsRecentRequest(timestamp: 100, operationID: "first-tie", status: 200),
            StatsRecentRequest(timestamp: 101, operationID: "newest", status: 200),
            StatsRecentRequest(timestamp: 100, operationID: "second-tie", status: 200)
        ])
        let capture = StatsCapture(
            scope: StatsSnapshotScope(runtimeGeneration: 1, runOrdinal: 1, snapshotSequence: 1),
            snapshot: snapshot,
            capturedAt: Date(timeIntervalSince1970: 200)
        )

        XCTAssertEqual(
            StatsRequestProjector.project(capture: capture).map(\.request.operationID),
            ["newest", "first-tie", "second-tie"]
        )
    }
}
