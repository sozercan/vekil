import Foundation
import XCTest

@testable import VekilCore

final class KeychainSecretStoreTests: XCTestCase {
  func testStableAccountNameRoundTripsProviderRoleAndGeneration() {
    let reference = ProviderSecretReference(
      providerID: UUID(uuidString: "11111111-2222-3333-4444-555555555555")!,
      role: ProviderSecretRole(rawValue: "header:X-API-Key/日本語"),
      generation: 42
    )

    let first = reference.accountName
    let second = reference.accountName

    XCTAssertEqual(first, second)
    XCTAssertTrue(first.hasPrefix("\(KeychainSecretAccount.formatPrefix):"))
    XCTAssertEqual(KeychainSecretAccount.reference(from: first), reference)
    XCTAssertFalse(first.contains("X-API-Key"), "Role delimiters and names should be encoded")
  }

  func testAccountNameChangesAcrossEveryIdentityDimension() {
    let providerA = UUID(uuidString: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")!
    let providerB = UUID(uuidString: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")!
    let base = ProviderSecretReference(providerID: providerA, role: .apiKey, generation: 1)

    XCTAssertNotEqual(
      base.accountName,
      ProviderSecretReference(providerID: providerB, role: .apiKey, generation: 1).accountName
    )
    XCTAssertNotEqual(
      base.accountName,
      ProviderSecretReference(providerID: providerA, role: .bearerToken, generation: 1).accountName
    )
    XCTAssertNotEqual(
      base.accountName,
      ProviderSecretReference(providerID: providerA, role: .apiKey, generation: 2).accountName
    )
  }

  func testMalformedAccountNamesAreRejected() {
    XCTAssertNil(KeychainSecretAccount.reference(from: "not-vekil"))
    XCTAssertNil(KeychainSecretAccount.reference(from: "vekil-provider-v1:not-a-uuid:1:YWJj"))
    XCTAssertNil(
      KeychainSecretAccount.reference(
        from: "vekil-provider-v1:11111111-2222-3333-4444-555555555555:nope:YWJj"))
  }

  func testInMemoryStoreSupportsCRUDAndReferenceEnumeration() async throws {
    let store = InMemoryKeychainSecretStore()
    let reference = ProviderSecretReference(providerID: UUID(), role: .apiKey, generation: 7)
    let first = Data("first".utf8)
    let second = Data("second".utf8)

    let initiallyStored = try await store.secret(for: reference)
    XCTAssertNil(initiallyStored)
    try await store.setSecret(first, for: reference)
    let storedFirst = try await store.secret(for: reference)
    let referencesAfterFirstWrite = try await store.allReferences()
    XCTAssertEqual(storedFirst, first)
    XCTAssertEqual(referencesAfterFirstWrite, [reference])

    try await store.setSecret(second, for: reference)
    let storedSecond = try await store.secret(for: reference)
    XCTAssertEqual(storedSecond, second)

    try await store.removeSecret(for: reference)
    let storedAfterRemoval = try await store.secret(for: reference)
    let referencesAfterRemoval = try await store.allReferences()
    XCTAssertNil(storedAfterRemoval)
    XCTAssertEqual(referencesAfterRemoval, [])
  }

  func testSecurityStoreUsesRequiredStableServiceIdentifier() async {
    let store = SecurityKeychainSecretStore()
    XCTAssertEqual(store.service, "com.vekil.menubar.providers")
    XCTAssertEqual(SecurityKeychainSecretStore.serviceIdentifier, "com.vekil.menubar.providers")
  }
}
