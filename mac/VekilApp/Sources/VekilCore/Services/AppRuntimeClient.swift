import Foundation

/// State-facing runtime boundary. A Runtime/Protocol implementation can adapt
/// its wire models into these public, forward-compatible value types without
/// making the UI state target depend on transport implementation details.
///
/// Mutation methods represent admission of asynchronous runtime operations and
/// must return promptly after the helper accepts or rejects the command. The
/// authoritative lifecycle continues to arrive through `events()` and
/// `refreshState()`; callers must not substitute HTTP reachability for it.
public protocol AppRuntimeClient: Sendable {
  func events() async -> AsyncStream<AppRuntimeClientEvent>

  /// Completes only after helper handshake and configuration recovery have
  /// both completed. This is the gate after which launch auto-start may be
  /// evaluated.
  func initialize() async throws -> AppRuntimeInitialization

  func refreshState() async throws -> AppRuntimeStateSnapshot

  func start(_ request: AppRuntimeStartRequest) async throws -> AppRuntimeOperationAcceptance
  func cancelOperation(id: String) async throws
  func stop() async throws -> AppRuntimeOperationAcceptance

  /// A high-level composite operation. The adapter owns stop/start ordering
  /// and waits for the appropriate protocol transitions; app state does not
  /// infer a restart from HTTP or optimistically mutate service lifecycle.
  func restart(_ request: AppRuntimeStartRequest) async throws -> AppRuntimeOperationAcceptance

  func startDeviceAuthentication() async throws -> AppRuntimeOperationAcceptance
  func authenticateWithGitHubCLI() async throws -> AppRuntimeOperationAcceptance
  func signOut() async throws -> AppRuntimeOperationAcceptance

  /// These operations pass paths only. Runtime/Go owns opening, bounded
  /// reading, validation, revision checks, and running-intent preservation.
  func selectExternalConfiguration(path: String) async throws -> AppRuntimeOperationAcceptance
  func reloadExternalConfiguration() async throws -> AppRuntimeOperationAcceptance
  func clearExternalConfiguration() async throws -> AppRuntimeOperationAcceptance
}
