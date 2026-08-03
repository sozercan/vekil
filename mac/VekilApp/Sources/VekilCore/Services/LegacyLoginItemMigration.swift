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
        guard let plist = readOwnedPlist(), Self.isVekilPlist(plist) else {
            return FileManager.default.fileExists(atPath: plistURL.path) ? .invalid : .absent
        }
        let domain = "gui/\(uid)/\(Self.label)"
        let status = try? await runner.run(["print", domain])
        return status == 0 ? .enabled : .disabled
    }

    public func removeOwnedLegacyItem() async throws {
        guard let plist = readOwnedPlist(), Self.isVekilPlist(plist) else { return }
        _ = try? await runner.run(["bootout", "gui/\(uid)/\(Self.label)"])
        var value = stat()
        guard lstat(plistURL.path, &value) == 0 else { return }
        guard value.st_mode & S_IFMT == S_IFREG, value.st_uid == uid else { return }
        try FileManager.default.removeItem(at: plistURL)
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

    private func readOwnedPlist() -> [String: Any]? {
        var before = stat()
        guard lstat(plistURL.path, &before) == 0,
              before.st_mode & S_IFMT == S_IFREG,
              before.st_uid == uid,
              before.st_nlink == 1 else {
            return nil
        }
        let fd = Darwin.open(plistURL.path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW)
        guard fd >= 0 else { return nil }
        let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        guard let data = try? handle.readToEnd() else { return nil }
        var format = PropertyListSerialization.PropertyListFormat.xml
        guard let object = try? PropertyListSerialization.propertyList(
            from: data, options: [], format: &format
        ) else { return nil }
        return object as? [String: Any]
    }
}
