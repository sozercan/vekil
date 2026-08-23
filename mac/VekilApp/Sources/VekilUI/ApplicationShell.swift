import AppKit
import Combine
import Darwin
import Foundation
import SwiftUI
import VekilCore

public enum ApplicationInstanceGateError: Error { case unsafeDirectory, lockOpen(Int32), unsafeLock }

public final class ApplicationInstanceGate: @unchecked Sendable {
    public static let activationNotification = Notification.Name("com.vekil.menubar.activate-main-window")
    private static let privateDirectoryMode: mode_t = 0o700
    private let lockFD: Int32, directoryFD: Int32, uid: uid_t, identifier: String
    private let activationName: String
    private var observer: NSObjectProtocol?
    private var source: DispatchSourceFileSystemObject?

    private init(lockFD: Int32, directoryFD: Int32, uid: uid_t, identifier: String, activationName: String) {
        self.lockFD = lockFD; self.directoryFD = directoryFD; self.uid = uid; self.identifier = identifier; self.activationName = activationName
    }

    public static func acquire(identifier: String = "com.vekil.menubar", baseDirectory: URL? = nil) throws -> ApplicationInstanceGate? {
        guard !identifier.isEmpty, !identifier.contains("/") else {
            throw ApplicationInstanceGateError.unsafeLock
        }
        let uid = getuid()
        let base = (baseDirectory ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]).resolvingSymlinksInPath()
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        let baseFD = try openOwnedBaseDirectory(at: base, uid: uid)
        defer { Darwin.close(baseFD) }
        let appFD = try openOrCreatePrivateDirectory(named: "vekil", parentFD: baseFD, uid: uid)
        defer { Darwin.close(appFD) }
        let dirFD = try openOrCreatePrivateDirectory(
            named: "Singleton-\(uid)",
            parentFD: appFD,
            uid: uid
        )
        let lockName = "\(identifier)-\(uid).lock"
        let coordinationName = "\(identifier)-\(uid).coordination.lock"
        let activationName = "activate-\(uid).request"
        let coordinationFD: Int32
        do {
            coordinationFD = try openOwnedLockFile(
                named: coordinationName,
                directoryFD: dirFD,
                uid: uid
            )
        } catch {
            Darwin.close(dirFD)
            throw error
        }
        guard flock(coordinationFD, LOCK_EX) == 0 else {
            Darwin.close(coordinationFD)
            Darwin.close(dirFD)
            throw ApplicationInstanceGateError.unsafeLock
        }
        defer {
            flock(coordinationFD, LOCK_UN)
            Darwin.close(coordinationFD)
        }

        let fd: Int32
        do {
            fd = try openOwnedLockFile(named: lockName, directoryFD: dirFD, uid: uid)
        } catch {
            Darwin.close(dirFD)
            throw error
        }
        guard flock(fd, LOCK_EX | LOCK_NB) == 0 else {
            let request = openat(
                dirFD,
                activationName,
                O_CREAT | O_WRONLY | O_CLOEXEC | O_NOFOLLOW,
                0o600
            )
            if request >= 0 {
                var requestInfo = stat()
                if fstat(request, &requestInfo) == 0,
                   requestInfo.st_mode & S_IFMT == S_IFREG,
                   requestInfo.st_uid == uid,
                   requestInfo.st_nlink == 1,
                   fchmod(request, 0o600) == 0,
                   ftruncate(request, 0) == 0 {
                    _ = fsync(request)
                }
                Darwin.close(request)
                _ = fsync(dirFD)
            }
            DistributedNotificationCenter.default().postNotificationName(activationNotification, object: identifier, userInfo: ["uid": uid], deliverImmediately: true)
            Darwin.close(fd); Darwin.close(dirFD); return nil
        }
        _ = unlinkat(dirFD, activationName, 0)
        return ApplicationInstanceGate(lockFD: fd, directoryFD: dirFD, uid: uid, identifier: identifier, activationName: activationName)
    }

    private static func openOwnedLockFile(
        named name: String,
        directoryFD: Int32,
        uid: uid_t
    ) throws -> Int32 {
        let fd = openat(
            directoryFD,
            name,
            O_CREAT | O_RDWR | O_CLOEXEC | O_NOFOLLOW,
            0o600
        )
        guard fd >= 0 else { throw ApplicationInstanceGateError.lockOpen(errno) }
        var info = stat()
        guard fstat(fd, &info) == 0,
              info.st_mode & S_IFMT == S_IFREG,
              info.st_uid == uid,
              info.st_nlink == 1,
              fchmod(fd, 0o600) == 0 else {
            Darwin.close(fd)
            throw ApplicationInstanceGateError.unsafeLock
        }
        return fd
    }

    private static func openOwnedBaseDirectory(at url: URL, uid: uid_t) throws -> Int32 {
        let fd = Darwin.open(url.path, O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW)
        guard fd >= 0 else { throw ApplicationInstanceGateError.unsafeDirectory }
        var info = stat()
        guard fstat(fd, &info) == 0,
              info.st_mode & S_IFMT == S_IFDIR,
              info.st_uid == uid,
              info.st_mode & 0o022 == 0 else {
            Darwin.close(fd)
            throw ApplicationInstanceGateError.unsafeDirectory
        }
        return fd
    }

    private static func openOrCreatePrivateDirectory(
        named name: String,
        parentFD: Int32,
        uid: uid_t
    ) throws -> Int32 {
        if mkdirat(parentFD, name, privateDirectoryMode) != 0, errno != EEXIST {
            throw ApplicationInstanceGateError.unsafeDirectory
        }
        let fd = openat(parentFD, name, O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW)
        guard fd >= 0 else { throw ApplicationInstanceGateError.unsafeDirectory }
        var info = stat()
        guard fstat(fd, &info) == 0,
              info.st_mode & S_IFMT == S_IFDIR,
              info.st_uid == uid,
              fchmod(fd, privateDirectoryMode) == 0 else {
            Darwin.close(fd)
            throw ApplicationInstanceGateError.unsafeDirectory
        }
        return fd
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
        var st = stat(); guard fstatat(directoryFD, activationName, &st, AT_SYMLINK_NOFOLLOW) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_uid == uid, st.st_nlink == 1, unlinkat(directoryFD, activationName, 0) == 0 else { return }; handler()
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

    public func applicationDidBecomeActive(_: Notification) {
        Task { await appState.applicationDidBecomeActive() }
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
