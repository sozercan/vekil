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

@MainActor
public final class VekilApplicationCoordinator: NSObject, NSApplicationDelegate, NSWindowDelegate,
    NSPopoverDelegate
{
    private let appState: VekilAppState, analytics: AnalyticsViewModel, gate: ApplicationInstanceGate
    private let shutdownRuntime: @Sendable () async -> Void
    private var window: NSWindow?, statusItem: NSStatusItem?, statusPopover: NSPopover?
    private var cancellables = Set<AnyCancellable>()
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
        appState.$isShowingOnboarding.removeDuplicates().sink { [weak self] isShowing in
            guard isShowing else { return }
            DispatchQueue.main.async { self?.showWindow() }
        }.store(in: &cancellables)
        Task {
            let initialized = await appState.initialize()
            if !initialized {
                fputs("Vekil runtime initialization failed.\n", stderr)
                showWindow()
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
        }
        persistMainWindowFrame()
        terminating = true
        Task {
            await analytics.store.shutdown()
            await shutdownRuntime()
            sender.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    public func windowDidMove(_ notification: Notification) {
        persistMainWindowFrame(from: notification)
    }

    public func windowDidResize(_ notification: Notification) {
        persistMainWindowFrame(from: notification)
    }

    public func windowWillClose(_ notification: Notification) {
        persistMainWindowFrame(from: notification)
        scheduleActivationPolicyUpdate()
    }

    public func popoverWillShow(_: Notification) {
        analytics.setVisible(.menu, true)
    }

    public func popoverDidClose(_: Notification) {
        analytics.setVisible(.menu, false)
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
            let win = NSWindow(
                contentViewController: NSHostingController(
                    rootView: VekilRootView(app: appState, analytics: analytics)
                )
            )
            win.title = "Vekil"
            win.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            win.setContentSize(.init(width: 980, height: 680))
            win.minSize = .init(width: 860, height: 580)
            restoreMainWindowFrame(on: win)
            win.delegate = self
            win.isReleasedWhenClosed = false
            window = win
        }
        window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func restoreMainWindowFrame(on window: NSWindow) {
        guard let saved = appState.mainWindowFrame else { return }
        var frame = NSRect(
            x: saved.x,
            y: saved.y,
            width: saved.width,
            height: saved.height
        )
        guard let screen = screen(for: saved.screenIdentifier, intersecting: frame) else { return }
        frame.size.width = min(max(frame.width, window.minSize.width), screen.visibleFrame.width)
        frame.size.height = min(max(frame.height, window.minSize.height), screen.visibleFrame.height)
        window.setFrame(window.constrainFrameRect(frame, to: screen), display: false)
    }

    private func persistMainWindowFrame(from notification: Notification? = nil) {
        guard let window else { return }
        if let source = notification?.object as? NSWindow, source !== window { return }
        let frame = window.frame
        appState.saveMainWindowFrame(
            VekilWindowFrame(
                x: frame.origin.x,
                y: frame.origin.y,
                width: frame.width,
                height: frame.height,
                screenIdentifier: screenIdentifier(window.screen)
            )
        )
    }

    private func screen(for identifier: String?, intersecting frame: NSRect) -> NSScreen? {
        if let identifier,
            let match = NSScreen.screens.first(where: { screenIdentifier($0) == identifier })
        {
            return match
        }
        return NSScreen.screens.first(where: { $0.visibleFrame.intersects(frame) }) ?? NSScreen.main
    }

    private func screenIdentifier(_ screen: NSScreen?) -> String? {
        guard
            let value = screen?.deviceDescription[NSDeviceDescriptionKey("NSScreenNumber")]
                as? NSNumber
        else { return nil }
        return value.stringValue
    }

    private func installStatusMenu() {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem = item

        let popover = NSPopover()
        popover.behavior = .transient
        popover.animates = !NSWorkspace.shared.accessibilityDisplayShouldReduceMotion
        popover.delegate = self
        popover.contentSize = NSSize(width: 340, height: 330)
        popover.contentViewController = NSHostingController(
            rootView: VekilMenuBarPopoverView(
                app: appState,
                analytics: analytics,
                openMainWindow: { [weak self] in
                    self?.statusPopover?.performClose(nil)
                    self?.showWindow()
                },
                openSettings: { [weak self] in
                    self?.statusPopover?.performClose(nil)
                    self?.openSettings()
                },
                quit: { [weak self] in
                    self?.statusPopover?.performClose(nil)
                    self?.quit()
                }
            )
        )
        statusPopover = popover

        if let button = item.button {
            button.target = self
            button.action = #selector(toggleStatusPopover(_:))
            button.sendAction(on: [.leftMouseUp])
        }
        refreshMenu()
    }

    private func refreshMenu() {
        let symbol = appState.presentation.kind == .ready ? "bolt.horizontal.circle.fill" : "bolt.horizontal.circle"
        statusItem?.button?.image = NSImage(systemSymbolName: symbol, accessibilityDescription: appState.presentation.title)
        statusItem?.button?.toolTip = "Vekil — \(appState.presentation.title)"
        statusItem?.button?.setAccessibilityLabel("Vekil status menu")
        statusItem?.button?.setAccessibilityValue(appState.presentation.title)
        statusItem?.button?.setAccessibilityHelp("Open Vekil proxy controls and status")
    }

    @objc private func toggleStatusPopover(_ sender: NSStatusBarButton) {
        guard let statusPopover else { return }
        if statusPopover.isShown {
            statusPopover.performClose(sender)
        } else {
            statusPopover.show(relativeTo: sender.bounds, of: sender, preferredEdge: .minY)
        }
    }

    private func openSettings() {
        appState.selectDestination(.settings)
        showWindow()
    }

    private func quit() {
        NSApp.terminate(nil)
    }
}
