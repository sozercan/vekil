import Foundation

public enum RuntimeSecretProjectionPreparationError: Error, Equatable, LocalizedError, Sendable {
  case invalidRequirement
  case duplicateRequirement
  case incompleteProjection
  case invalidSecretEncoding

  public var errorDescription: String? {
    switch self {
    case .invalidRequirement:
      return "The managed secret projection metadata is invalid."
    case .duplicateRequirement:
      return "The managed secret projection metadata contains a duplicate generation."
    case .incompleteProjection:
      return "The managed Keychain secret generation is incomplete."
    case .invalidSecretEncoding:
      return "A managed Keychain secret is not valid non-empty text."
    }
  }
}

/// Loads complete helper-described generations from Keychain and builds the
/// write-only requests used during runtime reconciliation. Secret values exist
/// only in the returned payload and are never included in errors or metadata.
public struct RuntimeSecretProjectionPreparer: Sendable {
  private let manager: any SecretGenerationManaging

  public init(manager: any SecretGenerationManaging) {
    self.manager = manager
  }

  public func requests(for context: RuntimeLaunchContext) async throws -> [RuntimePreparedRequest] {
    try await requests(for: context.state.configuration?.secretProjections ?? [])
  }

  public func requests(
    for requirements: [RuntimeSecretProjectionRequirement]
  ) async throws -> [RuntimePreparedRequest] {
    var seenGenerations = Set<UInt64>()
    var requests: [RuntimePreparedRequest] = []

    for requirement in requirements.sorted(by: requirementOrder) {
      let revision = requirement.configRevision.trimmingCharacters(in: .whitespacesAndNewlines)
      guard !revision.isEmpty, requirement.secretGeneration > 0 else {
        throw RuntimeSecretProjectionPreparationError.invalidRequirement
      }
      guard seenGenerations.insert(requirement.secretGeneration).inserted else {
        throw RuntimeSecretProjectionPreparationError.duplicateRequirement
      }

      let entries = try normalizedEntries(for: requirement)
      let staged = StagedSecretGeneration(
        generation: requirement.secretGeneration,
        references: Set(entries.map(\.reference))
      )
      let projection = try await manager.projection(for: staged)
      guard projection.generation == requirement.secretGeneration,
        Set(projection.secrets.keys) == Set(entries.map(\.identity))
      else {
        throw RuntimeSecretProjectionPreparationError.incompleteProjection
      }

      let secrets: [JSONValue] = try entries.map { entry in
        guard let data = projection.secrets[entry.identity],
          !data.isEmpty,
          let value = String(data: data, encoding: .utf8),
          !value.isEmpty
        else {
          throw RuntimeSecretProjectionPreparationError.invalidSecretEncoding
        }
        return .object([
          "provider_id": .string(entry.providerID),
          "reference": .string(entry.runtimeReference),
          "value": .string(value),
        ])
      }

      requests.append(
        RuntimePreparedRequest(
          command: .setSecretProjection,
          payload: .object([
            "config_revision": .string(revision),
            "secret_generation": .unsignedInteger(requirement.secretGeneration),
            "secrets": .array(secrets),
          ])
        )
      )
    }

    return requests
  }

  private func normalizedEntries(
    for requirement: RuntimeSecretProjectionRequirement
  ) throws -> [NormalizedEntry] {
    var identities = Set<ProviderSecretIdentity>()
    var runtimeKeys = Set<String>()
    var entries: [NormalizedEntry] = []

    for secret in requirement.secrets {
      let providerID = secret.providerID.trimmingCharacters(in: .whitespacesAndNewlines)
      let role = secret.role.trimmingCharacters(in: .whitespacesAndNewlines)
      let runtimeReference = secret.reference.trimmingCharacters(in: .whitespacesAndNewlines)
      guard !providerID.isEmpty,
        !role.isEmpty,
        !runtimeReference.isEmpty,
        let providerUUID = UUID(uuidString: secret.providerUUID)
      else {
        throw RuntimeSecretProjectionPreparationError.invalidRequirement
      }

      let identity = ProviderSecretIdentity(
        providerID: providerUUID,
        role: ProviderSecretRole(rawValue: role)
      )
      let runtimeKey = providerID + "\u{0}" + runtimeReference
      guard identities.insert(identity).inserted, runtimeKeys.insert(runtimeKey).inserted else {
        throw RuntimeSecretProjectionPreparationError.invalidRequirement
      }
      entries.append(
        NormalizedEntry(
          providerID: providerID,
          runtimeReference: runtimeReference,
          identity: identity,
          reference: ProviderSecretReference(
            identity: identity,
            generation: requirement.secretGeneration
          )
        )
      )
    }

    return entries.sorted {
      if $0.providerID != $1.providerID { return $0.providerID < $1.providerID }
      return $0.runtimeReference < $1.runtimeReference
    }
  }

  private func requirementOrder(
    _ left: RuntimeSecretProjectionRequirement,
    _ right: RuntimeSecretProjectionRequirement
  ) -> Bool {
    if left.configRevision != right.configRevision {
      return left.configRevision < right.configRevision
    }
    return left.secretGeneration < right.secretGeneration
  }

  private struct NormalizedEntry {
    let providerID: String
    let runtimeReference: String
    let identity: ProviderSecretIdentity
    let reference: ProviderSecretReference
  }
}
