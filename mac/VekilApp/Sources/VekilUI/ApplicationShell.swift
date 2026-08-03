import AppKit
import Combine
import Darwin
import Foundation
import SwiftUI
import VekilCore

public enum ApplicationInstanceGateError: Error { case unsafeDirectory, lockOpen(Int32), unsafeLock }

public final class ApplicationInstanceGate: @unchecked Sendable {
    public static let activationNotification = Notification.Name("com.vekil.menubar.activate-main-window")
    private let lockFD: Int32, directoryFD: Int32, uid: uid_t, identifier: String
    private let activationName: String
    private var observer: NSObjectProtocol?
    private var source: DispatchSourceFileSystemObject?

    private init(lockFD: Int32, directoryFD: Int32, uid: uid_t, identifier: String, activationName: String) {
        self.lockFD = lockFD; self.directoryFD = directoryFD; self.uid = uid; self.identifier = identifier; self.activationName = activationName
    }

    public static func acquire(identifier: String = "com.vekil.menubar", baseDirectory: URL? = nil) throws -> ApplicationInstanceGate? {
        let uid = getuid()
        let base = (baseDirectory ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]).resolvingSymlinksInPath()
        let appDir = base.appendingPathComponent("vekil")
        let directory = appDir.appendingPathComponent("Singleton-\(uid)")
        if !FileManager.default.fileExists(atPath: appDir.path) {
            try FileManager.default.createDirectory(
                at: appDir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700]
            )
        }
        if !FileManager.default.fileExists(atPath: directory.path) {
            try FileManager.default.createDirectory(
                at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700]
            )
        }
        for url in [appDir, directory] {
            var st = stat()
            guard lstat(url.path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR, st.st_uid == uid else {
                throw ApplicationInstanceGateError.unsafeDirectory
            }
            chmod(url.path, 0o700)
        }
        let dirFD = Darwin.open(directory.path, O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW)
        guard dirFD >= 0 else { throw ApplicationInstanceGateError.unsafeDirectory }
        let lockName = "\(identifier)-\(uid).lock"
        let activationName = "activate-\(uid).request"
        let fd = openat(dirFD, lockName, O_CREAT | O_RDWR | O_CLOEXEC | O_NOFOLLOW, 0o600)
        guard fd >= 0 else { Darwin.close(dirFD); throw ApplicationInstanceGateError.lockOpen(errno) }
        var st = stat(); guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_uid == uid, st.st_nlink == 1 else { Darwin.close(fd); Darwin.close(dirFD); throw ApplicationInstanceGateError.unsafeLock }
        fchmod(fd, 0o600)
        guard flock(fd, LOCK_EX | LOCK_NB) == 0 else {
            let request = openat(dirFD, activationName, O_CREAT | O_WRONLY | O_TRUNC | O_CLOEXEC | O_NOFOLLOW, 0o600)
            if request >= 0 {
                _ = fsync(request); Darwin.close(request); _ = fsync(dirFD)
            }
            DistributedNotificationCenter.default().postNotificationName(activationNotification, object: identifier, userInfo: ["uid": uid], deliverImmediately: true)
            Darwin.close(fd); Darwin.close(dirFD); return nil
        }
        return ApplicationInstanceGate(lockFD: fd, directoryFD: dirFD, uid: uid, identifier: identifier, activationName: activationName)
    }

    @MainActor public func observe(_ handler: @escaping @MainActor () -> Void) {
        observer = DistributedNotificationCenter.default().addObserver(forName: Self.activationNotification, object: identifier, queue: .main) { [weak self] _ in Task { @MainActor in self?.consume(handler) } }
        let watchFD = dup(directoryFD)
        if watchFD >= 0 {
            let source = DispatchSource.makeFileSystemObjectSource(fileDescriptor: watchFD, eventMask: .write, queue: .main)
            source.setEventHandler { [weak self] in self?.consume(handler) }
            source.setCancelHandler { Darwin.close(watchFD) }; source.resume(); self.source = source
        }
        consume(handler)
    }

    @MainActor private func consume(_ handler: @escaping @MainActor () -> Void) {
        var st = stat(); guard fstatat(directoryFD, activationName, &st, AT_SYMLINK_NOFOLLOW) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_uid == uid, unlinkat(directoryFD, activationName, 0) == 0 else { return }; handler()
    }

    deinit {
        if let observer {
            DistributedNotificationCenter.default().removeObserver(observer)
        }; source?.cancel(); flock(lockFD, LOCK_UN); Darwin.close(lockFD); Darwin.close(directoryFD)
    }
}

enum CompactStatusMenuAction: Equatable {
    case primary
    case openVekil
    case settings
    case copyBaseURL
    case quit
}

struct CompactStatusMenuItemDescriptor: Equatable {
    let title: String?
    let tag: Int?
    let action: CompactStatusMenuAction?
    let isEnabled: Bool
    let isHidden: Bool

    var isSeparator: Bool {
        title == nil
    }

    static let separator = Self(
        title: nil,
        tag: nil,
        action: nil,
        isEnabled: false,
        isHidden: false
    )
}

func makeCompactStatusMenuDescriptors(
    summaryTitle: String,
    warningTitle: String? = nil,
    primaryTitle: String = "Start Proxy",
    primaryEnabled: Bool = true,
    hasBaseURL: Bool
) -> [CompactStatusMenuItemDescriptor] {
    [
        .init(
            title: summaryTitle,
            tag: 2,
            action: nil,
            isEnabled: false,
            isHidden: false
        ),
        .init(
            title: warningTitle ?? "",
            tag: 8,
            action: nil,
            isEnabled: false,
            isHidden: warningTitle == nil
        ),
        .separator,
        .init(
            title: primaryTitle,
            tag: 3,
            action: .primary,
            isEnabled: primaryEnabled,
            isHidden: false
        ),
        .init(
            title: "Open Vekil…",
            tag: 5,
            action: .openVekil,
            isEnabled: true,
            isHidden: false
        ),
        .init(
            title: "Settings…",
            tag: 12,
            action: .settings,
            isEnabled: true,
            isHidden: false
        ),
        .init(
            title: "Copy Base URL",
            tag: 4,
            action: .copyBaseURL,
            isEnabled: hasBaseURL,
            isHidden: !hasBaseURL
        ),
        .separator,
        .init(
            title: "Quit Vekil",
            tag: nil,
            action: .quit,
            isEnabled: true,
            isHidden: false
        ),
    ]
}

@MainActor
public final class VekilApplicationCoordinator: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private let appState: VekilAppState, analytics: AnalyticsViewModel, gate: ApplicationInstanceGate
    private let shutdownRuntime: @Sendable () async -> Void
    private var window: NSWindow?, statusItem: NSStatusItem?, cancellables = Set<AnyCancellable>()
    private var windowDidCloseObserver: NSObjectProtocol?
    private var windowDidBecomeKeyObserver: NSObjectProtocol?
    private var terminating = false

    public init(
        appState: VekilAppState, analytics: AnalyticsViewModel, gate: ApplicationInstanceGate,
        shutdownRuntime: @escaping @Sendable () async -> Void = {}
    ) {
        self.appState = appState; self.analytics = analytics; self.gate = gate
        self.shutdownRuntime = shutdownRuntime
    }

    public func applicationWillFinishLaunching(_: Notification) {
        NSApp.setActivationPolicy(.accessory)
    }

    public func applicationDidFinishLaunching(_: Notification) {
        installStatusMenu(); gate.observe { [weak self] in self?.showWindow() }
        windowDidBecomeKeyObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.didBecomeKeyNotification, object: nil, queue: .main
        ) { _ in DispatchQueue.main.async { NSApp.setActivationPolicy(.regular) } }
        windowDidCloseObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.willCloseNotification, object: nil, queue: .main
        ) { [weak self] _ in Task { @MainActor in self?.scheduleActivationPolicyUpdate() } }
        appState.objectWillChange.receive(on: RunLoop.main).sink { [weak self] _ in DispatchQueue.main.async {
            self?.refreshMenu(); if let self {
                self.analytics.applyRuntime(self.appState.runtimeState)
            }
        } }.store(in: &cancellables)
        Task {
            let initialized = await appState.initialize()
            if !initialized {
                fputs("Vekil runtime initialization failed.\n", stderr)
            }
            analytics.applyRuntime(appState.runtimeState)
        }
    }

    public func applicationShouldHandleReopen(_: NSApplication, hasVisibleWindows _: Bool) -> Bool {
        showWindow(); return true
    }

    public func applicationShouldTerminateAfterLastWindowClosed(_: NSApplication) -> Bool {
        false
    }

    public func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        if terminating {
            return .terminateNow
        }; terminating = true
        Task {
            await analytics.store.shutdown()
            await shutdownRuntime()
            sender.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    public func windowWillClose(_: Notification) {
        analytics.setVisible(.menu, true)
        scheduleActivationPolicyUpdate()
    }

    private func scheduleActivationPolicyUpdate() {
        DispatchQueue.main.async {
            let hasVisibleWindow = NSApp.windows.contains { candidate in
                candidate.isVisible && candidate.alphaValue > 0 && candidate.canBecomeKey
            }
            if !hasVisibleWindow {
                NSApp.setActivationPolicy(.accessory)
            }
        }
    }

    deinit {
        if let windowDidCloseObserver {
            NotificationCenter.default.removeObserver(windowDidCloseObserver)
        }
        if let windowDidBecomeKeyObserver {
            NotificationCenter.default.removeObserver(windowDidBecomeKeyObserver)
        }
    }

    private func showWindow() {
        NSApp.setActivationPolicy(.regular)
        if window == nil {
            let win = NSWindow(contentViewController: NSHostingController(rootView: VekilRootView(app: appState, analytics: analytics)))
            win.title = "Vekil"; win.styleMask = [.titled, .closable, .miniaturizable, .resizable]; win.setContentSize(.init(width: 980, height: 680)); win.minSize = .init(width: 860, height: 580); win.setFrameAutosaveName("VekilMainWindow"); win.delegate = self; win.isReleasedWhenClosed = false; window = win
        }
        window?.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true)
    }

    private var statusMenuSummaryTitle: String {
        "\(appState.presentation.title) · \(appState.runtimeState.configuration.displayName)"
    }

    private var statusMenuWarningTitle: String? {
        appState.lastError?.userMessage ?? appState.environmentTokenSignOutNotice
    }

    private func statusMenuSelector(for action: CompactStatusMenuAction) -> Selector {
        switch action {
        case .primary: #selector(primary)
        case .openVekil: #selector(openWindow)
        case .settings: #selector(openSettings)
        case .copyBaseURL: #selector(copyURL)
        case .quit: #selector(quit)
        }
    }

    private func installStatusMenu() {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem = item

        let menu = NSMenu()
        menu.autoenablesItems = false
        let descriptors = makeCompactStatusMenuDescriptors(
            summaryTitle: statusMenuSummaryTitle,
            warningTitle: statusMenuWarningTitle,
            primaryTitle: appState.primaryAction.title,
            primaryEnabled: appState.primaryAction.isEnabled,
            hasBaseURL: appState.baseURL != nil
        )
        for descriptor in descriptors {
            if descriptor.isSeparator {
                menu.addItem(.separator())
                continue
            }

            let action = descriptor.action.map { statusMenuSelector(for: $0) }
            let keyEquivalent = descriptor.action == .settings ? "," : ""
            let menuItem = NSMenuItem(
                title: descriptor.title ?? "", action: action, keyEquivalent: keyEquivalent
            )
            if descriptor.action == .settings {
                menuItem.keyEquivalentModifierMask = [.command]
            }
            menuItem.target = descriptor.action == nil ? nil : self
            menuItem.tag = descriptor.tag ?? 0
            menuItem.isEnabled = descriptor.isEnabled
            menuItem.isHidden = descriptor.isHidden
            menu.addItem(menuItem)
        }

        item.menu = menu
        analytics.setVisible(.menu, true)
        refreshMenu()
    }

    private func refreshMenu() {
        guard let menu = statusItem?.menu else { return }
        menu.item(withTag: 2)?.title = statusMenuSummaryTitle

        let primary = menu.item(withTag: 3)
        primary?.title = appState.primaryAction.title
        primary?.isEnabled = appState.primaryAction.isEnabled

        let hasBaseURL = appState.baseURL != nil
        let copyBaseURL = menu.item(withTag: 4)
        copyBaseURL?.isEnabled = hasBaseURL
        copyBaseURL?.isHidden = !hasBaseURL

        let persistentWarning = statusMenuWarningTitle
        menu.item(withTag: 8)?.title = persistentWarning ?? ""
        menu.item(withTag: 8)?.isHidden = persistentWarning == nil

        let symbol = appState.presentation.kind == .ready ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle"
        statusItem?.button?.image = NSImage(systemSymbolName: symbol, accessibilityDescription: appState.presentation.title)
        statusItem?.button?.toolTip = "Vekil — \(appState.presentation.title)"
        statusItem?.button?.setAccessibilityLabel("Vekil status menu")
        statusItem?.button?.setAccessibilityValue(appState.presentation.title)
        statusItem?.button?.setAccessibilityHelp("Open Vekil proxy controls and status")
    }

    @objc private func primary() {
        Task { await appState.performPrimaryAction() }
    }

    @objc private func copyURL() {
        Task { await appState.copyBaseURL() }
    }

    @objc private func openWindow() {
        showWindow()
    }

    @objc private func openSettings() {
        appState.selectDestination(.settings)
        showWindow()
    }

    @objc private func quit() {
        NSApp.terminate(nil)
    }
}
