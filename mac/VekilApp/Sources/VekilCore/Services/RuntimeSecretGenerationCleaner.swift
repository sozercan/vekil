import Foundation

/// Removes Keychain generations only after the helper publishes an idle,
/// authoritative state. Active managed operations keep their staged candidate
/// available until the paired terminal state identifies the committed or
/// rollback-required generations.
public actor RuntimeSecretGenerationCleaner {
  private let manager: any SecretGenerationManaging

  public init(manager: any SecretGenerationManaging) {
    self.manager = manager
  }

  public func reconcile(_ state: RuntimeStatePayload) async throws {
    guard case .idle = state.operation, let configuration = state.configuration else { return }
    let retained = Set(configuration.secretProjections.map(\.secretGeneration))
    try await manager.cleanup(retainingGenerations: retained)
  }
}
