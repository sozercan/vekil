import Darwin
import Foundation
import XCTest
@testable import VekilCore

final class LegacyLoginItemMigrationTests: XCTestCase {
    func testRecognizesOwnedLegacyItemAsEnabledWhenLaunchdHasNotLoadedIt() async throws {
        let home = try makeHome(plist: ownedPlist())
        defer { try? FileManager.default.removeItem(at: home) }

        let migrator = LegacyLaunchAgentMigrator(
            homeDirectory: home,
            runner: LegacyLaunchctlRunner { _ in 113 }
        )

        let intent = await migrator.inspect()

        XCTAssertEqual(intent, .enabled)
    }

    func testRejectsForeignMalformedAndSymlinkedItems() async throws {
        let foreign = try makeHome(plist: ownedPlist(label: "com.example.foreign"))
        defer { try? FileManager.default.removeItem(at: foreign) }
        let foreignIntent = await LegacyLaunchAgentMigrator(homeDirectory: foreign).inspect()
        XCTAssertEqual(foreignIntent, .invalid)

        let malformed = try makeHome(raw: Data("not a plist".utf8))
        defer { try? FileManager.default.removeItem(at: malformed) }
        let malformedIntent = await LegacyLaunchAgentMigrator(homeDirectory: malformed).inspect()
        XCTAssertEqual(malformedIntent, .invalid)

        let symlinkHome = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let launchAgents = symlinkHome.appendingPathComponent("Library/LaunchAgents")
        try FileManager.default.createDirectory(at: launchAgents, withIntermediateDirectories: true)
        let outside = symlinkHome.appendingPathComponent("outside.plist")
        try ownedPlist().write(to: outside)
        try FileManager.default.createSymbolicLink(
            at: launchAgents.appendingPathComponent("com.vekil.menubar.plist"),
            withDestinationURL: outside
        )
        defer { try? FileManager.default.removeItem(at: symlinkHome) }
        let symlinkIntent = await LegacyLaunchAgentMigrator(homeDirectory: symlinkHome).inspect()
        XCTAssertEqual(symlinkIntent, .invalid)
    }

    func testRemovalOnlyDeletesOwnedLegacyItem() async throws {
        let home = try makeHome(plist: ownedPlist())
        defer { try? FileManager.default.removeItem(at: home) }
        let migrator = LegacyLaunchAgentMigrator(
            homeDirectory: home,
            runner: LegacyLaunchctlRunner { _ in 0 }
        )
        try await migrator.removeOwnedLegacyItem()
        XCTAssertFalse(FileManager.default.fileExists(atPath: plistURL(home).path))

        let foreign = try makeHome(plist: ownedPlist(label: "com.example.foreign"))
        defer { try? FileManager.default.removeItem(at: foreign) }
        try await LegacyLaunchAgentMigrator(homeDirectory: foreign).removeOwnedLegacyItem()
        XCTAssertTrue(FileManager.default.fileExists(atPath: plistURL(foreign).path))
    }

    func testRemovalAcceptsVerifiedNotLoadedStatus() async throws {
        let home = try makeHome(plist: ownedPlist())
        defer { try? FileManager.default.removeItem(at: home) }
        let migrator = LegacyLaunchAgentMigrator(
            homeDirectory: home,
            runner: LegacyLaunchctlRunner { _ in 113 }
        )

        try await migrator.removeOwnedLegacyItem()

        XCTAssertFalse(FileManager.default.fileExists(atPath: plistURL(home).path))
    }

    func testRemovalPreservesOwnedItemWhenBootoutFails() async throws {
        let home = try makeHome(plist: ownedPlist())
        defer { try? FileManager.default.removeItem(at: home) }
        let migrator = LegacyLaunchAgentMigrator(
            homeDirectory: home,
            runner: LegacyLaunchctlRunner { _ in 5 }
        )

        do {
            try await migrator.removeOwnedLegacyItem()
            XCTFail("Expected launchctl failure")
        } catch {
            XCTAssertEqual(
                error as? LegacyLoginItemMigrationError,
                .launchctlFailed(
                    arguments: ["bootout", "gui/\(getuid())/com.vekil.menubar"],
                    status: 5
                )
            )
        }
        XCTAssertTrue(FileManager.default.fileExists(atPath: plistURL(home).path))
    }

    private func makeHome(plist: Data) throws -> URL { try makeHome(raw: plist) }

    private func makeHome(raw: Data) throws -> URL {
        let home = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let launchAgents = home.appendingPathComponent("Library/LaunchAgents")
        try FileManager.default.createDirectory(at: launchAgents, withIntermediateDirectories: true)
        try raw.write(to: plistURL(home), options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: plistURL(home).path)
        return home
    }

    private func plistURL(_ home: URL) -> URL {
        home.appendingPathComponent("Library/LaunchAgents/com.vekil.menubar.plist")
    }

    private func ownedPlist(label: String = "com.vekil.menubar") throws -> Data {
        try PropertyListSerialization.data(
            fromPropertyList: [
                "Label": label,
                "ProgramArguments": ["/usr/bin/open", "-b", "com.vekil.menubar"],
                "RunAtLoad": true,
                "KeepAlive": false,
            ],
            format: .xml,
            options: 0
        )
    }
}
