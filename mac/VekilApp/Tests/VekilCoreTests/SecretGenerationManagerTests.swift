import Foundation
import XCTest

@testable import VekilCore

final class SecretGenerationManagerTests: XCTestCase {
  func testStageCreatesAnExactCompleteGenerationAndRetainsOlderGeneration() async throws {
    let providerA = UUID(uuidString: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")!
    let providerB = UUID(uuidString: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")!
    let old = ProviderSecretReference(providerID: providerA, role: .apiKey, generation: 1)
    let staleInCandidateGeneration = ProviderSecretReference(
      providerID: providerB,
      role: ProviderSecretRole(rawValue: "obsolete"),
      generation: 2
    )
    let store = InMemoryKeychainSecretStore(secrets: [
      old: Data("old".utf8),
      staleInCandidateGeneration: Data("stale".utf8),
    ])
    let manager = KeychainSecretGenerationManager(store: store)
    let identityA = ProviderSecretIdentity(providerID: providerA, role: .apiKey)
    let identityB = ProviderSecretIdentity(providerID: providerB, role: .bearerToken)
    let candidate = CompleteSecretGeneration(
      generation: 2,
      secrets: [
        identityA: Data("new-a".utf8),
        identityB: Data("new-b".utf8),
      ])
    XCTAssertFalse(String(describing: candidate).contains("new-a"))

    let staged = try await manager.stage(candidate)
    let snapshot = await store.snapshot()

    XCTAssertEqual(
      snapshot[old], Data("old".utf8), "Prior generation must remain available for rollback")
    XCTAssertNil(snapshot[staleInCandidateGeneration])
    XCTAssertEqual(
      staged.references,
      Set([
        ProviderSecretReference(identity: identityA, generation: 2),
        ProviderSecretReference(identity: identityB, generation: 2),
      ]))

    let projection = try await manager.projection(for: staged)
    XCTAssertFalse(String(describing: projection).contains("new-a"))
    XCTAssertEqual(projection.generation, 2)
    XCTAssertEqual(projection.secrets, candidate.secrets)
  }

  func testEmptyGenerationRemovesStaleAccountsAndVerifiesAsComplete() async throws {
    let stale = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 9)
    let retained = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 8)
    let store = InMemoryKeychainSecretStore(secrets: [
      stale: Data("stale".utf8),
      retained: Data("retained".utf8),
    ])
    let manager = KeychainSecretGenerationManager(store: store)

    let staged = try await manager.stage(CompleteSecretGeneration(generation: 9, secrets: [:]))
    let projection = try await manager.projection(for: staged)
    let snapshot = await store.snapshot()
    XCTAssertEqual(staged.references, [])
    XCTAssertEqual(projection.secrets, [:])
    XCTAssertEqual(snapshot, [retained: Data("retained".utf8)])
  }

  func testFailedStageCleansEveryPartiallyWrittenSecret() async throws {
    let store = FailingKeychainSecretStore(failOnWriteNumber: 2)
    let manager = KeychainSecretGenerationManager(store: store)
    let candidate = CompleteSecretGeneration(
      generation: 4,
      secrets: [
        ProviderSecretIdentity(providerID: UUID(), role: .apiKey): Data("one".utf8),
        ProviderSecretIdentity(providerID: UUID(), role: .bearerToken): Data("two".utf8),
      ])

    do {
      _ = try await manager.stage(candidate)
      XCTFail("Expected staging to fail")
    } catch {
      XCTAssertEqual(error as? SecretGenerationStoreError, .stagingFailed)
    }

    let references = try await store.allReferences()
    XCTAssertEqual(references, [])
  }

  func testCleanupDeletesOnlyObsoleteGenerations() async throws {
    let generation1 = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 1)
    let generation2 = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 2)
    let generation3 = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 3)
    let store = InMemoryKeychainSecretStore(secrets: [
      generation1: Data("one".utf8),
      generation2: Data("two".utf8),
      generation3: Data("three".utf8),
    ])
    let manager = KeychainSecretGenerationManager(store: store)

    try await manager.cleanup(retainingGenerations: Set([1, 3]))

    let references = try await store.allReferences()
    XCTAssertEqual(references, Set([generation1, generation3]))
  }

  func testProjectionRejectsExtraOrMissingAccounts() async throws {
    let identity = ProviderSecretIdentity(providerID: UUID(), role: .apiKey)
    let expected = ProviderSecretReference(identity: identity, generation: 5)
    let extra = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 5)
    let store = InMemoryKeychainSecretStore(secrets: [
      expected: Data("expected".utf8),
      extra: Data("extra".utf8),
    ])
    let manager = KeychainSecretGenerationManager(store: store)
    let staged = StagedSecretGeneration(generation: 5, references: [expected])

    do {
      _ = try await manager.projection(for: staged)
      XCTFail("Expected an incomplete-generation error")
    } catch {
      XCTAssertEqual(error as? SecretGenerationStoreError, .incompleteGeneration)
    }
  }
}

private actor FailingKeychainSecretStore: KeychainSecretStore {
  private var secrets: [ProviderSecretReference: Data] = [:]
  private let failOnWriteNumber: Int
  private var writeCount = 0

  init(failOnWriteNumber: Int) {
    self.failOnWriteNumber = failOnWriteNumber
  }

  func secret(for reference: ProviderSecretReference) async throws -> Data? {
    secrets[reference]
  }

  func setSecret(_ secret: Data, for reference: ProviderSecretReference) async throws {
    writeCount += 1
    if writeCount == failOnWriteNumber {
      throw FailingStoreError.injected
    }
    secrets[reference] = secret
  }

  func removeSecret(for reference: ProviderSecretReference) async throws {
    secrets.removeValue(forKey: reference)
  }

  func allReferences() async throws -> Set<ProviderSecretReference> {
    Set(secrets.keys)
  }
}

private enum FailingStoreError: Error {
  case injected
}
