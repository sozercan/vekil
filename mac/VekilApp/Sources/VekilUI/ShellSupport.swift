import AppKit
import Combine
import Foundation
import Security
import UniformTypeIdentifiers
import VekilCore

enum ShellArgument {
    static func quote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "'", with: "'\"'\"'") + "'"
    }
}

public enum VekilBundleLayout {
    public static let helperRelativePath = "Contents/Helpers/vekil-runtime"
    public static func helperURL(bundleURL: URL) -> URL {
        bundleURL.appendingPathComponent("Contents/Helpers/vekil-runtime")
    }
}

public protocol HelperCodeSignatureValidating: Sendable {
    func validate(
        at helperURL: URL,
        matchingApplicationAt applicationURL: URL
    ) throws -> RuntimeHelperCodeIdentity
    func validateRunningProcess(
        identifier: Int32,
        expectedIdentity: RuntimeHelperCodeIdentity
    ) throws
}

public struct RuntimeHelperCodeIdentity: Equatable, Sendable {
    public var codeDirectoryHash: Data

    public init(codeDirectoryHash: Data) {
        self.codeDirectoryHash = codeDirectoryHash
    }
}

public enum RuntimeHelperSignaturePolicy: Equatable, Sendable {
    case requireProductionTeam
    case allowAdHocForTesting
}

public struct SecurityHelperCodeSignatureValidator: HelperCodeSignatureValidating {
    private let policy: RuntimeHelperSignaturePolicy

    public init(policy: RuntimeHelperSignaturePolicy = .requireProductionTeam) {
        self.policy = policy
    }

    public func validate(
        at helperURL: URL,
        matchingApplicationAt applicationURL: URL
    ) throws -> RuntimeHelperCodeIdentity {
        let helper = try staticCode(at: helperURL)
        let application = try staticCode(at: applicationURL)
        let runningApplication = try currentApplicationCode()
        let staticFlags = SecCSFlags(
            rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures
        )
        let runningFlags = SecCSFlags(rawValue: kSecCSStrictValidate)
        guard SecStaticCodeCheckValidity(helper, staticFlags, nil) == errSecSuccess,
              SecStaticCodeCheckValidity(application, staticFlags, nil) == errSecSuccess,
              SecCodeCheckValidity(runningApplication, runningFlags, nil) == errSecSuccess else {
            throw RuntimeHelperValidationError.invalidSignature
        }

        let runningStaticCode = try staticCode(for: runningApplication)
        guard try codeURL(for: runningStaticCode).resolvingSymlinksInPath().standardizedFileURL
            == applicationURL.resolvingSymlinksInPath().standardizedFileURL else {
            throw RuntimeHelperValidationError.signatureIdentityMismatch
        }
        let runningRequirement = try designatedRequirement(for: runningStaticCode)
        guard SecStaticCodeCheckValidity(
            application,
            runningFlags,
            runningRequirement
        ) == errSecSuccess else {
            throw RuntimeHelperValidationError.signatureIdentityMismatch
        }

        let runningTeam = try teamIdentifier(for: runningStaticCode)
        guard try teamIdentifier(for: application) == runningTeam else {
            throw RuntimeHelperValidationError.signatureIdentityMismatch
        }
        if policy == .requireProductionTeam, runningTeam == nil {
            throw RuntimeHelperValidationError.missingTeamIdentifier
        }
        guard try teamIdentifier(for: helper) == runningTeam else {
            throw RuntimeHelperValidationError.signatureIdentityMismatch
        }
        return RuntimeHelperCodeIdentity(codeDirectoryHash: try codeDirectoryHash(for: helper))
    }

    public func validateRunningProcess(
        identifier: Int32,
        expectedIdentity: RuntimeHelperCodeIdentity
    ) throws {
        var code: SecCode?
        let attributes = [
            kSecGuestAttributePid as String: NSNumber(value: identifier)
        ] as CFDictionary
        guard SecCodeCopyGuestWithAttributes(nil, attributes, [], &code) == errSecSuccess,
              let code else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        let flags = SecCSFlags(rawValue: kSecCSStrictValidate)
        guard SecCodeCheckValidity(code, flags, nil) == errSecSuccess else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        var staticCode: SecStaticCode?
        guard SecCodeCopyStaticCode(code, [], &staticCode) == errSecSuccess,
              let staticCode,
              try codeDirectoryHash(for: staticCode) == expectedIdentity.codeDirectoryHash else {
            throw RuntimeHelperValidationError.codeIdentityMismatch
        }
    }

    private func staticCode(at url: URL) throws -> SecStaticCode {
        var code: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, [], &code) == errSecSuccess, let code else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return code
    }

    private func currentApplicationCode() throws -> SecCode {
        var code: SecCode?
        guard SecCodeCopySelf([], &code) == errSecSuccess, let code else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return code
    }

    private func staticCode(for code: SecCode) throws -> SecStaticCode {
        var staticCode: SecStaticCode?
        guard SecCodeCopyStaticCode(code, [], &staticCode) == errSecSuccess,
              let staticCode else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return staticCode
    }

    private func codeURL(for code: SecStaticCode) throws -> URL {
        var url: CFURL?
        guard SecCodeCopyPath(code, [], &url) == errSecSuccess, let url else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return url as URL
    }

    private func designatedRequirement(for code: SecStaticCode) throws -> SecRequirement {
        var requirement: SecRequirement?
        guard SecCodeCopyDesignatedRequirement(code, [], &requirement) == errSecSuccess,
              let requirement else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return requirement
    }

    private func teamIdentifier(for code: SecStaticCode) throws -> String? {
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
            code, SecCSFlags(rawValue: kSecCSSigningInformation), &information
        ) == errSecSuccess, let values = information as? [String: Any] else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        guard let identifier = values[kSecCodeInfoTeamIdentifier as String] as? String else {
            return nil
        }
        let trimmed = identifier.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }

    private func codeDirectoryHash(for code: SecStaticCode) throws -> Data {
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
            code, SecCSFlags(rawValue: kSecCSSigningInformation), &information
        ) == errSecSuccess,
            let values = information as? [String: Any],
            let identity = values[kSecCodeInfoUnique as String] as? Data,
            !identity.isEmpty else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return identity
    }
}

public enum RuntimeHelperValidationError: Error, Equatable, LocalizedError {
    case outsideBundle, missing, symlink, notRegular, notExecutable, ownerMismatch
    case invalidSignature, missingTeamIdentifier, signatureIdentityMismatch, codeIdentityMismatch
    case changedDuringValidation, missingProcessIdentifier
    public var errorDescription: String? {
        switch self {
        case .outsideBundle: "The runtime helper is outside Vekil.app."
        case .missing: "The runtime helper is missing."
        case .symlink: "The runtime helper path must not contain symbolic links."
        case .notRegular: "The runtime helper is not a regular file."
        case .notExecutable: "The runtime helper is not executable."
        case .ownerMismatch: "The runtime helper owner does not match Vekil.app."
        case .invalidSignature: "The runtime helper code signature is invalid."
        case .missingTeamIdentifier: "The production app signature has no Team Identifier."
        case .signatureIdentityMismatch: "The runtime helper signature does not match Vekil.app."
        case .codeIdentityMismatch: "The running helper does not match the validated runtime helper."
        case .changedDuringValidation: "The runtime helper changed while Vekil was validating it."
        case .missingProcessIdentifier: "Vekil could not identify the running helper process."
        }
    }
}

public struct ValidatedRuntimeHelper: Equatable, Sendable {
    public var executableURL: URL
    public var codeIdentity: RuntimeHelperCodeIdentity

    public init(executableURL: URL, codeIdentity: RuntimeHelperCodeIdentity) {
        self.executableURL = executableURL
        self.codeIdentity = codeIdentity
    }
}

public struct RuntimeHelperValidator: Sendable {
    private let signature: any HelperCodeSignatureValidating
    public init(signature: any HelperCodeSignatureValidating = SecurityHelperCodeSignatureValidator()) { self.signature = signature }

    public func validate(bundleURL: URL) throws -> ValidatedRuntimeHelper {
        let bundle = bundleURL.resolvingSymlinksInPath().standardizedFileURL
        let helper = VekilBundleLayout.helperURL(bundleURL: bundle).standardizedFileURL
        guard helper.path.hasPrefix(bundle.path + "/") else { throw RuntimeHelperValidationError.outsideBundle }
        var bundleStat = stat()
        guard lstat(bundle.path, &bundleStat) == 0 else { throw RuntimeHelperValidationError.missing }
        for path in [bundle.appendingPathComponent("Contents"), bundle.appendingPathComponent("Contents/Helpers")] {
            var value = stat()
            guard lstat(path.path, &value) == 0 else { throw RuntimeHelperValidationError.missing }
            guard value.st_mode & S_IFMT != S_IFLNK else { throw RuntimeHelperValidationError.symlink }
            guard value.st_mode & S_IFMT == S_IFDIR else { throw RuntimeHelperValidationError.notRegular }
        }
        var before = stat()
        guard lstat(helper.path, &before) == 0 else { throw RuntimeHelperValidationError.missing }
        guard before.st_mode & S_IFMT != S_IFLNK else { throw RuntimeHelperValidationError.symlink }
        guard before.st_mode & S_IFMT == S_IFREG else { throw RuntimeHelperValidationError.notRegular }
        guard before.st_uid == bundleStat.st_uid else { throw RuntimeHelperValidationError.ownerMismatch }
        guard access(helper.path, X_OK) == 0 else { throw RuntimeHelperValidationError.notExecutable }
        let fd = Darwin.open(helper.path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW)
        guard fd >= 0 else { throw RuntimeHelperValidationError.symlink }
        defer { Darwin.close(fd) }
        var after = stat()
        guard fstat(fd, &after) == 0, after.st_mode & S_IFMT == S_IFREG, after.st_ino == before.st_ino, after.st_dev == before.st_dev else {
            throw RuntimeHelperValidationError.notRegular
        }
        let codeIdentity = try signature.validate(at: helper, matchingApplicationAt: bundle)
        var final = stat()
        guard lstat(helper.path, &final) == 0,
              final.st_mode & S_IFMT == S_IFREG,
              final.st_ino == after.st_ino,
              final.st_dev == after.st_dev else {
            throw RuntimeHelperValidationError.changedDuringValidation
        }
        return ValidatedRuntimeHelper(executableURL: helper, codeIdentity: codeIdentity)
    }

    public func validateRunningProcess(
        identifier: Int32,
        expectedIdentity: RuntimeHelperCodeIdentity
    ) throws {
        try signature.validateRunningProcess(
            identifier: identifier,
            expectedIdentity: expectedIdentity
        )
    }
}

public struct ValidatingProcessFactory: RuntimeProcessFactory {
    private let bundleURL: URL
    private let validator: RuntimeHelperValidator
    private let processFactory: any RuntimeProcessFactory

    public init(
        bundleURL: URL,
        validator: RuntimeHelperValidator,
        processFactory: any RuntimeProcessFactory = FoundationRuntimeProcessFactory()
    ) {
        self.bundleURL = bundleURL
        self.validator = validator
        self.processFactory = processFactory
    }

    public func makeProcess(
        configuration: RuntimeProcessConfiguration
    ) throws -> any RuntimeProcess {
        let validated = try validator.validate(bundleURL: bundleURL)
        guard validated.executableURL.standardizedFileURL
            == configuration.executableURL.standardizedFileURL else {
            throw RuntimeHelperValidationError.outsideBundle
        }
        let process = try processFactory.makeProcess(configuration: configuration)
        return ValidatedRuntimeProcess(
            process: process,
            validator: validator,
            expectedIdentity: validated.codeIdentity
        )
    }
}

private final class ValidatedRuntimeProcess: RuntimeProcess, @unchecked Sendable {
    let standardOutput: AsyncStream<Data>
    let standardError: AsyncStream<Data>
    let termination: AsyncStream<RuntimeProcessTermination>

    private let process: any RuntimeProcess
    private let validator: RuntimeHelperValidator
    private let expectedIdentity: RuntimeHelperCodeIdentity

    var processIdentifier: Int32? { process.processIdentifier }

    init(
        process: any RuntimeProcess,
        validator: RuntimeHelperValidator,
        expectedIdentity: RuntimeHelperCodeIdentity
    ) {
        self.process = process
        self.validator = validator
        self.expectedIdentity = expectedIdentity
        standardOutput = process.standardOutput
        standardError = process.standardError
        termination = process.termination
    }

    func run() throws {
        try process.run()
        do {
            guard let identifier = process.processIdentifier else {
                throw RuntimeHelperValidationError.missingProcessIdentifier
            }
            try validator.validateRunningProcess(
                identifier: identifier,
                expectedIdentity: expectedIdentity
            )
        } catch {
            process.forceTerminate()
            throw error
        }
    }

    func writeStandardInput(_ data: Data) async throws {
        try await process.writeStandardInput(data)
    }

    func closeStandardInput() { process.closeStandardInput() }
    func terminate() { process.terminate() }
    func forceTerminate() { process.forceTerminate() }
}

@MainActor
public final class OpenPanelExternalConfigurationSelector: ExternalConfigurationPathSelecting {
    public init() {}
    public func selectExternalConfigurationPath() async throws -> URL? {
        let panel = NSOpenPanel()
        panel.title = "Choose External Provider Configuration"
        panel.prompt = "Choose"
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        panel.allowedContentTypes = [.json, UTType(filenameExtension: "yaml") ?? .plainText, UTType(filenameExtension: "yml") ?? .plainText]
        return await withCheckedContinuation { continuation in
            panel.begin { continuation.resume(returning: $0 == .OK ? panel.url : nil) }
        }
    }
}

@MainActor
public final class AnalyticsViewModel: ObservableObject {
    @Published public private(set) var state = StatsStoreState()
    @Published public private(set) var requests: [StatsProjectedRequest] = []
    public let store: StatsStore
    private var tick: Task<Void, Never>?
    private var runtimeUpdateTail: Task<Void, Never>?
    private var visibilityUpdateTail: Task<Void, Never>?
    private var visibleSurfaces: Set<StatsVisibility> = []

    public init(store: StatsStore) { self.store = store }
    public func applyRuntime(_ runtime: AppRuntimeStateSnapshot) {
        let service = AnalyticsServiceState(rawValue: runtime.service.rawValue) ?? .failed
        let identity = runtime.runtimeGeneration.map {
            AnalyticsRuntimeIdentity(
                launchIdentity: RuntimeLaunchIdentity(launchToken: runtime.launchToken, helperEpoch: runtime.helperEpoch),
                runtimeGeneration: $0
            )
        }
        let predecessor = runtimeUpdateTail
        runtimeUpdateTail = Task { [weak self, store] in
            await predecessor?.value
            guard !Task.isCancelled else { return }
            await store.updateRuntime(identity: identity, serviceState: service)
            await self?.reload()
        }
    }
    func waitForRuntimeUpdates() async {
        await runtimeUpdateTail?.value
    }
    func waitForVisibilityUpdates() async {
        await visibilityUpdateTail?.value
    }
    public func setVisible(_ surface: StatsVisibility, _ visible: Bool) {
        if visible {
            visibleSurfaces.insert(surface)
        } else {
            visibleSurfaces.remove(surface)
        }
        let predecessor = visibilityUpdateTail
        visibilityUpdateTail = Task { [weak self, store] in
            await predecessor?.value
            guard !Task.isCancelled else { return }
            await store.setVisibility(surface, isVisible: visible)
            await self?.reload()
        }
        if !visibleSurfaces.isEmpty, tick == nil {
            tick = Task { [weak self] in
                while !Task.isCancelled {
                    do {
                        try await Task.sleep(nanoseconds: 1_000_000_000)
                    } catch {
                        return
                    }
                    await self?.reload()
                }
            }
        } else if visibleSurfaces.isEmpty {
            tick?.cancel(); tick = nil
        }
    }

    deinit {
        runtimeUpdateTail?.cancel()
        visibilityUpdateTail?.cancel()
        tick?.cancel()
    }
    public func reload() async {
        state = await store.state()
        requests = await store.requests()
    }
}
