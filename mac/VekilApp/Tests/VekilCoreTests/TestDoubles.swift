import Foundation
import XCTest

@testable import VekilCore

actor RuntimeClientSpy: AppRuntimeClient {
  enum Call: Equatable, Sendable {
    case events
    case initialize
    case refresh
    case start(AppRuntimeStartRequest)
    case cancel(String)
    case stop
    case restart(AppRuntimeStartRequest)
    case restartHelper
    case deviceAuth
    case githubCLIAuth
    case signOut
    case selectExternal(String)
    case reloadExternal
    case clearExternal
  }

  private let stream: AsyncStream<AppRuntimeClientEvent>
  private let continuation: AsyncStream<AppRuntimeClientEvent>.Continuation

  var initialization: AppRuntimeInitialization
  var refreshedState: AppRuntimeStateSnapshot
  var calls: [Call] = []
  var initializationDelayNanoseconds: UInt64 = 0
  var thrownError: Error?
  var eventBeforeAcceptance: AppRuntimeClientEvent?
  private var operationCounter = 0

  init(state: AppRuntimeStateSnapshot = .connectedStopped) {
    var captured: AsyncStream<AppRuntimeClientEvent>.Continuation?
    stream = AsyncStream { captured = $0 }
    continuation = captured!
    initialization = AppRuntimeInitialization(state: state)
    refreshedState = state
  }

  func events() async -> AsyncStream<AppRuntimeClientEvent> {
    calls.append(.events)
    return stream
  }

  func initialize() async throws -> AppRuntimeInitialization {
    calls.append(.initialize)
    if initializationDelayNanoseconds > 0 {
      try await Task.sleep(nanoseconds: initializationDelayNanoseconds)
    }
    try throwIfConfigured()
    return initialization
  }

  func refreshState() async throws -> AppRuntimeStateSnapshot {
    calls.append(.refresh)
    try throwIfConfigured()
    return refreshedState
  }

  func start(_ request: AppRuntimeStartRequest) async throws -> AppRuntimeOperationAcceptance {
    calls.append(.start(request))
    try throwIfConfigured()
    return acceptance(kind: .start)
  }

  func cancelOperation(id: String) async throws {
    calls.append(.cancel(id))
    try throwIfConfigured()
  }

  func stop() async throws -> AppRuntimeOperationAcceptance {
    calls.append(.stop)
    try throwIfConfigured()
    return acceptance(kind: .stop)
  }

  func restart(_ request: AppRuntimeStartRequest) async throws -> AppRuntimeOperationAcceptance {
    calls.append(.restart(request))
    try throwIfConfigured()
    return acceptance(kind: .restart)
  }

  func restartHelper() async throws {
    calls.append(.restartHelper)
    try throwIfConfigured()
  }

  func startDeviceAuthentication() async throws -> AppRuntimeOperationAcceptance {
    calls.append(.deviceAuth)
    try throwIfConfigured()
    return acceptance(kind: .authDevice)
  }

  func authenticateWithGitHubCLI() async throws -> AppRuntimeOperationAcceptance {
    calls.append(.githubCLIAuth)
    try throwIfConfigured()
    return acceptance(kind: .authGitHubCLI)
  }

  func signOut() async throws -> AppRuntimeOperationAcceptance {
    calls.append(.signOut)
    try throwIfConfigured()
    return acceptance(kind: .authSignOut)
  }

  func selectExternalConfiguration(path: String) async throws -> AppRuntimeOperationAcceptance {
    calls.append(.selectExternal(path))
    try throwIfConfigured()
    return acceptance(kind: .selectExternalConfig)
  }

  func reloadExternalConfiguration() async throws -> AppRuntimeOperationAcceptance {
    calls.append(.reloadExternal)
    try throwIfConfigured()
    return acceptance(kind: .reloadExternalConfig)
  }

  func clearExternalConfiguration() async throws -> AppRuntimeOperationAcceptance {
    calls.append(.clearExternal)
    try throwIfConfigured()
    return acceptance(kind: .clearExternalConfig)
  }

  func emit(_ event: AppRuntimeClientEvent) {
    continuation.yield(event)
  }

  func finishEvents() {
    continuation.finish()
  }

  func recordedCalls() -> [Call] {
    calls
  }

  func setThrownError(_ error: Error?) {
    thrownError = error
  }

  func setInitializationDelay(nanoseconds: UInt64) {
    initializationDelayNanoseconds = nanoseconds
  }

  func setInitialization(_ state: AppRuntimeStateSnapshot) {
    initialization = AppRuntimeInitialization(state: state)
    refreshedState = state
  }

  func setEventBeforeAcceptance(_ event: AppRuntimeClientEvent?) {
    eventBeforeAcceptance = event
  }

  private func acceptance(kind: AppRuntimeOperationKind) -> AppRuntimeOperationAcceptance {
    operationCounter += 1
    if let eventBeforeAcceptance { continuation.yield(eventBeforeAcceptance) }
    return AppRuntimeOperationAcceptance(
      operation: AppRuntimeOperation(id: "op_\(operationCounter)", kind: kind)
    )
  }

  private func throwIfConfigured() throws {
    if let thrownError {
      throw thrownError
    }
  }
}

extension AppRuntimeStateSnapshot {
  static var connectedStopped: AppRuntimeStateSnapshot {
    AppRuntimeStateSnapshot(
      launchToken: UUID(uuidString: "11111111-1111-1111-1111-111111111111")!,
      helperEpoch: "hep_1",
      stateRevision: 1,
      helper: .connected,
      service: .stopped,
      readiness: .unknown,
      authentication: AppRuntimeAuthentication(state: .signedIn, source: .vekil),
      configuration: AppRuntimeConfigurationState(requiresGitHubAuthentication: true),
      baseURL: URL(string: "http://127.0.0.1:1337")
    )
  }
}

@MainActor
final class LoginItemServiceSpy: LoginItemService {
  var status: LoginItemStatus
  var currentStatusCount = 0
  var requestedValues: [Bool] = []
  var openSettingsCount = 0
  var error: Error?

  init(status: LoginItemStatus = .disabled) {
    self.status = status
  }

  func currentStatus() async -> LoginItemStatus {
    currentStatusCount += 1
    return status
  }

  func setEnabled(_ enabled: Bool) async throws -> LoginItemStatus {
    requestedValues.append(enabled)
    if let error {
      throw error
    }
    status = enabled ? .enabled : .disabled
    return status
  }

  func openSystemSettings() async throws {
    openSettingsCount += 1
    if let error {
      throw error
    }
  }
}

@MainActor
final class BrowserServiceSpy: BrowserService {
  var openedURLs: [URL] = []
  var error: Error?

  func open(_ url: URL) async throws {
    if let error {
      throw error
    }
    openedURLs.append(url)
  }
}

@MainActor
final class ClipboardServiceSpy: ClipboardService {
  var copiedStrings: [String] = []
  var error: Error?

  func copy(_ string: String) async throws {
    if let error {
      throw error
    }
    copiedStrings.append(string)
  }
}

@MainActor
final class UpdaterServiceSpy: UpdaterService {
  var isAvailable: Bool
  var checkCount = 0
  var error: Error?

  init(isAvailable: Bool = true) {
    self.isAvailable = isAvailable
  }

  func checkForUpdates() async throws {
    checkCount += 1
    if let error {
      throw error
    }
  }
}

@MainActor
final class ExternalConfigurationPathSelectorSpy: ExternalConfigurationPathSelecting {
  var selectedURL: URL?
  var selectionCount = 0
  var error: Error?

  init(selectedURL: URL? = nil) {
    self.selectedURL = selectedURL
  }

  func selectExternalConfigurationPath() async throws -> URL? {
    selectionCount += 1
    if let error {
      throw error
    }
    return selectedURL
  }
}

struct LeakyTestError: Error {
  let secret: String
}

@MainActor
func eventually(
  timeoutNanoseconds: UInt64 = 1_000_000_000,
  condition: @escaping @MainActor () async -> Bool
) async -> Bool {
  let start = ContinuousClock.now
  while !(await condition()) {
    if ContinuousClock.now - start > .nanoseconds(Int64(timeoutNanoseconds)) {
      return false
    }
    await Task.yield()
  }
  return true
}

@MainActor
func assertTrueAsync(
  _ expression: @autoclosure () async -> Bool,
  _ message: @autoclosure () -> String = "",
  file: StaticString = #filePath,
  line: UInt = #line
) async {
  let value = await expression()
  XCTAssertTrue(value, message(), file: file, line: line)
}

@MainActor
func assertFalseAsync(
  _ expression: @autoclosure () async -> Bool,
  _ message: @autoclosure () -> String = "",
  file: StaticString = #filePath,
  line: UInt = #line
) async {
  let value = await expression()
  XCTAssertFalse(value, message(), file: file, line: line)
}
