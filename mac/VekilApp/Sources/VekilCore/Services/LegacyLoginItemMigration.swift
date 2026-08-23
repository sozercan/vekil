import Darwin
import Foundation

public enum LegacyLoginIntent: String, Sendable, Equatable {
    case absent
    case disabled
    case enabled
    case invalid
}

public protocol LegacyLoginItemMigrating: Sendable {
    func inspect() async -> LegacyLoginIntent
    func removeOwnedLegacyItem() async throws
}

public struct LegacyLaunchctlRunner: Sendable {
    public var run: @Sendable (_ arguments: [String]) async throws -> Int32

    public init(run: @escaping @Sendable (_ arguments: [String]) async throws -> Int32) {
        self.run = run
    }

    public static let live = LegacyLaunchctlRunner { arguments in
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        process.arguments = arguments
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        try process.run()
        process.waitUntilExit()
        return process.terminationStatus
    }
}

public actor LegacyLaunchAgentMigrator: LegacyLoginItemMigrating {
    public static let label = "com.vekil.menubar"

    private struct PlistIdentity: Equatable, Sendable {
        let device: dev_t
        let inode: ino_t
    }

    private struct OpenedPlist {
        let contents: [String: Any]
        let identity: PlistIdentity
    }

    private let plistURL: URL
    private let uid: uid_t
    private let runner: LegacyLaunchctlRunner

    public init(
        homeDirectory: URL = FileManager.default.homeDirectoryForCurrentUser,
        uid: uid_t = getuid(),
        runner: LegacyLaunchctlRunner = .live
    ) {
        plistURL = homeDirectory
            .appendingPathComponent("Library/LaunchAgents", isDirectory: true)
            .appendingPathComponent("\(Self.label).plist", isDirectory: false)
        self.uid = uid
        self.runner = runner
    }

    public func inspect() async -> LegacyLoginIntent {
        guard let plist = readOwnedPlist(), Self.isVekilPlist(plist.contents) else {
            return FileManager.default.fileExists(atPath: plistURL.path) ? .invalid : .absent
        }
        // The legacy shell persisted enabled intent by keeping this owned
        // plist on disk. Whether launchd has loaded it in the current session
        // does not change that saved preference.
        return .enabled
    }

    public func removeOwnedLegacyItem() async throws {
        guard let directoryFD = openLaunchAgentsDirectory() else { return }
        defer { Darwin.close(directoryFD) }
        guard let original = readOwnedPlist(at: directoryFD),
              Self.isVekilPlist(original.contents) else { return }
        let originalIdentity = original.identity
        let arguments = ["bootout", "gui/\(uid)/\(Self.label)"]
        let status = try await runner.run(arguments)
        // `launchctl bootout` exits with ESRCH when the service is not loaded.
        guard status == 0 || status == Self.serviceNotFoundStatus else {
            throw LegacyLoginItemMigrationError.launchctlFailed(
                arguments: arguments,
                status: status
            )
        }
        guard let current = readOwnedPlist(at: directoryFD),
              current.identity == originalIdentity,
              Self.isVekilPlist(current.contents),
              pathIdentity(at: directoryFD) == current.identity else { return }
        guard unlinkat(directoryFD, plistURL.lastPathComponent, 0) != 0 else { return }
        let removalError = errno
        guard removalError != ENOENT else { return }
        throw POSIXError(POSIXErrorCode(rawValue: removalError) ?? .EIO)
    }

    static func isVekilPlist(_ plist: [String: Any]) -> Bool {
        guard plist["Label"] as? String == label,
              plist["RunAtLoad"] as? Bool == true,
              let arguments = plist["ProgramArguments"] as? [String],
              !arguments.isEmpty else {
            return false
        }
        if arguments == ["/usr/bin/open", "-b", label] { return true }
        guard arguments.count == 1 else { return false }
        let executable = URL(fileURLWithPath: arguments[0]).standardizedFileURL
        return executable.lastPathComponent == "vekil-menubar"
            && executable.path.contains("/Vekil.app/Contents/MacOS/")
    }

    private func openLaunchAgentsDirectory() -> Int32? {
        let directoryURL = plistURL.deletingLastPathComponent()
        let fd = Darwin.open(
            directoryURL.path,
            O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW
        )
        guard fd >= 0 else { return nil }
        var value = stat()
        guard fstat(fd, &value) == 0,
              value.st_mode & S_IFMT == S_IFDIR,
              value.st_uid == uid else {
            Darwin.close(fd)
            return nil
        }
        return fd
    }

    private func readOwnedPlist() -> OpenedPlist? {
        guard let directoryFD = openLaunchAgentsDirectory() else { return nil }
        defer { Darwin.close(directoryFD) }
        return readOwnedPlist(at: directoryFD)
    }

    private func readOwnedPlist(at directoryFD: Int32) -> OpenedPlist? {
        let fd = openat(
            directoryFD,
            plistURL.lastPathComponent,
            O_RDONLY | O_CLOEXEC | O_NOFOLLOW
        )
        guard fd >= 0 else { return nil }
        defer { Darwin.close(fd) }
        var value = stat()
        guard fstat(fd, &value) == 0,
              value.st_mode & S_IFMT == S_IFREG,
              value.st_uid == uid,
              value.st_nlink == 1 else {
            return nil
        }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: false)
        guard let data = try? handle.readToEnd() else { return nil }
        var format = PropertyListSerialization.PropertyListFormat.xml
        guard let object = try? PropertyListSerialization.propertyList(
            from: data, options: [], format: &format
        ) else { return nil }
        guard let contents = object as? [String: Any] else { return nil }
        return OpenedPlist(
            contents: contents,
            identity: PlistIdentity(device: value.st_dev, inode: value.st_ino)
        )
    }

    private func pathIdentity(at directoryFD: Int32) -> PlistIdentity? {
        var value = stat()
        guard fstatat(
            directoryFD,
            plistURL.lastPathComponent,
            &value,
            AT_SYMLINK_NOFOLLOW
        ) == 0,
              value.st_mode & S_IFMT == S_IFREG,
              value.st_uid == uid,
              value.st_nlink == 1 else {
            return nil
        }
        return PlistIdentity(device: value.st_dev, inode: value.st_ino)
    }

    private static let serviceNotFoundStatus: Int32 = ESRCH
}

public enum LegacyLoginItemMigrationError: Error, Sendable, Equatable {
    case launchctlFailed(arguments: [String], status: Int32)
}
