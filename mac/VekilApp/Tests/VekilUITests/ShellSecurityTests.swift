import Darwin
import Foundation
import XCTest
import VekilCore
@testable import VekilUI

final class ShellSecurityTests: XCTestCase {
    final class SignatureSpy: HelperCodeSignatureValidating, @unchecked Sendable {
        var calls: [URL] = []
        func validate(at helperURL: URL, matchingApplicationAt applicationURL: URL) throws {
            calls.append(helperURL)
        }
    }

    func testRequiredHelperBundlePathAndValidation() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let bundle = root.appendingPathComponent("Vekil.app")
        let helper = VekilBundleLayout.helperURL(bundleURL: bundle)
        try FileManager.default.createDirectory(at: helper.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("helper".utf8).write(to: helper)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: helper.path)
        defer { try? FileManager.default.removeItem(at: root) }
        let spy = SignatureSpy()
        XCTAssertEqual(try RuntimeHelperValidator(signature: spy).validate(bundleURL: bundle).path, helper.path)
        XCTAssertEqual(spy.calls.map(\.path), [helper.path])
        XCTAssertEqual(VekilBundleLayout.helperRelativePath, "Contents/Helpers/vekil-runtime")
    }

    func testSymlinkHelperIsRejected() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let bundle = root.appendingPathComponent("Vekil.app")
        let helper = VekilBundleLayout.helperURL(bundleURL: bundle)
        try FileManager.default.createDirectory(at: helper.deletingLastPathComponent(), withIntermediateDirectories: true)
        let outside = root.appendingPathComponent("outside")
        try Data().write(to: outside); try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: outside.path)
        try FileManager.default.createSymbolicLink(at: helper, withDestinationURL: outside)
        defer { try? FileManager.default.removeItem(at: root) }
        XCTAssertThrowsError(try RuntimeHelperValidator(signature: SignatureSpy()).validate(bundleURL: bundle)) { XCTAssertEqual($0 as? RuntimeHelperValidationError, .symlink) }
    }

    func testAppEntryPointWiresQuitToRuntimeShutdown() throws {
        // The executable target is not importable by this test target, so verify
        // its composition-root wiring directly rather than duplicating it in a
        // test-only factory that could drift from main.swift.
        let packageRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let sourceURL = packageRoot.appendingPathComponent("Sources/Vekil/main.swift")
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        let compact = source.components(separatedBy: .whitespacesAndNewlines).joined()

        XCTAssertTrue(
            compact.contains(
                "shutdownRuntime:{awaitcontroller.shutdown(reason:.quit)}"
            ),
            "Normal application termination must shut down the runtime helper"
        )
    }


    @MainActor func testSingletonCreatesMissingApplicationSupportParents() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let base = root.appendingPathComponent("Library/Application Support")
        defer { try? FileManager.default.removeItem(at: root) }
        let gate = try XCTUnwrap(
            ApplicationInstanceGate.acquire(identifier: "com.vekil.test.parents", baseDirectory: base)
        )
        withExtendedLifetime(gate) {}
        var info = stat()
        let privateDirectory = base
            .appendingPathComponent("vekil")
            .appendingPathComponent("Singleton-\(getuid())")
        XCTAssertEqual(lstat(privateDirectory.path, &info), 0)
        XCTAssertEqual(info.st_mode & S_IFMT, S_IFDIR)
        XCTAssertEqual(info.st_mode & 0o777, 0o700)
    }

    @MainActor func testSecondaryLaunchActivationPersistsUntilObserver() throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: base) }
        let first = try XCTUnwrap(ApplicationInstanceGate.acquire(identifier: "com.vekil.test", baseDirectory: base))
        XCTAssertNil(try ApplicationInstanceGate.acquire(identifier: "com.vekil.test", baseDirectory: base))
        let activated = expectation(description: "activation")
        first.observe { activated.fulfill() }
        wait(for: [activated], timeout: 1)
    }

    @MainActor func testSingletonRejectsSymlinkedApplicationDirectory() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let base = root.appendingPathComponent("ApplicationSupport")
        let outside = root.appendingPathComponent("outside")
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: outside, withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(
            at: base.appendingPathComponent("vekil"),
            withDestinationURL: outside
        )
        defer { try? FileManager.default.removeItem(at: root) }

        XCTAssertThrowsError(
            try ApplicationInstanceGate.acquire(
                identifier: "com.vekil.test.symlink-app",
                baseDirectory: base
            )
        )
    }

    @MainActor func testSingletonRejectsSymlinkedPerUserDirectory() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let base = root.appendingPathComponent("ApplicationSupport")
        let appDirectory = base.appendingPathComponent("vekil")
        let outside = root.appendingPathComponent("outside")
        try FileManager.default.createDirectory(at: appDirectory, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: outside, withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(
            at: appDirectory.appendingPathComponent("Singleton-\(getuid())"),
            withDestinationURL: outside
        )
        defer { try? FileManager.default.removeItem(at: root) }

        XCTAssertThrowsError(
            try ApplicationInstanceGate.acquire(
                identifier: "com.vekil.test.symlink-user",
                baseDirectory: base
            )
        )
    }
}

extension ShellSecurityTests {
    private struct PackagedValidatingFactory: RuntimeProcessFactory {
        let bundleURL: URL
        func makeProcess(configuration: RuntimeProcessConfiguration) throws -> any RuntimeProcess {
            let validated = try RuntimeHelperValidator().validate(bundleURL: bundleURL)
            guard validated.standardizedFileURL == configuration.executableURL.standardizedFileURL else {
                throw RuntimeHelperValidationError.outsideBundle
            }
            return try FoundationRuntimeProcessFactory().makeProcess(configuration: configuration)
        }
    }

    func testPackagedValidatedHelperIntegrationWhenConfigured() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard let bundlePath = environment["VEKIL_TEST_BUNDLE_PATH"],
              let expectedBuildID = environment["VEKIL_TEST_BUNDLE_BUILD_ID"] else {
            throw XCTSkip("Set VEKIL_TEST_BUNDLE_PATH and VEKIL_TEST_BUNDLE_BUILD_ID for packaged integration")
        }
        let bundleURL = URL(fileURLWithPath: bundlePath)
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let controller = RuntimeController(
            configuration: RuntimeControllerConfiguration(
                process: RuntimeProcessConfiguration(
                    executableURL: VekilBundleLayout.helperURL(bundleURL: bundleURL),
                    arguments: ["--host", "127.0.0.1", "--port", "0", "--parent-pid", String(ProcessInfo.processInfo.processIdentifier)],
                    environment: environment.merging(["HOME": root.appendingPathComponent("home").path]) { _, replacement in replacement }
                ),
                expectedBundleBuildID: expectedBuildID,
                restartPolicy: RuntimeRestartPolicy(maximumAutomaticRestarts: 0)
            ),
            processFactory: PackagedValidatingFactory(bundleURL: bundleURL)
        )
        do {
            _ = try await controller.connect()
            let snapshot = await controller.snapshot()
            XCTAssertEqual(snapshot.connectionState, .connected)
        } catch {
            let diagnostics = await controller.diagnosticsSnapshot()
            XCTFail("Validated packaged helper failed: \(error); diagnostics=\(diagnostics.text)")
        }
        await controller.shutdown()
    }
}
