import Foundation
import XCTest

@testable import VekilCore

final class RuntimeSecretProjectionPreparerTests: XCTestCase {
  func testBuildsCompleteWriteOnlyProjectionFromHelperRequirements() async throws {
    let providerUUID = UUID(uuidString: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")!
    let identity = ProviderSecretIdentity(providerID: providerUUID, role: .apiKey)
    let reference = ProviderSecretReference(identity: identity, generation: 4)
    let store = InMemoryKeychainSecretStore(secrets: [reference: Data("secret-value".utf8)])
    let preparer = RuntimeSecretProjectionPreparer(
      manager: KeychainSecretGenerationManager(store: store)
    )
    let requirement = RuntimeSecretProjectionRequirement(
      configRevision: " cfg_test ",
      secretGeneration: 4,
      secrets: [
        RuntimeManagedSecretRequirement(
          providerID: " upstream ",
          providerUUID: providerUUID.uuidString,
          role: "api_key",
          reference: " VEKIL_MANAGED_UPSTREAM_API_KEY_4 "
        )
      ]
    )

    let requests = try await preparer.requests(for: [requirement])

    XCTAssertEqual(requests.count, 1)
    XCTAssertEqual(requests[0].command, .setSecretProjection)
    XCTAssertEqual(
      requests[0].payload,
      .object([
        "config_revision": .string("cfg_test"),
        "secret_generation": .unsignedInteger(4),
        "secrets": .array([
          .object([
            "provider_id": .string("upstream"),
            "reference": .string("VEKIL_MANAGED_UPSTREAM_API_KEY_4"),
            "value": .string("secret-value"),
          ])
        ]),
      ])
    )
  }

  func testRejectsIncompleteGeneration() async throws {
    let providerUUID = UUID(uuidString: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")!
    let store = InMemoryKeychainSecretStore()
    let preparer = RuntimeSecretProjectionPreparer(
      manager: KeychainSecretGenerationManager(store: store)
    )
    let requirement = RuntimeSecretProjectionRequirement(
      configRevision: "cfg_test",
      secretGeneration: 2,
      secrets: [
        RuntimeManagedSecretRequirement(
          providerID: "upstream",
          providerUUID: providerUUID.uuidString,
          role: "api_key",
          reference: "VEKIL_MANAGED_UPSTREAM_API_KEY_2"
        )
      ]
    )

    do {
      _ = try await preparer.requests(for: [requirement])
      XCTFail("Expected incomplete Keychain generation")
    } catch {
      XCTAssertEqual(error as? SecretGenerationStoreError, .incompleteGeneration)
    }
  }

  func testEmptyRequirementsDoNotAccessKeychain() async throws {
    let preparer = RuntimeSecretProjectionPreparer(manager: FailingProjectionManager())
    let requests = try await preparer.requests(for: [])
    XCTAssertEqual(requests, [])
  }
}

private actor FailingProjectionManager: SecretGenerationManaging {
  func stage(_ candidate: CompleteSecretGeneration) async throws -> StagedSecretGeneration {
    throw ProjectionTestError.unexpectedAccess
  }

  func projection(for staged: StagedSecretGeneration) async throws -> SecretProjection {
    throw ProjectionTestError.unexpectedAccess
  }

  func releaseLease(for staged: StagedSecretGeneration) async {}

  func removeGeneration(_ generation: UInt64) async throws {
    throw ProjectionTestError.unexpectedAccess
  }

  func cleanup(retainingGenerations: Set<UInt64>) async throws {
    throw ProjectionTestError.unexpectedAccess
  }
}

private enum ProjectionTestError: Error {
  case unexpectedAccess
}
