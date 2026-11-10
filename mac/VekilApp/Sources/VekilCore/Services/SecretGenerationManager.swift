import Foundation

/// A complete candidate generation. The caller supplies every managed secret
/// that should exist in the generation; staging removes stale accounts for the
/// same generation and verifies an exact read-back before returning success.
public struct CompleteSecretGeneration: Equatable, Sendable, CustomStringConvertible,
  CustomDebugStringConvertible
{
  public var generation: UInt64
  public var secrets: [ProviderSecretIdentity: Data]

  public init(generation: UInt64, secrets: [ProviderSecretIdentity: Data]) {
    self.generation = generation
    self.secrets = secrets
  }

  public var description: String {
    "CompleteSecretGeneration(generation: \(generation), secretCount: \(secrets.count))"
  }

  public var debugDescription: String { description }
}

public struct StagedSecretGeneration: Codable, Equatable, Sendable {
  public var generation: UInt64
  public var references: Set<ProviderSecretReference>

  public init(generation: UInt64, references: Set<ProviderSecretReference>) {
    self.generation = generation
    self.references = references
  }
}

/// Complete in-memory projection passed write-only to the runtime adapter.
/// Its descriptions expose only generation metadata, never secret bytes.
public struct SecretProjection: Equatable, Sendable, CustomStringConvertible,
  CustomDebugStringConvertible
{
  public var generation: UInt64
  public var secrets: [ProviderSecretIdentity: Data]

  public init(generation: UInt64, secrets: [ProviderSecretIdentity: Data]) {
    self.generation = generation
    self.secrets = secrets
  }

  public var description: String {
    "SecretProjection(generation: \(generation), secretCount: \(secrets.count))"
  }

  public var debugDescription: String { description }
}

public protocol SecretGenerationManaging: Sendable {
  func stage(_ candidate: CompleteSecretGeneration) async throws -> StagedSecretGeneration
  func projection(for staged: StagedSecretGeneration) async throws -> SecretProjection
  func releaseLease(for staged: StagedSecretGeneration) async
  func removeGeneration(_ generation: UInt64) async throws
  func cleanup(retainingGenerations: Set<UInt64>) async throws
}

public enum SecretGenerationStoreError: Error, Equatable, LocalizedError, Sendable {
  case preparationFailed
  case stagingFailed
  case stagingFailedAndCleanupFailed
  case verificationFailed
  case incompleteGeneration
  case cleanupFailed

  public var errorDescription: String? {
    switch self {
    case .preparationFailed:
      return "Could not prepare the Keychain secret generation."
    case .stagingFailed:
      return "Could not stage the complete Keychain secret generation."
    case .stagingFailedAndCleanupFailed:
      return
        "Secret generation staging failed and its partial Keychain data could not be fully removed."
    case .verificationFailed:
      return "The staged Keychain secret generation could not be verified."
    case .incompleteGeneration:
      return "The Keychain secret generation is incomplete."
    case .cleanupFailed:
      return "Obsolete Keychain secret generations could not be fully removed."
    }
  }
}

/// Serializes complete-generation Keychain transactions. Staging acquires a
/// generation lease so cleanup cannot remove a candidate before the paired
/// helper operation reaches a terminal state. The caller releases that lease
/// immediately before reconciling the terminal state, or removes the generation
/// explicitly when the helper never accepted the candidate.
public actor KeychainSecretGenerationManager: SecretGenerationManaging {
  private let store: any KeychainSecretStore
  private var leaseCounts: [UInt64: Int] = [:]

  public init(store: any KeychainSecretStore) {
    self.store = store
  }

  public func stage(_ candidate: CompleteSecretGeneration) async throws -> StagedSecretGeneration {
    acquireLease(for: candidate.generation)
    let desired: [ProviderSecretReference: Data] = Dictionary(
      uniqueKeysWithValues: candidate.secrets.map { identity, secret in
        (ProviderSecretReference(identity: identity, generation: candidate.generation), secret)
      }
    )
    let desiredReferences = Set(desired.keys)

    do {
      try await removeReferences(inGeneration: candidate.generation)
    } catch {
      releaseLease(forGeneration: candidate.generation)
      throw SecretGenerationStoreError.preparationFailed
    }

    do {
      for reference in desiredReferences.sorted(by: { $0.accountName < $1.accountName }) {
        guard let secret = desired[reference] else {
          throw SecretGenerationStoreError.verificationFailed
        }
        try await store.setSecret(secret, for: reference)
      }
      try await verifyExactGeneration(
        generation: candidate.generation,
        expected: desired
      )
    } catch {
      do {
        try await removeReferences(inGeneration: candidate.generation)
      } catch {
        releaseLease(forGeneration: candidate.generation)
        throw SecretGenerationStoreError.stagingFailedAndCleanupFailed
      }
      releaseLease(forGeneration: candidate.generation)
      if error is SecretGenerationStoreError {
        throw SecretGenerationStoreError.verificationFailed
      }
      throw SecretGenerationStoreError.stagingFailed
    }

    return StagedSecretGeneration(
      generation: candidate.generation,
      references: desiredReferences
    )
  }

  public func projection(for staged: StagedSecretGeneration) async throws -> SecretProjection {
    let actual = try await store.allReferences().filter { $0.generation == staged.generation }
    guard actual == staged.references else {
      throw SecretGenerationStoreError.incompleteGeneration
    }

    var secrets: [ProviderSecretIdentity: Data] = [:]
    for reference in staged.references.sorted(by: { $0.accountName < $1.accountName }) {
      guard let secret = try await store.secret(for: reference) else {
        throw SecretGenerationStoreError.incompleteGeneration
      }
      secrets[reference.identity] = secret
    }

    return SecretProjection(generation: staged.generation, secrets: secrets)
  }

  public func releaseLease(for staged: StagedSecretGeneration) async {
    releaseLease(forGeneration: staged.generation)
  }

  public func removeGeneration(_ generation: UInt64) async throws {
    leaseCounts.removeValue(forKey: generation)
    do {
      try await removeReferences(inGeneration: generation)
    } catch {
      throw SecretGenerationStoreError.cleanupFailed
    }
  }

  public func cleanup(retainingGenerations: Set<UInt64>) async throws {
    let references: Set<ProviderSecretReference>
    let leasesAtStart = Set(leaseCounts.keys)
    do {
      references = try await store.allReferences()
    } catch {
      throw SecretGenerationStoreError.cleanupFailed
    }

    var failed = false
    for reference
      in references
      .sorted(by: { $0.accountName < $1.accountName })
    {
      if retainingGenerations.contains(reference.generation)
        || leasesAtStart.contains(reference.generation)
        || leaseCounts[reference.generation] != nil
      {
        continue
      }
      do {
        try await store.removeSecret(for: reference)
      } catch {
        failed = true
      }
    }

    if failed {
      throw SecretGenerationStoreError.cleanupFailed
    }
  }

  private func acquireLease(for generation: UInt64) {
    leaseCounts[generation, default: 0] += 1
  }

  private func releaseLease(forGeneration generation: UInt64) {
    guard let count = leaseCounts[generation] else { return }
    if count <= 1 {
      leaseCounts.removeValue(forKey: generation)
    } else {
      leaseCounts[generation] = count - 1
    }
  }

  private func verifyExactGeneration(
    generation: UInt64,
    expected: [ProviderSecretReference: Data]
  ) async throws {
    let actualReferences = try await store.allReferences().filter { $0.generation == generation }
    guard actualReferences == Set(expected.keys) else {
      throw SecretGenerationStoreError.verificationFailed
    }

    for (reference, expectedSecret) in expected {
      guard try await store.secret(for: reference) == expectedSecret else {
        throw SecretGenerationStoreError.verificationFailed
      }
    }
  }

  private func removeReferences(inGeneration generation: UInt64) async throws {
    let references = try await store.allReferences()
      .filter { $0.generation == generation }
      .sorted(by: { $0.accountName < $1.accountName })

    var firstError: Error?
    for reference in references {
      do {
        try await store.removeSecret(for: reference)
      } catch {
        if firstError == nil {
          firstError = error
        }
      }
    }

    if let firstError {
      throw firstError
    }
  }
}
