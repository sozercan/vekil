import Darwin
import Foundation
import XCTest
import VekilCore
@testable import VekilUI

final class ShellSecurityTests: XCTestCase {
    func testBuildConfigurationSelectsExpectedHelperSignaturePolicy() {
#if VEKIL_DEVELOPMENT_BUILD
        let expected = RuntimeHelperSignaturePolicy.allowAdHocForDevelopment
#else
        let expected = RuntimeHelperSignaturePolicy.requireProductionTeam
#endif
        XCTAssertEqual(VekilBuildConfiguration.helperSignaturePolicy, expected)
    }

    func testShellArgumentQuotesModelIDs() {
        XCTAssertEqual(ShellArgument.quote("model name; rm -rf /"), "'model name; rm -rf /'")
        XCTAssertEqual(ShellArgument.quote("model'$(command)'"), "'model'\"'\"'$(command)'\"'\"''")
        XCTAssertEqual(ShellArgument.quote("<model-id>"), "'<model-id>'")
    }

    final class SignatureSpy: HelperCodeSignatureValidating, @unchecked Sendable {
        var calls: [URL] = []
        var applicationCalls: [URL] = []
        var runningCalls: [(Int32, RuntimeHelperCodeIdentity)] = []
        var runningError: Error?
        var runningValidation: ((Int32, RuntimeHelperCodeIdentity) throws -> Void)?
        let identity = RuntimeHelperCodeIdentity(codeDirectoryHash: Data([1, 2, 3]))

        func validate(
            at helperURL: URL,
            matchingApplicationAt applicationURL: URL
        ) throws -> RuntimeHelperCodeIdentity {
            calls.append(helperURL)
            applicationCalls.append(applicationURL)
            return identity
        }

        func validateRunningProcess(
            identifier: Int32,
            expectedIdentity: RuntimeHelperCodeIdentity
        ) throws {
            runningCalls.append((identifier, expectedIdentity))
            if let runningError { throw runningError }
            try runningValidation?(identifier, expectedIdentity)
        }
    }

    final class RuntimeProcessSpy: RuntimeProcess, @unchecked Sendable {
        let standardOutput = AsyncStream<Data> { $0.finish() }
        let standardError = AsyncStream<Data> { $0.finish() }
        let termination = AsyncStream<RuntimeProcessTermination> { $0.finish() }
        let processIdentifier: Int32?
        var runCount = 0
        var forceTerminateCount = 0

        init(processIdentifier: Int32? = 42) {
            self.processIdentifier = processIdentifier
        }

        func run() throws { runCount += 1 }
        func writeStandardInput(_: Data) async throws {}
        func closeStandardInput() {}
        func terminate() {}
        func forceTerminate() { forceTerminateCount += 1 }
    }

    struct RuntimeProcessFactoryStub: RuntimeProcessFactory, @unchecked Sendable {
        let process: RuntimeProcessSpy
        func makeProcess(configuration _: RuntimeProcessConfiguration) throws -> any RuntimeProcess {
            process
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
        XCTAssertEqual(
            try RuntimeHelperValidator(signature: spy).validate(bundleURL: bundle).executableURL.path,
            helper.path
        )
        XCTAssertEqual(spy.calls.map(\.path), [helper.path])
        XCTAssertEqual(spy.applicationCalls.map(\.path), [bundle.path])
        XCTAssertEqual(VekilBundleLayout.helperRelativePath, "Contents/Helpers/vekil-runtime")
    }

    func testValidatedFactoryChecksRunningCodeIdentity() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let bundle = root.appendingPathComponent("Vekil.app")
        let helper = VekilBundleLayout.helperURL(bundleURL: bundle)
        try FileManager.default.createDirectory(
            at: helper.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try Data("helper".utf8).write(to: helper)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: helper.path)
        defer { try? FileManager.default.removeItem(at: root) }

        let signature = SignatureSpy()
        let underlying = RuntimeProcessSpy(processIdentifier: 1234)
        signature.runningValidation = { identifier, identity in
            XCTAssertEqual(underlying.runCount, 1)
            XCTAssertEqual(identifier, 1234)
            XCTAssertEqual(identity, signature.identity)
        }
        let factory = ValidatingProcessFactory(
            bundleURL: bundle,
            validator: RuntimeHelperValidator(signature: signature),
            processFactory: RuntimeProcessFactoryStub(process: underlying)
        )
        let process = try factory.makeProcess(
            configuration: RuntimeProcessConfiguration(executableURL: helper)
        )
        try process.run()

        XCTAssertEqual(signature.runningCalls.count, 1)
        XCTAssertEqual(underlying.forceTerminateCount, 0)
    }

    func testValidatedFactoryTerminatesCodeIdentityMismatch() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let bundle = root.appendingPathComponent("Vekil.app")
        let helper = VekilBundleLayout.helperURL(bundleURL: bundle)
        try FileManager.default.createDirectory(
            at: helper.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try Data("helper".utf8).write(to: helper)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: helper.path)
        defer { try? FileManager.default.removeItem(at: root) }

        let signature = SignatureSpy()
        signature.runningError = RuntimeHelperValidationError.codeIdentityMismatch
        let underlying = RuntimeProcessSpy(processIdentifier: 1234)
        let factory = ValidatingProcessFactory(
            bundleURL: bundle,
            validator: RuntimeHelperValidator(signature: signature),
            processFactory: RuntimeProcessFactoryStub(process: underlying)
        )
        let process = try factory.makeProcess(
            configuration: RuntimeProcessConfiguration(executableURL: helper)
        )

        XCTAssertThrowsError(try process.run()) {
            XCTAssertEqual($0 as? RuntimeHelperValidationError, .codeIdentityMismatch)
        }
        XCTAssertEqual(underlying.forceTerminateCount, 1)
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

    func testRuntimeAppClientRefreshQueriesTheHelper() throws {
        let packageRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let sourceURL = packageRoot.appendingPathComponent("Sources/Vekil/RuntimeAppClient.swift")
        let source = try String(contentsOf: sourceURL, encoding: .utf8)
        let compact = source.components(separatedBy: .whitespacesAndNewlines).joined()

        XCTAssertTrue(
            compact.contains("letsnapshot=tryawaitcontroller.refreshSnapshot()"),
            "Refresh must send get_state through RuntimeController instead of remapping a cached snapshot"
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

    @MainActor func testNewPrimaryDiscardsActivationFromPreviousGeneration() throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: base, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: base) }

        var first: ApplicationInstanceGate? = try XCTUnwrap(
            ApplicationInstanceGate.acquire(
                identifier: "com.vekil.test.stale-activation",
                baseDirectory: base
            )
        )
        XCTAssertNil(
            try ApplicationInstanceGate.acquire(
                identifier: "com.vekil.test.stale-activation",
                baseDirectory: base
            )
        )
        XCTAssertNotNil(first)
        first = nil

        let replacement = try XCTUnwrap(
            ApplicationInstanceGate.acquire(
                identifier: "com.vekil.test.stale-activation",
                baseDirectory: base
            )
        )
        let activated = expectation(description: "stale activation")
        activated.isInverted = true
        replacement.observe { activated.fulfill() }
        wait(for: [activated], timeout: 0.2)
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
            processFactory: ValidatingProcessFactory(
                bundleURL: bundleURL,
                validator: RuntimeHelperValidator()
            )
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
