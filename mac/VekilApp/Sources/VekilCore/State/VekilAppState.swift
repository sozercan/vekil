import Combine
import Foundation

/// Main-actor composition root for menu and window presentation.
///
/// `runtimeState` remains an orthogonal protocol projection. All flattened UI
/// labels and actions are derived properties; no HTTP health request is used to
/// infer helper, service, operation, authentication, or configuration lifecycle.
@MainActor
public final class VekilAppState: ObservableObject {
  public static let currentOnboardingVersion = 1

  @Published public private(set) var initializationState: VekilInitializationState = .notStarted
  @Published public private(set) var runtimeState: AppRuntimeStateSnapshot = .placeholder
  @Published public private(set) var lastError: AppRuntimeStructuredError?
  @Published public private(set) var deviceCode: AppRuntimeDeviceCode?
  @Published public private(set) var helperBuild: String?
  @Published public private(set) var bundleBuildID: String?
  public let applicationVersion: String

  @Published public private(set) var selectedDestination: VekilDestination
  @Published public private(set) var mainWindowFrame: VekilWindowFrame?
  @Published public private(set) var openAtLogin: Bool
  @Published public private(set) var startProxyWhenAppLaunches: Bool
  @Published public private(set) var loginItemStatus: LoginItemStatus = .unavailable
  @Published public private(set) var isShowingOnboarding = false

  @Published public private(set) var isSubmittingCommand = false
  @Published public private(set) var isUpdatingOpenAtLogin = false
  @Published public private(set) var cancellationRequestedOperationID: String?

  public var presentation: VekilPresentation {
    VekilPresentationProjector.presentation(
      for: runtimeState,
      initialization: initializationState,
      persistentError: lastError
    )
  }

  public var primaryAction: VekilPrimaryAction {
    VekilPresentationProjector.primaryAction(
      for: runtimeState,
      initialization: initializationState,
      isSubmittingCommand: isSubmittingCommand,
      cancellationRequestedOperationID: cancellationRequestedOperationID
    )
  }

  public var baseURL: URL? {
    runtimeState.baseURL
  }

  public var dashboardURL: URL? {
    runtimeState.baseURL?.appendingPathComponent("dashboard")
  }

  public var updaterAvailable: Bool {
    updaterService.isAvailable
  }

  public var environmentTokenSignOutNotice: String? {
    guard runtimeState.authentication.source == .environment else {
      return nil
    }
    return
      "Vekil cannot remove an environment token. Remove it from the environment that launches Vekil."
  }

  private let runtimeClient: any AppRuntimeClient
  private let preferences: any VekilPreferencesStore
  private let loginItemService: any LoginItemService
  private let updaterService: any UpdaterService
  private let browserService: any BrowserService
  private let clipboardService: any ClipboardService
  private let externalConfigurationPathSelector: any ExternalConfigurationPathSelecting
  private let hadPersistedNativeSetupEvidence: Bool

  private var initializationTask: Task<Bool, Never>?
  private var eventTask: Task<Void, Never>?
  private var hasEvaluatedAutoStart = false
  private var retiredLaunchTokens = Set<UUID>()

  public init(
    runtimeClient: any AppRuntimeClient,
    preferences: (any VekilPreferencesStore)? = nil,
    loginItemService: (any LoginItemService)? = nil,
    updaterService: (any UpdaterService)? = nil,
    browserService: (any BrowserService)? = nil,
    clipboardService: (any ClipboardService)? = nil,
    externalConfigurationPathSelector: (any ExternalConfigurationPathSelecting)? = nil,
    applicationVersion: String = "Development"
  ) {
    let resolvedPreferences = preferences ?? UserDefaultsVekilPreferencesStore()
    let resolvedLoginItemService = loginItemService ?? SystemLoginItemService()
    let resolvedUpdaterService = updaterService ?? UnavailableUpdaterService()
    let resolvedBrowserService = browserService ?? SystemBrowserService()
    let resolvedClipboardService = clipboardService ?? SystemClipboardService()
    let resolvedPathSelector =
      externalConfigurationPathSelector ?? NoExternalConfigurationPathSelector()

    self.runtimeClient = runtimeClient
    self.preferences = resolvedPreferences
    self.loginItemService = resolvedLoginItemService
    self.updaterService = resolvedUpdaterService
    self.browserService = resolvedBrowserService
    self.clipboardService = resolvedClipboardService
    self.externalConfigurationPathSelector = resolvedPathSelector
    self.applicationVersion = applicationVersion
    hadPersistedNativeSetupEvidence =
      resolvedPreferences.mainWindowFrame != nil
      || resolvedPreferences.selectedDestination != .overview
      || resolvedPreferences.openAtLogin
      || resolvedPreferences.startProxyWhenAppLaunches

    selectedDestination = resolvedPreferences.selectedDestination
    mainWindowFrame = resolvedPreferences.mainWindowFrame
    openAtLogin = resolvedPreferences.openAtLogin
    startProxyWhenAppLaunches = resolvedPreferences.startProxyWhenAppLaunches
  }

  deinit {
    initializationTask?.cancel()
    eventTask?.cancel()
  }

  /// Idempotent helper/config initialization. Concurrent callers await the
  /// same task. Launch auto-start is evaluated once, after successful runtime
  /// initialization, and is always submitted as noninteractive.
  @discardableResult
  public func initialize() async -> Bool {
    if initializationState == .initialized {
      return true
    }
    if let initializationTask {
      return await initializationTask.value
    }

    let task = Task { @MainActor [weak self] () -> Bool in
      guard let self else {
        return false
      }
      return await self.performInitialization()
    }
    initializationTask = task
    let result = await task.value
    initializationTask = nil
    return result
  }

  @discardableResult
  public func refreshRuntimeState() async -> Bool {
    do {
      let state = try await runtimeClient.refreshState()
      apply(state)
      return true
    } catch {
      record(error, code: "state_refresh_failed", message: "Could not refresh Vekil state.")
      return false
    }
  }

  @discardableResult
  public func performPrimaryAction() async -> Bool {
    let action = primaryAction
    guard action.isEnabled else {
      return false
    }

    switch action.kind {
    case .startProxy:
      return await startProxy()
    case .cancelStarting:
      return await cancelStarting(operationID: action.operationID)
    case .stopProxy:
      return await stopProxy()
    case .none:
      return false
    }
  }

  @discardableResult
  public func startProxy() async -> Bool {
    await submitMutation(
      failureCode: "start_failed",
      failureMessage: "Could not start the proxy."
    ) {
      try await runtimeClient.start(.userInitiated)
    }
  }

  @discardableResult
  public func cancelStarting(operationID: String? = nil) async -> Bool {
    let target = operationID ?? runtimeState.operation?.id
    guard let target, !target.isEmpty, !isSubmittingCommand else {
      return false
    }

    isSubmittingCommand = true
    defer { isSubmittingCommand = false }
    do {
      try await runtimeClient.cancelOperation(id: target)
      cancellationRequestedOperationID = target
      return true
    } catch {
      record(error, code: "cancel_failed", message: "Could not cancel proxy startup.")
      return false
    }
  }

  @discardableResult
  public func stopProxy() async -> Bool {
    await submitMutation(
      failureCode: "stop_failed",
      failureMessage: "Could not stop the proxy."
    ) {
      try await runtimeClient.stop()
    }
  }

  @discardableResult
  public func restartProxy() async -> Bool {
    await submitMutation(
      failureCode: "restart_failed",
      failureMessage: "Could not restart the proxy."
    ) {
      try await runtimeClient.restart(.userInitiated)
    }
  }

  @discardableResult
  public func signInWithGitHub() async -> Bool {
    deviceCode = nil
    return await submitMutation(
      failureCode: "device_auth_failed",
      failureMessage: "Could not start GitHub sign in."
    ) {
      try await runtimeClient.startDeviceAuthentication()
    }
  }

  @discardableResult
  public func signInWithGitHubCLI() async -> Bool {
    await submitMutation(
      failureCode: "github_cli_auth_failed",
      failureMessage: "Could not sign in with the GitHub CLI account."
    ) {
      try await runtimeClient.authenticateWithGitHubCLI()
    }
  }

  @discardableResult
  public func signOut() async -> Bool {
    deviceCode = nil
    return await submitMutation(
      failureCode: "sign_out_failed",
      failureMessage: "Could not sign out."
    ) {
      try await runtimeClient.signOut()
    }
  }

  /// Opens a path picker but never accesses the chosen file. A missing or
  /// otherwise unreadable path is still forwarded to Runtime/Go for its safe,
  /// bounded validation and transactional running-intent handling.
  @discardableResult
  public func chooseExternalConfiguration() async -> Bool {
    do {
      guard let url = try await externalConfigurationPathSelector.selectExternalConfigurationPath()
      else {
        return false
      }
      return await selectExternalConfiguration(at: url)
    } catch {
      record(
        error,
        code: "external_config_selection_failed",
        message: "Could not choose an External Configuration."
      )
      return false
    }
  }

  @discardableResult
  public func selectExternalConfiguration(at url: URL) async -> Bool {
    guard url.isFileURL, !url.path.isEmpty else {
      lastError = AppRuntimeStructuredError(
        code: "invalid_external_config_path",
        userMessage: "Choose a local JSON or YAML configuration file."
      )
      return false
    }

    return await submitMutation(
      failureCode: "external_config_select_failed",
      failureMessage: "Could not use the selected External Configuration."
    ) {
      try await runtimeClient.selectExternalConfiguration(path: url.path)
    }
  }

  @discardableResult
  public func reloadExternalConfiguration() async -> Bool {
    await submitMutation(
      failureCode: "external_config_reload_failed",
      failureMessage: "Could not reload the External Configuration."
    ) {
      try await runtimeClient.reloadExternalConfiguration()
    }
  }

  @discardableResult
  public func clearExternalConfiguration() async -> Bool {
    await submitMutation(
      failureCode: "external_config_clear_failed",
      failureMessage: "Could not clear the External Configuration selection."
    ) {
      try await runtimeClient.clearExternalConfiguration()
    }
  }

  public func selectDestination(_ destination: VekilDestination) {
    selectedDestination = destination
    preferences.selectedDestination = destination
  }

  public func saveMainWindowFrame(_ frame: VekilWindowFrame?) {
    let usableFrame = frame?.isUsable == true ? frame : nil
    mainWindowFrame = usableFrame
    preferences.mainWindowFrame = usableFrame
  }

  /// Presents the setup assistant on demand without changing completion state.
  public func showOnboarding() {
    isShowingOnboarding = true
  }

  /// Completing or explicitly skipping onboarding is durable and never
  /// changes proxy running intent, navigation, or launch preferences.
  public func completeOnboarding() {
    preferences.completedOnboardingVersion = max(
      preferences.completedOnboardingVersion ?? 0,
      Self.currentOnboardingVersion
    )
    isShowingOnboarding = false
  }

  public func deferOnboarding() {
    completeOnboarding()
  }

  /// This preference only affects the one launch-time evaluation. Changing it
  /// after initialization never starts or stops the current runtime.
  public func setStartProxyWhenAppLaunches(_ enabled: Bool) {
    startProxyWhenAppLaunches = enabled
    preferences.startProxyWhenAppLaunches = enabled
  }

  @discardableResult
  public func setOpenAtLogin(_ enabled: Bool) async -> Bool {
    guard !isUpdatingOpenAtLogin else {
      return false
    }

    isUpdatingOpenAtLogin = true
    defer { isUpdatingOpenAtLogin = false }
    do {
      let status = try await loginItemService.setEnabled(enabled)
      applyLoginItemStatus(status, requestedValue: enabled)
      return status == .enabled || status == .disabled || status == .requiresApproval
    } catch {
      record(error, code: "login_item_failed", message: "Could not update Open at Login.")
      return false
    }
  }

  @discardableResult
  public func openLoginItemSettings() async -> Bool {
    do {
      try await loginItemService.openSystemSettings()
      return true
    } catch {
      record(
        error, code: "login_item_settings_failed", message: "Could not open Login Items settings.")
      return false
    }
  }

  public func applicationDidBecomeActive() async {
    guard initializationState == .initialized else {
      return
    }
    await refreshLoginItemStatus()
  }

  @discardableResult
  public func checkForUpdates() async -> Bool {
    guard updaterService.isAvailable else {
      lastError = AppRuntimeStructuredError(
        code: "updater_unavailable",
        userMessage: "Update checking is unavailable in this build."
      )
      return false
    }
    do {
      try await updaterService.checkForUpdates()
      return true
    } catch {
      record(error, code: "update_check_failed", message: "Could not check for updates.")
      return false
    }
  }

  @discardableResult
  public func copyBaseURL() async -> Bool {
    guard let baseURL else {
      return false
    }
    do {
      try await clipboardService.copy(baseURL.absoluteString)
      return true
    } catch {
      record(error, code: "clipboard_failed", message: "Could not copy the base URL.")
      return false
    }
  }

  @discardableResult
  public func copyText(_ text: String) async -> Bool {
    guard !text.isEmpty, text.utf8.count <= 32 * 1024 else {
      lastError = AppRuntimeStructuredError(
        code: "clipboard_value_invalid",
        userMessage: "The setup value could not be copied."
      )
      return false
    }
    do {
      try await clipboardService.copy(text)
      return true
    } catch {
      record(error, code: "clipboard_failed", message: "Could not copy the setup value.")
      return false
    }
  }

  @discardableResult
  public func openDashboard() async -> Bool {
    guard runtimeState.service == .running, let dashboardURL else {
      lastError = AppRuntimeStructuredError(
        code: "proxy_not_running",
        userMessage: "Start the proxy before opening the dashboard."
      )
      return false
    }
    do {
      try await browserService.open(dashboardURL)
      return true
    } catch {
      record(error, code: "open_dashboard_failed", message: "Could not open the dashboard.")
      return false
    }
  }

  @discardableResult
  public func copyDeviceCode() async -> Bool {
    guard let deviceCode else {
      return false
    }
    do {
      try await clipboardService.copy(deviceCode.userCode)
      return true
    } catch {
      record(error, code: "clipboard_failed", message: "Could not copy the device code.")
      return false
    }
  }

  @discardableResult
  public func openDeviceVerificationPage() async -> Bool {
    guard let deviceCode else {
      return false
    }
    do {
      try await browserService.open(deviceCode.verificationURL)
      return true
    } catch {
      record(
        error, code: "open_auth_page_failed", message: "Could not open the GitHub sign-in page.")
      return false
    }
  }

  public func dismissError() {
    lastError = nil
  }

  private func performInitialization() async -> Bool {
    initializationState = .initializing
    await startEventObservationIfNeeded()

    do {
      var initialization = try await runtimeClient.initialize()
      initialization.state.configuration = initialization.configuration
      helperBuild = initialization.helperBuild
      bundleBuildID = initialization.bundleBuildID
      apply(initialization.state)

      await refreshLoginItemStatus()

      initializationState = .initialized
      resolveInitialOnboardingPresentation()
      await evaluateAutoStartExactlyOnce()
      return true
    } catch {
      initializationState = .failed
      record(
        error, code: "runtime_initialization_failed",
        message: "Could not initialize the Vekil runtime.")
      if needsOnboardingForStoredVersion {
        isShowingOnboarding = true
      }
      return false
    }
  }

  /// A missing preference is only a signal to inspect recovered runtime and
  /// native state. It is not, on its own, proof that this is a fresh install.
  private func resolveInitialOnboardingPresentation() {
    if let completedVersion = preferences.completedOnboardingVersion {
      isShowingOnboarding = completedVersion < Self.currentOnboardingVersion
      return
    }

    let recoveredExistingSetup =
      runtimeState.authentication.state == .signedIn
      || runtimeState.configuration.mode == .external
      || runtimeState.configuration.mode == .managed
      || runtimeState.configuration.selectedExternalPath != nil
      || runtimeState.service == .running
      || openAtLogin
      || hadPersistedNativeSetupEvidence

    if recoveredExistingSetup {
      preferences.completedOnboardingVersion = Self.currentOnboardingVersion
      isShowingOnboarding = false
    } else {
      isShowingOnboarding = true
    }
  }

  private var needsOnboardingForStoredVersion: Bool {
    guard let completedVersion = preferences.completedOnboardingVersion else {
      return true
    }
    return completedVersion < Self.currentOnboardingVersion
  }

  private func startEventObservationIfNeeded() async {
    guard eventTask == nil else {
      return
    }

    // Acquire the stream before initialization so a fast helper handshake or
    // initial state frame cannot race subscription setup.
    let stream = await runtimeClient.events()
    eventTask = Task { @MainActor [weak self] in
      for await event in stream {
        guard !Task.isCancelled, let self else {
          break
        }
        await self.receive(event)
      }
    }
  }

  private func receive(_ event: AppRuntimeClientEvent) async {
    switch event {
    case .connection(let connection):
      apply(connection)
    case .state(let state):
      apply(state)
    case .operation(let event):
      apply(event)
    case .deviceCode(let code):
      await apply(code)
    }
  }

  private func apply(_ connection: AppRuntimeConnectionEvent) {
    guard connection.launchToken == runtimeState.launchToken,
      connection.helperEpoch == runtimeState.helperEpoch
    else { return }

    var state = runtimeState
    state.helper = connection.helper
    switch connection.helper {
    case .launching:
      state.service = .stopped
      state.readiness = .unknown
    case .restarting, .failed:
      state.service = .failed
      state.readiness = .unknown
      state.operation = nil
      cancellationRequestedOperationID = nil
      deviceCode = nil
    case .stopped:
      state.service = .stopped
      state.readiness = .unknown
      state.operation = nil
    case .connected:
      break
    default:
      break
    }
    runtimeState = state
    if let error = connection.error { lastError = error }
  }

  @discardableResult
  private func apply(_ incoming: AppRuntimeStateSnapshot) -> Bool {
    let currentToken = runtimeState.launchToken
    let hasCurrentIdentity = currentToken != RuntimeLaunchIdentity.zero.launchToken
    if !hasCurrentIdentity {
      // Initial authoritative state.
    } else if incoming.launchToken == currentToken {
      guard incoming.helperEpoch == runtimeState.helperEpoch,
            incoming.stateRevision > runtimeState.stateRevision else {
        return false
      }
    } else {
      guard !retiredLaunchTokens.contains(incoming.launchToken) else {
        return false
      }
      retiredLaunchTokens.insert(currentToken)
    }

    if let cancellationRequestedOperationID,
      incoming.operation?.id != cancellationRequestedOperationID
    {
      self.cancellationRequestedOperationID = nil
    }
    if let deviceCode,
      incoming.operation?.id != deviceCode.operationID,
      incoming.authentication.state != .signingIn
    {
      self.deviceCode = nil
    }

    runtimeState = incoming
    if let error = incoming.lastError {
      lastError = error
    }
    return true
  }

  private func apply(_ event: AppRuntimeOperationEvent) {
    guard
      event.launchToken == runtimeState.launchToken,
      event.helperEpoch == runtimeState.helperEpoch,
      event.stateRevision > runtimeState.stateRevision
    else {
      return
    }

    var state = runtimeState
    state.stateRevision = event.stateRevision
    let isCurrentOperation = state.operation?.id == event.operationID

    if event.status.isTerminal {
      if isCurrentOperation {
        state.operation = nil
      }
      if cancellationRequestedOperationID == event.operationID {
        cancellationRequestedOperationID = nil
      }
      if deviceCode?.operationID == event.operationID {
        deviceCode = nil
      }
    } else if let operation = event.operation,
      state.operation == nil || isCurrentOperation
    {
      state.operation = operation
    }

    runtimeState = state
    if event.status == .failed, isCurrentOperation || state.operation == nil,
      let error = event.error
    {
      lastError = error
    }
  }

  private func apply(_ code: AppRuntimeDeviceCode) async {
    guard code.helperEpoch == runtimeState.helperEpoch else {
      return
    }
    if let operation = runtimeState.operation, operation.id != code.operationID {
      return
    }

    deviceCode = code
    do {
      // Preserve the legacy shell's copy-on-device-code behavior without
      // invoking pbcopy or placing secrets in a process environment.
      try await clipboardService.copy(code.userCode)
    } catch {
      record(error, code: "clipboard_failed", message: "Could not copy the device code.")
    }
  }

  private func evaluateAutoStartExactlyOnce() async {
    guard !hasEvaluatedAutoStart else {
      return
    }
    hasEvaluatedAutoStart = true

    guard startProxyWhenAppLaunches else {
      return
    }
    guard runtimeState.helper == .connected, runtimeState.service == .stopped else {
      return
    }

    _ = await submitMutation(
      failureCode: "automatic_start_failed",
      failureMessage: "Vekil could not start the proxy automatically."
    ) {
      try await runtimeClient.start(.automaticLaunch)
    }
  }

  @discardableResult
  private func submitMutation(
    failureCode: String,
    failureMessage: String,
    operation: () async throws -> AppRuntimeOperationAcceptance
  ) async -> Bool {
    guard !isSubmittingCommand else {
      return false
    }

    isSubmittingCommand = true
    lastError = nil
    let submittedStateRevision = runtimeState.stateRevision
    defer { isSubmittingCommand = false }

    do {
      let acceptance = try await operation()
      guard acceptance.accepted else {
        lastError = AppRuntimeStructuredError(code: "operation_rejected", userMessage: failureMessage)
        return false
      }
      if let acceptedOperation = acceptance.operation,
        runtimeState.stateRevision == submittedStateRevision, runtimeState.operation == nil
      {
        var state = runtimeState
        state.operation = acceptedOperation
        runtimeState = state
      }
      return true
    } catch {
      record(error, code: failureCode, message: failureMessage)
      return false
    }
  }

  private func applyLoginItemStatus(_ status: LoginItemStatus, requestedValue: Bool) {
    loginItemStatus = status
    switch status {
    case .enabled, .requiresApproval:
      openAtLogin = true
      preferences.openAtLogin = true
    case .disabled, .notFound:
      openAtLogin = false
      preferences.openAtLogin = false
    case .unavailable:
      openAtLogin = requestedValue
      preferences.openAtLogin = requestedValue
    }
  }

  private func refreshLoginItemStatus() async {
    let status = await loginItemService.currentStatus()
    applyLoginItemStatus(status, requestedValue: preferences.openAtLogin)
  }

  private func record(_ error: Error, code: String, message: String) {
    if let structured = error as? AppRuntimeStructuredError {
      lastError = structured
    } else {
      // Avoid persisting or displaying arbitrary error descriptions, which
      // can contain external-file bytes, upstream bodies, headers, paths,
      // or managed secret values.
      lastError = AppRuntimeStructuredError(code: code, userMessage: message)
    }
  }
}
