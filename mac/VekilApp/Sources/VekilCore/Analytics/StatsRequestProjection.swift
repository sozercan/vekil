import Foundation

/// Identity attached by the native shell to one decoded HTTP snapshot.
/// The Go payload has no runtime-generation field, so the shell must bind it at
/// capture time and retain that scope through all projections.
public struct StatsSnapshotScope: Hashable, Sendable {
    public var launchToken: UUID
    public var helperEpoch: String
    public var runtimeGeneration: UInt64
    public var runOrdinal: UInt64
    public var snapshotSequence: UInt64

    public init(
        launchToken: UUID = RuntimeLaunchIdentity.zero.launchToken,
        helperEpoch: String = RuntimeLaunchIdentity.zero.helperEpoch,
        runtimeGeneration: UInt64,
        runOrdinal: UInt64 = 0,
        snapshotSequence: UInt64
    ) {
        self.launchToken = launchToken
        self.helperEpoch = helperEpoch
        self.runtimeGeneration = runtimeGeneration
        self.runOrdinal = runOrdinal
        self.snapshotSequence = snapshotSequence
    }

    public var launchIdentity: RuntimeLaunchIdentity {
        RuntimeLaunchIdentity(launchToken: launchToken, helperEpoch: helperEpoch)
    }
}

public struct StatsCapture: Equatable, Sendable {
    public var scope: StatsSnapshotScope
    public var snapshot: StatsSnapshot
    public var capturedAt: Date

    public init(scope: StatsSnapshotScope, snapshot: StatsSnapshot, capturedAt: Date) {
        self.scope = scope
        self.snapshot = snapshot
        self.capturedAt = capturedAt
    }

    public var runtimeGeneration: UInt64 { scope.runtimeGeneration }
    public var age: TimeInterval { max(0, Date().timeIntervalSince(capturedAt)) }

    public func age(at date: Date) -> TimeInterval {
        max(0, date.timeIntervalSince(capturedAt))
    }
}

public enum StatsRequestFilter: String, CaseIterable, Codable, Sendable {
    case all
    case errors
    case failovers

    public var title: String {
        switch self {
        case .all: return "All"
        case .errors: return "Errors"
        case .failovers: return "Failovers"
        }
    }

    public func includes(_ request: StatsRecentRequest) -> Bool {
        switch self {
        case .all: return true
        case .errors: return request.status >= 400
        case .failovers: return request.targetSwitches > 0
        }
    }
}

public enum StatsAttemptPartialReason: Equatable, Sendable {
    case attemptsAbsent(expected: Int64)
    case fewerAttempts(expected: Int64, observed: Int)
    case redactedAttemptDetails
    case webSocketLimited
    case snapshotOrGenerationMismatch

    public var description: String {
        switch self {
        case let .attemptsAbsent(expected):
            return expected > 0 ? "Attempt trace absent (expected \(expected))" : "Attempt trace absent"
        case let .fewerAttempts(expected, observed):
            return "Attempt trace contains \(observed) of \(expected) sends"
        case .redactedAttemptDetails:
            return "Attempt details are redacted"
        case .webSocketLimited:
            return "WebSocket request topology is limited"
        case .snapshotOrGenerationMismatch:
            return "Attempt trace belongs to another snapshot or proxy run"
        }
    }
}

public enum StatsAttemptCoverage: Equatable, Sendable {
    case complete
    case partial([StatsAttemptPartialReason])

    public var isPartial: Bool {
        if case .partial = self { return true }
        return false
    }

    public var label: String { isPartial ? "Partial" : "Complete" }

    public var reasons: [StatsAttemptPartialReason] {
        if case let .partial(reasons) = self { return reasons }
        return []
    }
}

public struct StatsProjectedRequest: Equatable, Sendable {
    public var scope: StatsSnapshotScope
    public var sourceIndex: Int
    public var request: StatsRecentRequest
    public var attempts: [StatsRecentAttempt]
    public var attemptCoverage: StatsAttemptCoverage

    public init(
        scope: StatsSnapshotScope,
        sourceIndex: Int,
        request: StatsRecentRequest,
        attempts: [StatsRecentAttempt],
        attemptCoverage: StatsAttemptCoverage
    ) {
        self.scope = scope
        self.sourceIndex = sourceIndex
        self.request = request
        self.attempts = attempts
        self.attemptCoverage = attemptCoverage
    }

    public var isPartial: Bool { attemptCoverage.isPartial }
    public var completenessLabel: String { attemptCoverage.label }
}

/// Builds bounded request rows without ever borrowing attempt topology from a
/// different HTTP snapshot or runtime generation.
public enum StatsRequestProjector {
    public static func project(
        capture: StatsCapture,
        filter: StatsRequestFilter = .all
    ) -> [StatsProjectedRequest] {
        project(requestsFrom: capture, attemptsFrom: capture, filter: filter)
    }

    public static func project(
        requestsFrom requestCapture: StatsCapture,
        attemptsFrom attemptCapture: StatsCapture,
        filter: StatsRequestFilter = .all
    ) -> [StatsProjectedRequest] {
        let scopesMatch = requestCapture.scope == attemptCapture.scope
        let groupedAttempts = scopesMatch ? groupAttempts(attemptCapture.snapshot.recentAttempts) : [:]

        let indexedRequests = requestCapture.snapshot.recent.enumerated()
            .filter { filter.includes($0.element) }
            .sorted { left, right in
                if left.element.timestamp != right.element.timestamp {
                    return left.element.timestamp > right.element.timestamp
                }
                // Preserve the Go snapshot's newest-first order for equal-second
                // rows; the source index is a stable final tiebreaker.
                return left.offset < right.offset
            }

        return indexedRequests.map { indexed in
            let request = indexed.element
            let attempts: [StatsRecentAttempt]
            if scopesMatch, !request.operationID.isEmpty {
                attempts = groupedAttempts[request.operationID] ?? []
            } else {
                attempts = []
            }

            return StatsProjectedRequest(
                scope: requestCapture.scope,
                sourceIndex: indexed.offset,
                request: request,
                attempts: attempts,
                attemptCoverage: coverage(
                    for: request,
                    attempts: attempts,
                    scopesMatch: scopesMatch
                )
            )
        }
    }

    private static func groupAttempts(_ attempts: [StatsRecentAttempt]) -> [String: [StatsRecentAttempt]] {
        var indexed: [String: [(offset: Int, attempt: StatsRecentAttempt)]] = [:]
        for (offset, attempt) in attempts.enumerated() where !attempt.operationID.isEmpty {
            indexed[attempt.operationID, default: []].append((offset, attempt))
        }

        var grouped: [String: [StatsRecentAttempt]] = [:]
        grouped.reserveCapacity(indexed.count)
        for (operationID, rows) in indexed {
            grouped[operationID] = rows.sorted { left, right in
                if left.attempt.sequence != right.attempt.sequence {
                    return left.attempt.sequence < right.attempt.sequence
                }
                if left.attempt.timestamp != right.attempt.timestamp {
                    return left.attempt.timestamp < right.attempt.timestamp
                }
                if left.attempt.targetID != right.attempt.targetID {
                    return left.attempt.targetID < right.attempt.targetID
                }
                if left.attempt.providerID != right.attempt.providerID {
                    return left.attempt.providerID < right.attempt.providerID
                }
                return left.offset < right.offset
            }.map(\.attempt)
        }
        return grouped
    }

    private static func coverage(
        for request: StatsRecentRequest,
        attempts: [StatsRecentAttempt],
        scopesMatch: Bool
    ) -> StatsAttemptCoverage {
        var reasons: [StatsAttemptPartialReason] = []
        let websocketLimited = isWebSocketLimited(request.endpoint)
        if websocketLimited {
            reasons.append(.webSocketLimited)
        }

        var expected = max(request.upstreamSends, 0)
        if request.targetSwitches > 0 {
            expected = max(expected, request.targetSwitches + 1)
        }

        if expected > 0 {
            if !scopesMatch {
                reasons.append(.snapshotOrGenerationMismatch)
            }
            if attempts.isEmpty {
                reasons.append(.attemptsAbsent(expected: expected))
            } else if Int64(attempts.count) < expected {
                reasons.append(.fewerAttempts(expected: expected, observed: attempts.count))
            }
        }

        if attempts.contains(where: \.isTopologyRedacted) {
            reasons.append(.redactedAttemptDetails)
        }

        return reasons.isEmpty ? .complete : .partial(reasons)
    }

    private static func isWebSocketLimited(_ endpoint: String) -> Bool {
        switch endpoint.trimmingCharacters(in: .whitespacesAndNewlines).lowercased() {
        case "responses_ws", "responses_websocket", "openai_responses_ws":
            return true
        default:
            return false
        }
    }
}
