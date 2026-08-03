import Foundation
import XCTest

@testable import VekilCore

@MainActor
final class VekilAppStateTests: XCTestCase {
  func testAutoStartIsEvaluatedExactlyOnceAndIsNoninteractive() async {
    let runtime = RuntimeClientSpy()
    await runtime.setInitializationDelay(nanoseconds: 10_000_000)
    let preferences = InMemoryVekilPreferencesStore(startProxyWhenAppLaunches: true)
    let state = makeState(runtime: runtime, preferences: preferences)

    async let first = state.initialize()
    async let second = state.initialize()
    let results = await (first, second)

    XCTAssertTrue(results.0)
    XCTAssertTrue(results.1)
    await assertTrueAsync(await state.initialize())

    let calls = await runtime.recordedCalls()
    XCTAssertEqual(calls.filter { $0 == .initialize }.count, 1)
    let starts = calls.compactMap { call -> AppRuntimeStartRequest? in
      guard case .start(let request) = call else { return nil }
      return request
    }
    XCTAssertEqual(starts, [.automaticLaunch])
    XCTAssertFalse(starts[0].allowsInteractiveAuthentication)
  }

  func testAutoStartDefaultsOffAndDoesNotRunWhenRuntimeIsAlreadyRunning() async {
    let stoppedRuntime = RuntimeClientSpy()
    let stoppedState = makeState(runtime: stoppedRuntime)
    await assertTrueAsync(await stoppedState.initialize())
    await assertFalseAsync(
      (await stoppedRuntime.recordedCalls()).contains {
        if case .start = $0 { true } else { false }
      })

    var running = AppRuntimeStateSnapshot.connectedStopped
    running.service = .running
    running.readiness = .ready
    let runningRuntime = RuntimeClientSpy(state: running)
    let preferences = InMemoryVekilPreferencesStore(startProxyWhenAppLaunches: true)
    let runningState = makeState(runtime: runningRuntime, preferences: preferences)
    await assertTrueAsync(await runningState.initialize())
    await assertFalseAsync(
      (await runningRuntime.recordedCalls()).contains {
        if case .start = $0 { true } else { false }
      })
  }

  func testFailedInitializationCanRetryButAutoStartStillRunsOnlyOnceAfterSuccess() async {
    let runtime = RuntimeClientSpy()
    await runtime.setThrownError(LeakyTestError(secret: "not-for-state"))
    let preferences = InMemoryVekilPreferencesStore(startProxyWhenAppLaunches: true)
    let state = makeState(runtime: runtime, preferences: preferences)

    await assertFalseAsync(await state.initialize())
    XCTAssertEqual(state.initializationState, .failed)
    await runtime.setThrownError(nil)
    await assertTrueAsync(await state.initialize())
    await assertTrueAsync(await state.initialize())

    let calls = await runtime.recordedCalls()
    XCTAssertEqual(calls.filter { if case .start = $0 { true } else { false } }.count, 1)
    XCTAssertEqual(calls.filter { $0 == .initialize }.count, 2)
  }

  func testManualLifecycleAuthAndConfigActionsAreForwardedAsAsyncRuntimeOperations() async {
    let runtime = RuntimeClientSpy()
    let missingURL = URL(fileURLWithPath: "/tmp/vekil-does-not-exist-\(UUID().uuidString).yaml")
    let selector = ExternalConfigurationPathSelectorSpy(selectedURL: missingURL)
    let state = makeState(runtime: runtime, pathSelector: selector)
    await assertTrueAsync(await state.initialize())

    await assertTrueAsync(await state.startProxy())
    XCTAssertEqual(state.runtimeState.operation?.kind, .start)
    let startOperationID = state.runtimeState.operation?.id
    await assertTrueAsync(await state.cancelStarting())
    XCTAssertEqual(state.cancellationRequestedOperationID, startOperationID)
    await assertTrueAsync(await state.stopProxy())
    await assertTrueAsync(await state.restartProxy())
    await assertTrueAsync(await state.signInWithGitHub())
    await assertTrueAsync(await state.signInWithGitHubCLI())
    await assertTrueAsync(await state.signOut())
    await assertTrueAsync(await state.chooseExternalConfiguration())
    await assertTrueAsync(await state.reloadExternalConfiguration())
    await assertTrueAsync(await state.clearExternalConfiguration())

    let calls = await runtime.recordedCalls()
    XCTAssertTrue(calls.contains(.start(.userInitiated)))
    XCTAssertTrue(calls.contains(.cancel(startOperationID!)))
    XCTAssertTrue(calls.contains(.stop))
    XCTAssertTrue(calls.contains(.restart(.userInitiated)))
    XCTAssertTrue(calls.contains(.deviceAuth))
    XCTAssertTrue(calls.contains(.githubCLIAuth))
    XCTAssertTrue(calls.contains(.signOut))
    XCTAssertTrue(calls.contains(.selectExternal(missingURL.path)))
    XCTAssertTrue(calls.contains(.reloadExternal))
    XCTAssertTrue(calls.contains(.clearExternal))
  }

  func testFastTerminalStateIsNotOverwrittenByLateAcceptance() async {
    let runtime = RuntimeClientSpy()
    let state = makeState(runtime: runtime)
    await assertTrueAsync(await state.initialize())
    var terminal = AppRuntimeStateSnapshot.connectedStopped
    terminal.stateRevision = 3
    terminal.runtimeGeneration = 2
    terminal.service = .running
    terminal.readiness = .ready
    terminal.operation = nil
    await runtime.setEventBeforeAcceptance(.state(terminal))

    await assertTrueAsync(await state.startProxy())
    await assertTrueAsync(await eventually { state.runtimeState.stateRevision == 3 })
    XCTAssertNil(state.runtimeState.operation)
    XCTAssertEqual(state.runtimeState.service, .running)
  }

  func testExternalConfigurationSelectionPassesOnlyAPathAndWaitsForRuntimeState() async {
    let runtime = RuntimeClientSpy()
    let path = "/tmp/nonexistent-external-\(UUID().uuidString).yaml"
    XCTAssertFalse(FileManager.default.fileExists(atPath: path))
    let state = makeState(
      runtime: runtime,
      pathSelector: ExternalConfigurationPathSelectorSpy(selectedURL: URL(fileURLWithPath: path))
    )
    await assertTrueAsync(await state.initialize())

    await assertTrueAsync(await state.chooseExternalConfiguration())
    XCTAssertNil(
      state.runtimeState.configuration.selectedExternalPath, "Path is not applied optimistically")
    await assertTrueAsync((await runtime.recordedCalls()).contains(.selectExternal(path)))

    var selected = AppRuntimeStateSnapshot.connectedStopped
    selected.stateRevision = 2
    selected.configuration = AppRuntimeConfigurationState(
      mode: .external,
      displayName: "External",
      selectedExternalPath: path,
      selectedRevision: "disk-revision",
      activeRevision: "active-revision",
      drift: .changed,
      requiresGitHubAuthentication: false
    )
    await runtime.emit(.state(selected))
    await assertTrueAsync(await eventually { state.runtimeState.stateRevision == 2 })
    XCTAssertEqual(state.runtimeState.configuration.selectedExternalPath, path)
  }

  func testCanceledExternalPathSelectionDoesNotMutateRuntime() async {
    let runtime = RuntimeClientSpy()
    let state = makeState(runtime: runtime, pathSelector: ExternalConfigurationPathSelectorSpy())
    await assertTrueAsync(await state.initialize())

    await assertFalseAsync(await state.chooseExternalConfiguration())
    await assertFalseAsync(
      (await runtime.recordedCalls()).contains {
        if case .selectExternal = $0 { true } else { false }
      })
  }

  func testHelperFailureConnectionEventClearsReadyAndActiveState() async {
    var running = AppRuntimeStateSnapshot.connectedStopped
    running.service = .running
    running.readiness = .ready
    running.operation = AppRuntimeOperation(id: "op_running", kind: .start)
    let runtime = RuntimeClientSpy(state: running)
    let state = makeState(runtime: runtime)
    await assertTrueAsync(await state.initialize())

    await runtime.emit(.connection(AppRuntimeConnectionEvent(
      launchToken: running.launchToken, helperEpoch: running.helperEpoch, helper: .failed,
      error: AppRuntimeStructuredError(
        code: "helper_failed", userMessage: "The runtime helper stopped.",
        recoveryAction: "restart_helper"
      )
    )))
    await assertTrueAsync(await eventually { state.runtimeState.helper == .failed })
    XCTAssertEqual(state.runtimeState.service, .failed)
    XCTAssertEqual(state.runtimeState.readiness, .unknown)
    XCTAssertNil(state.runtimeState.operation)
    XCTAssertEqual(state.lastError?.code, "helper_failed")
  }

  func testConnectionEventsDoNotConsumeTheNextAuthoritativeStateRevision() async {
    let runtime = RuntimeClientSpy()
    var initial = AppRuntimeStateSnapshot.connectedStopped
    initial.stateRevision = 5
    initial.helper = .launching
    await setInitialization(runtime, state: initial)
    let state = makeState(runtime: runtime)
    await assertTrueAsync(await state.initialize())

    await runtime.emit(
      .connection(
        AppRuntimeConnectionEvent(
          launchToken: initial.launchToken,
          helperEpoch: initial.helperEpoch,
          helper: .connected
        )))
    await assertTrueAsync(await eventually { state.runtimeState.helper == .connected })
    XCTAssertEqual(
      state.runtimeState.stateRevision, 5,
      "Connection-only UI state must not consume an authoritative helper revision")

    var next = initial
    next.stateRevision = 6
    next.helper = .connected
    next.service = .running
    next.readiness = .ready
    await runtime.emit(.state(next))

    await assertTrueAsync(await eventually { state.runtimeState.stateRevision == 6 })
    XCTAssertEqual(state.runtimeState.service, .running)
    XCTAssertEqual(state.runtimeState.readiness, .ready)
  }

  func testStateEventsRejectOlderRevisionsAndRetiredHelperEpochs() async {
    let runtime = RuntimeClientSpy()
    var initial = AppRuntimeStateSnapshot.connectedStopped
    initial.stateRevision = 5
    let state = makeState(runtime: runtime)
    await runtime.setThrownError(nil)
    // Replace initialization before initializing the state.
    await setInitialization(runtime, state: initial)
    await assertTrueAsync(await state.initialize())

    var stale = initial
    stale.stateRevision = 4
    stale.service = .running
    await runtime.emit(.state(stale))
    await Task.yield()
    XCTAssertEqual(state.runtimeState.service, .stopped)

    var current = initial
    current.stateRevision = 6
    current.service = .running
    current.readiness = .ready
    await runtime.emit(.state(current))
    await assertTrueAsync(await eventually { state.runtimeState.stateRevision == 6 })
    XCTAssertEqual(state.presentation.kind, .ready)

    var replacement = AppRuntimeStateSnapshot.connectedStopped
    replacement.launchToken = UUID(uuidString: "22222222-2222-2222-2222-222222222222")!
    replacement.helperEpoch = "hep_1"
    replacement.stateRevision = 1
    await runtime.emit(.state(replacement))
    await assertTrueAsync(await eventually { state.runtimeState.launchToken == replacement.launchToken })

    var retired = current
    retired.stateRevision = 99
    retired.service = .failed
    await runtime.emit(.state(retired))
    await Task.yield()
    XCTAssertEqual(state.runtimeState.launchToken, replacement.launchToken)
    XCTAssertEqual(state.runtimeState.service, .stopped)
  }

  func testDeviceCodeIsCopiedAndBrowserOpeningUsesInjectedServices() async {
    let runtime = RuntimeClientSpy()
    let clipboard = ClipboardServiceSpy()
    let browser = BrowserServiceSpy()
    let state = makeState(runtime: runtime, browser: browser, clipboard: clipboard)
    await assertTrueAsync(await state.initialize())
    await assertTrueAsync(await state.signInWithGitHub())
    let operationID = state.runtimeState.operation!.id
    let code = AppRuntimeDeviceCode(
      launchToken: UUID(uuidString: "11111111-1111-1111-1111-111111111111")!,
      helperEpoch: "hep_1",
      operationID: operationID,
      verificationURL: URL(string: "https://github.com/login/device")!,
      userCode: "ABCD-EFGH",
      expiresAt: Date().addingTimeInterval(900)
    )

    await runtime.emit(.deviceCode(code))
    await assertTrueAsync(
      await eventually { state.deviceCode == code && clipboard.copiedStrings.contains("ABCD-EFGH") }
    )
    await assertTrueAsync(await state.openDeviceVerificationPage())
    XCTAssertEqual(browser.openedURLs, [code.verificationURL])
  }

  func testOpenAtLoginAndStartOnLaunchRemainIndependent() async {
    let runtime = RuntimeClientSpy()
    let preferences = InMemoryVekilPreferencesStore(
      openAtLogin: false,
      startProxyWhenAppLaunches: false
    )
    let login = LoginItemServiceSpy(status: .enabled)
    let state = makeState(runtime: runtime, preferences: preferences, login: login)
    await assertTrueAsync(await state.initialize())

    XCTAssertTrue(state.openAtLogin)
    XCTAssertTrue(preferences.openAtLogin)
    XCTAssertFalse(state.startProxyWhenAppLaunches)

    state.setStartProxyWhenAppLaunches(true)
    XCTAssertTrue(state.startProxyWhenAppLaunches)
    XCTAssertTrue(state.openAtLogin)
    await assertFalseAsync(
      (await runtime.recordedCalls()).contains { if case .start = $0 { true } else { false } },
      "Changing the preference after initialization must not auto-start")

    await assertTrueAsync(await state.setOpenAtLogin(false))
    XCTAssertFalse(state.openAtLogin)
    XCTAssertTrue(state.startProxyWhenAppLaunches)
    XCTAssertEqual(login.requestedValues, [false])
  }

  func testNavigationWindowUpdaterBrowserAndClipboardUseInjectedServices() async {
    var running = AppRuntimeStateSnapshot.connectedStopped
    running.service = .running
    running.readiness = .ready
    let runtime = RuntimeClientSpy(state: running)
    let preferences = InMemoryVekilPreferencesStore()
    let updater = UpdaterServiceSpy()
    let browser = BrowserServiceSpy()
    let clipboard = ClipboardServiceSpy()
    let state = makeState(
      runtime: runtime,
      preferences: preferences,
      updater: updater,
      browser: browser,
      clipboard: clipboard
    )
    await assertTrueAsync(await state.initialize())

    state.selectDestination(.requests)
    let frame = VekilWindowFrame(x: 10, y: 10, width: 800, height: 600)
    state.saveMainWindowFrame(frame)
    XCTAssertEqual(preferences.selectedDestination, .requests)
    XCTAssertEqual(preferences.mainWindowFrame, frame)

    await assertTrueAsync(await state.copyBaseURL())
    await assertTrueAsync(await state.openDashboard())
    await assertTrueAsync(await state.checkForUpdates())
    XCTAssertEqual(clipboard.copiedStrings, ["http://127.0.0.1:1337"])
    XCTAssertEqual(browser.openedURLs, [URL(string: "http://127.0.0.1:1337/dashboard")!])
    XCTAssertEqual(updater.checkCount, 1)
  }

  func testArbitraryThrownErrorsAreSanitizedBeforeEnteringPublishedState() async {
    let runtime = RuntimeClientSpy()
    let state = makeState(runtime: runtime)
    await assertTrueAsync(await state.initialize())
    await runtime.setThrownError(LeakyTestError(secret: "managed-secret-sentinel"))

    await assertFalseAsync(await state.startProxy())
    XCTAssertEqual(state.lastError?.code, "start_failed")
    XCTAssertEqual(state.lastError?.userMessage, "Could not start the proxy.")
    XCTAssertFalse(state.lastError?.userMessage.contains("managed-secret-sentinel") ?? true)
  }

  func testStructuredOperationFailureClearsMatchingOperationAndPreservesSafeError() async {
    let runtime = RuntimeClientSpy()
    let state = makeState(runtime: runtime)
    await assertTrueAsync(await state.initialize())
    await assertTrueAsync(await state.startProxy())
    let operationID = state.runtimeState.operation!.id
    let failure = AppRuntimeStructuredError(
      code: "provider_validation_failed",
      userMessage: "Provider model validation failed.",
      retryable: true,
      recoveryAction: "retry_start"
    )

    await runtime.emit(
      .operation(
        AppRuntimeOperationEvent(
          launchToken: UUID(uuidString: "11111111-1111-1111-1111-111111111111")!,
          helperEpoch: "hep_1",
          stateRevision: 2,
          operationID: operationID,
          status: .failed,
          error: failure
        )))

    await assertTrueAsync(await eventually { state.runtimeState.stateRevision == 2 })
    XCTAssertNil(state.runtimeState.operation)
    XCTAssertEqual(state.lastError, failure)
  }

  private func makeState(
    runtime: RuntimeClientSpy,
    preferences: InMemoryVekilPreferencesStore? = nil,
    login: LoginItemServiceSpy? = nil,
    updater: UpdaterServiceSpy? = nil,
    browser: BrowserServiceSpy? = nil,
    clipboard: ClipboardServiceSpy? = nil,
    pathSelector: ExternalConfigurationPathSelectorSpy? = nil
  ) -> VekilAppState {
    VekilAppState(
      runtimeClient: runtime,
      preferences: preferences ?? InMemoryVekilPreferencesStore(),
      loginItemService: login ?? LoginItemServiceSpy(),
      updaterService: updater ?? UpdaterServiceSpy(),
      browserService: browser ?? BrowserServiceSpy(),
      clipboardService: clipboard ?? ClipboardServiceSpy(),
      externalConfigurationPathSelector: pathSelector ?? ExternalConfigurationPathSelectorSpy()
    )
  }

  private func setInitialization(_ runtime: RuntimeClientSpy, state: AppRuntimeStateSnapshot) async {
    await runtime.setInitialization(state)
  }
}
