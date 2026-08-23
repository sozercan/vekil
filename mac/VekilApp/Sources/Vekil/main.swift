import AppKit
import Foundation
import VekilCore
import VekilUI
import VekilUpdater

// Real AppKit entry point. Singleton acquisition occurs before helper launch,
// preferences migration, updater creation, or Keychain access.
MainActor.assumeIsolated {
    let testRoot = ProcessInfo.processInfo.environment["VEKIL_TEST_ROOT"].map {
        URL(fileURLWithPath: $0, isDirectory: true)
    }
    let gate: ApplicationInstanceGate
    do {
        let singletonBase = testRoot?.appendingPathComponent("ApplicationSupport", isDirectory: true)
        guard let acquired = try ApplicationInstanceGate.acquire(baseDirectory: singletonBase) else { return }
        gate = acquired
    } catch {
        fputs("Vekil could not acquire its private singleton lock.\n", stderr)
        return
    }

    let bundle = Bundle.main
    let appVersion = bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "Development"
    let buildID = bundle.object(forInfoDictionaryKey: "VekilBundleBuildID") as? String ?? "development"
    let helperURL = VekilBundleLayout.helperURL(bundleURL: bundle.bundleURL.resolvingSymlinksInPath())
    var helperArguments = ["--parent-pid", String(ProcessInfo.processInfo.processIdentifier)]
    if let testRoot {
        helperArguments += [
            "--state-dir", testRoot.appendingPathComponent("state", isDirectory: true).path,
            "--token-dir", testRoot.appendingPathComponent("tokens", isDirectory: true).path,
        ]
    }
    let configuration = RuntimeControllerConfiguration(
        process: RuntimeProcessConfiguration(executableURL: helperURL, arguments: helperArguments),
        expectedBundleBuildID: buildID
    )
    let keychainStore: any KeychainSecretStore = testRoot == nil
        ? SecurityKeychainSecretStore() : InMemoryKeychainSecretStore()
    let secretGenerationManager = KeychainSecretGenerationManager(store: keychainStore)
    let secretProjectionPreparer = RuntimeSecretProjectionPreparer(
        manager: secretGenerationManager
    )
    let secretGenerationCleaner = RuntimeSecretGenerationCleaner(
        manager: secretGenerationManager
    )
    let controller = RuntimeController(
        configuration: configuration,
        processFactory: ValidatingProcessFactory(
            bundleURL: bundle.bundleURL,
            validator: RuntimeHelperValidator()
        ),
        launchPreparation: { context in
            let requests = try await secretProjectionPreparer.requests(for: context)
            do {
                try await secretGenerationCleaner.reconcile(context.state)
            } catch {
                fputs("Vekil could not remove obsolete Keychain credentials.\n", stderr)
            }
            return requests
        }
    )
    let secretCleanupTask = Task {
        let notifications = await controller.notificationStream()
        for await notification in notifications {
            guard case let .state(event) = notification else { continue }
            do {
                try await secretGenerationCleaner.reconcile(event.payload)
            } catch {
                fputs("Vekil could not remove obsolete Keychain credentials.\n", stderr)
            }
        }
    }
    let runtimeClient = RuntimeAppClient(controller: controller)
    let updater = SparkleUpdateDriver()
    let preferences: any VekilPreferencesStore = testRoot == nil
        ? UserDefaultsVekilPreferencesStore() : InMemoryVekilPreferencesStore()
    let loginItemService: any LoginItemService = testRoot == nil
        ? SystemLoginItemService() : UnavailableLoginItemService()
    let appState = VekilAppState(
        runtimeClient: runtimeClient,
        preferences: preferences,
        loginItemService: loginItemService,
        updaterService: updater,
        browserService: SystemBrowserService(),
        clipboardService: SystemClipboardService(),
        externalConfigurationPathSelector: OpenPanelExternalConfigurationSelector(),
        applicationVersion: appVersion
    )
    let analyticsStore = StatsStore(
        dataSource: URLSessionAnalyticsDataSource(baseURL: URL(string: "http://127.0.0.1:1337")!)
    )
    let analytics = AnalyticsViewModel(store: analyticsStore)
    let coordinator = VekilApplicationCoordinator(
        appState: appState,
        analytics: analytics,
        gate: gate,
        shutdownRuntime: { await controller.shutdown(reason: .quit) }
    )
    let application = NSApplication.shared
    application.delegate = coordinator
    application.run()
    secretCleanupTask.cancel()
    withExtendedLifetime(
        (
            gate, coordinator, updater, runtimeClient, controller, analytics,
            keychainStore, secretGenerationManager, secretProjectionPreparer,
            secretGenerationCleaner, secretCleanupTask
        )
    ) {}
}
