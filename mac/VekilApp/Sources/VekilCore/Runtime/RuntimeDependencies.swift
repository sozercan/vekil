import Foundation

public protocol RuntimeClock: Sendable {
    func now() -> Date
    func sleep(for interval: TimeInterval) async throws
}

public struct SystemRuntimeClock: RuntimeClock {
    public init() {}

    public func now() -> Date { Date() }

    public func sleep(for interval: TimeInterval) async throws {
        guard interval > 0 else { return }
        let clamped = min(interval, Double(UInt64.max) / 1_000_000_000)
        try await Task.sleep(nanoseconds: UInt64(clamped * 1_000_000_000))
    }
}

public protocol RuntimeIDGenerator: Sendable {
    func nextRequestID() -> String
}

public struct UUIDRuntimeIDGenerator: RuntimeIDGenerator {
    public init() {}

    public func nextRequestID() -> String {
        "req_\(UUID().uuidString.lowercased())"
    }
}

public struct RuntimePreparedRequest: Sendable, Equatable {
    public var command: RuntimeCommand
    public var payload: JSONValue?

    public init(command: RuntimeCommand, payload: JSONValue? = nil) {
        self.command = command
        self.payload = payload
    }
}

public struct RuntimeLaunchContext: Sendable, Equatable {
    public var hello: RuntimeHelloPayload
    public var isAutomaticRestart: Bool
    public var automaticRestartAttempt: Int?

    public init(
        hello: RuntimeHelloPayload,
        isAutomaticRestart: Bool,
        automaticRestartAttempt: Int? = nil
    ) {
        self.hello = hello
        self.isAutomaticRestart = isAutomaticRestart
        self.automaticRestartAttempt = automaticRestartAttempt
    }
}

/// Produces launch-scoped control requests, such as the complete active secret
/// projection. Returned requests run after a valid hello and before `get_state`.
/// The closure must never log or persist secret-bearing payloads.
public typealias RuntimeLaunchPreparation = @Sendable (RuntimeLaunchContext) async throws -> [RuntimePreparedRequest]
