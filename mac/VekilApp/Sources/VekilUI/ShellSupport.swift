import AppKit
import Combine
import Foundation
import Security
import UniformTypeIdentifiers
import VekilCore

public enum VekilBundleLayout {
    public static let helperRelativePath = "Contents/Helpers/vekil-runtime"
    public static func helperURL(bundleURL: URL) -> URL {
        bundleURL.appendingPathComponent("Contents/Helpers/vekil-runtime")
    }
}

public protocol HelperCodeSignatureValidating: Sendable {
    func validate(at helperURL: URL, matchingApplicationAt applicationURL: URL) throws
}

public struct SecurityHelperCodeSignatureValidator: HelperCodeSignatureValidating {
    public init() {}

    public func validate(at helperURL: URL, matchingApplicationAt applicationURL: URL) throws {
        let helper = try staticCode(at: helperURL)
        let application = try staticCode(at: applicationURL)
        let flags = SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures)
        guard SecStaticCodeCheckValidity(helper, flags, nil) == errSecSuccess,
              SecStaticCodeCheckValidity(application, flags, nil) == errSecSuccess else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        guard try teamIdentifier(for: helper) == teamIdentifier(for: application) else {
            throw RuntimeHelperValidationError.signatureIdentityMismatch
        }
    }

    private func staticCode(at url: URL) throws -> SecStaticCode {
        var code: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, [], &code) == errSecSuccess, let code else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return code
    }

    private func teamIdentifier(for code: SecStaticCode) throws -> String? {
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
            code, SecCSFlags(rawValue: kSecCSSigningInformation), &information
        ) == errSecSuccess, let values = information as? [String: Any] else {
            throw RuntimeHelperValidationError.invalidSignature
        }
        return values[kSecCodeInfoTeamIdentifier as String] as? String
    }
}

public enum RuntimeHelperValidationError: Error, Equatable, LocalizedError {
    case outsideBundle, missing, symlink, notRegular, notExecutable, ownerMismatch, invalidSignature, signatureIdentityMismatch
    public var errorDescription: String? {
        switch self {
        case .outsideBundle: "The runtime helper is outside Vekil.app."
        case .missing: "The runtime helper is missing."
        case .symlink: "The runtime helper path must not contain symbolic links."
        case .notRegular: "The runtime helper is not a regular file."
        case .notExecutable: "The runtime helper is not executable."
        case .ownerMismatch: "The runtime helper owner does not match Vekil.app."
        case .invalidSignature: "The runtime helper code signature is invalid."
        case .signatureIdentityMismatch: "The runtime helper signature does not match Vekil.app."
        }
    }
}

public struct RuntimeHelperValidator: Sendable {
    private let signature: any HelperCodeSignatureValidating
    public init(signature: any HelperCodeSignatureValidating = SecurityHelperCodeSignatureValidator()) { self.signature = signature }

    public func validate(bundleURL: URL) throws -> URL {
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
        let application = bundle.appendingPathComponent("Contents/MacOS/Vekil")
        try signature.validate(at: helper, matchingApplicationAt: application)
        return helper
    }
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

    public init(store: StatsStore) { self.store = store }
    public func applyRuntime(_ runtime: AppRuntimeStateSnapshot) {
        Task {
            let service = AnalyticsServiceState(rawValue: runtime.service.rawValue) ?? .failed
            let identity = runtime.runtimeGeneration.map {
                AnalyticsRuntimeIdentity(
                    launchIdentity: RuntimeLaunchIdentity(launchToken: runtime.launchToken, helperEpoch: runtime.helperEpoch),
                    runtimeGeneration: $0
                )
            }
            await store.updateRuntime(identity: identity, serviceState: service)
            await reload()
        }
    }
    public func setVisible(_ surface: StatsVisibility, _ visible: Bool) {
        Task { await store.setVisibility(surface, isVisible: visible); await reload() }
        if visible, tick == nil {
            tick = Task { [weak self] in
                while !Task.isCancelled {
                    try? await Task.sleep(nanoseconds: 1_000_000_000)
                    await self?.reload()
                }
            }
        } else if !visible {
            tick?.cancel(); tick = nil
        }
    }
    public func reload() async {
        state = await store.state()
        requests = await store.requests()
    }
}
