import XCTest

@testable import VekilCore

final class RuntimeSecretGenerationCleanerTests: XCTestCase {
  func testIdleStateRetainsOnlyHelperRequiredGenerations() async throws {
    let manager = CleanupRecordingSecretManager()
    let cleaner = RuntimeSecretGenerationCleaner(manager: manager)
    let state = RuntimeStatePayload(
      service: .stopped,
      readiness: .unknown,
      auth: .notRequired,
      configuration: RuntimeConfigurationState(
        mode: .managed,
        secretProjections: [
          RuntimeSecretProjectionRequirement(
            configRevision: "cfg_old",
            secretGeneration: 4,
            secrets: []
          ),
          RuntimeSecretProjectionRequirement(
            configRevision: "cfg_candidate",
            secretGeneration: 5,
            secrets: []
          ),
        ]
      )
    )

    try await cleaner.reconcile(state)

    let cleanupCalls = await manager.cleanupCalls()
    XCTAssertEqual(cleanupCalls, [Set([4, 5])])
  }

  func testActiveOperationDefersCleanupUntilTerminalState() async throws {
    let manager = CleanupRecordingSecretManager()
    let cleaner = RuntimeSecretGenerationCleaner(manager: manager)
    let active = RuntimeStatePayload(
      service: .stopped,
      readiness: .unknown,
      auth: .notRequired,
      operation: .active(
        RuntimeOperationSummary(
          id: "op_validate",
          kind: RuntimeOperationKind("validate_managed_draft")
        )
      ),
      configuration: RuntimeConfigurationState(mode: .managed)
    )

    try await cleaner.reconcile(active)

    let cleanupCalls = await manager.cleanupCalls()
    XCTAssertEqual(cleanupCalls, [])
  }

  func testMissingConfigurationDefersCleanup() async throws {
    let manager = CleanupRecordingSecretManager()
    let cleaner = RuntimeSecretGenerationCleaner(manager: manager)
    let partial = RuntimeStatePayload(
      service: .stopped,
      readiness: .unknown,
      auth: .notRequired
    )

    try await cleaner.reconcile(partial)

    let cleanupCalls = await manager.cleanupCalls()
    XCTAssertEqual(cleanupCalls, [])
  }
}

private actor CleanupRecordingSecretManager: SecretGenerationManaging {
  private var calls: [Set<UInt64>] = []

  func stage(_ candidate: CompleteSecretGeneration) async throws -> StagedSecretGeneration {
    throw CleanupRecordingError.unexpectedCall
  }

  func projection(for staged: StagedSecretGeneration) async throws -> SecretProjection {
    throw CleanupRecordingError.unexpectedCall
  }

  func releaseLease(for staged: StagedSecretGeneration) async {}

  func removeGeneration(_ generation: UInt64) async throws {
    throw CleanupRecordingError.unexpectedCall
  }

  func cleanup(retainingGenerations: Set<UInt64>) async throws {
    calls.append(retainingGenerations)
  }

  func cleanupCalls() -> [Set<UInt64>] { calls }
}

private enum CleanupRecordingError: Error {
  case unexpectedCall
}
