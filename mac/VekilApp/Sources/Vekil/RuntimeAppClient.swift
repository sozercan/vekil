import Foundation
import VekilCore
import VekilUI

struct ValidatingProcessFactory: RuntimeProcessFactory {
    let bundleURL: URL
    let validator: RuntimeHelperValidator
    private let foundation = FoundationRuntimeProcessFactory()
    func makeProcess(configuration: RuntimeProcessConfiguration) throws -> any RuntimeProcess {
        let validated = try validator.validate(bundleURL: bundleURL)
        guard validated.standardizedFileURL == configuration.executableURL.standardizedFileURL else { throw RuntimeHelperValidationError.outsideBundle }
        return try foundation.makeProcess(configuration: configuration)
    }
}

actor RuntimeAppClient: AppRuntimeClient {
    let controller: RuntimeController

    init(controller: RuntimeController) { self.controller = controller }
    func events() async -> AsyncStream<AppRuntimeClientEvent> {
        AsyncStream<AppRuntimeClientEvent> { continuation in
            let observation = Task { [controller] in
                let notifications = await controller.scopedNotificationStream()
                for await scoped in notifications {
                    guard !Task.isCancelled else { break }
                    guard let identity = scoped.launchIdentity else { continue }
                    if let event = Self.map(scoped.notification, identity: identity) {
                        continuation.yield(event)
                    }
                }
                continuation.finish()
            }
            continuation.onTermination = { @Sendable _ in observation.cancel() }
        }
    }

    func initialize() async throws -> AppRuntimeInitialization {
        do {
            _ = try await controller.connect()
            let snapshot = await controller.snapshot()
            guard let identity = snapshot.launchIdentity, let state = snapshot.currentState else {
                throw RuntimeControllerError.invalidResponsePayload
            }
            let mapped = Self.map(state, identity: identity)
            return AppRuntimeInitialization(
                state: mapped, configuration: mapped.configuration,
                helperBuild: snapshot.hello?.helperBuild, bundleBuildID: snapshot.hello?.bundleBuildID
            )
        } catch {
            let message = "Vekil runtime connection failed: \(error.localizedDescription)\n"
            try? FileHandle.standardError.write(contentsOf: Data(message.utf8))
            throw error
        }
    }
    func refreshState() async throws -> AppRuntimeStateSnapshot {
        let snapshot = await controller.snapshot()
        guard let identity = snapshot.launchIdentity, let state = snapshot.currentState else { throw RuntimeControllerError.notConnected }
        return Self.map(state, identity: identity)
    }
    func start(_ request: AppRuntimeStartRequest) async throws -> AppRuntimeOperationAcceptance {
        let expected = (await controller.currentState)?.payload.configRevision
        let payload = try JSONValue.encode(["expected_config_revision": expected ?? ""])
        return try await submit(.start, payload: payload)
    }
    func cancelOperation(id: String) async throws { try await controller.cancelOperation(id: id) }
    func stop() async throws -> AppRuntimeOperationAcceptance { try await submit(.stop) }
    func restart(_ request: AppRuntimeStartRequest) async throws -> AppRuntimeOperationAcceptance {
        if (await controller.currentState)?.payload.service == .running {
            let stopped = try await controller.performOperation(command: .stop)
            guard stopped.status == .succeeded else { throw stopped.error ?? RuntimeControllerError.protocolViolation }
        }
        return try await start(request)
    }
    func startDeviceAuthentication() async throws -> AppRuntimeOperationAcceptance { try await submit(.authDeviceStart) }
    func authenticateWithGitHubCLI() async throws -> AppRuntimeOperationAcceptance { try await submit(.authGitHubCLI) }
    func signOut() async throws -> AppRuntimeOperationAcceptance { try await submit(.authSignOut) }
    func selectExternalConfiguration(path: String) async throws -> AppRuntimeOperationAcceptance { try await submit(.selectExternalConfig, payload: try JSONValue.encode(["path": path])) }
    func reloadExternalConfiguration() async throws -> AppRuntimeOperationAcceptance { try await submit(.reloadExternalConfig) }
    func clearExternalConfiguration() async throws -> AppRuntimeOperationAcceptance { try await submit(.useManagedConfig) }

    private func submit(_ command: RuntimeCommand, payload: JSONValue? = nil) async throws -> AppRuntimeOperationAcceptance {
        let handle = try await controller.submitOperation(command: command, payload: payload)
        return AppRuntimeOperationAcceptance(accepted: true, operation: AppRuntimeOperation(id: handle.id, kind: AppRuntimeOperationKind(rawValue: handle.kind.rawValue)))
    }

    private static func map(_ notification: RuntimeControllerNotification, identity: RuntimeLaunchIdentity) -> AppRuntimeClientEvent? {
        switch notification {
        case let .connectionStateChanged(connection):
            let helper: AppRuntimeHelperState
            let error: AppRuntimeStructuredError?
            switch connection {
            case .idle, .stopped:
                helper = .stopped; error = nil
            case .launching, .awaitingHello, .reconciling:
                helper = .launching; error = nil
            case .connected:
                helper = .connected; error = nil
            case .restarting:
                helper = .restarting; error = nil
            case let .failed(failure):
                helper = .failed
                error = AppRuntimeStructuredError(
                    code: "helper_failed", userMessage: failure.localizedDescription,
                    retryable: false, recoveryAction: "restart_helper"
                )
            case .stopping:
                helper = .stopped; error = nil
            }
            return .connection(AppRuntimeConnectionEvent(
                launchToken: identity.launchToken, helperEpoch: identity.helperEpoch,
                helper: helper, error: error
            ))
        case let .state(state): return .state(map(state, identity: identity))
        case let .operation(operation):
            return .operation(AppRuntimeOperationEvent(
                launchToken: identity.launchToken, helperEpoch: identity.helperEpoch,
                stateRevision: operation.stateRevision ?? 0, operationID: operation.id,
                status: AppRuntimeOperationEventStatus(rawValue: operation.status.rawValue),
                operation: AppRuntimeOperation(id: operation.id, kind: AppRuntimeOperationKind(rawValue: operation.kind.rawValue), phase: operation.phase.map { AppRuntimeOperationPhase(rawValue: $0.rawValue) }),
                error: operation.error.map(map)
            ))
        case let .deviceCode(code):
            let expiry = code.payload.expiresInSeconds.map { Date().addingTimeInterval(TimeInterval($0)) } ?? Date().addingTimeInterval(900)
            guard let url = URL(string: code.payload.verificationURL) else { return nil }
            return .deviceCode(AppRuntimeDeviceCode(launchToken: identity.launchToken, helperEpoch: identity.helperEpoch, operationID: code.payload.operationID, verificationURL: url, userCode: code.payload.userCode, expiresAt: expiry))
        default: return nil
        }
    }

    private static func map(_ event: RuntimeStateEvent, identity: RuntimeLaunchIdentity) -> AppRuntimeStateSnapshot {
        let payload = event.payload
        let operation: AppRuntimeOperation? = {
            guard case let .active(summary) = payload.operation else { return nil }
            return AppRuntimeOperation(id: summary.id, kind: AppRuntimeOperationKind(rawValue: summary.kind.rawValue), phase: summary.phase.map { AppRuntimeOperationPhase(rawValue: $0.rawValue) })
        }()
        let config = payload.configuration
        let baseURL: URL? = payload.baseURL.flatMap { raw in
            if raw.contains("://") { return URL(string: raw) }
            return URL(string: "http://\(raw)")
        }
        return AppRuntimeStateSnapshot(
            launchToken: identity.launchToken, helperEpoch: identity.helperEpoch, stateRevision: event.stateRevision,
            runtimeGeneration: payload.runtimeGeneration, configRevision: payload.configRevision,
            helper: AppRuntimeHelperState(rawValue: payload.helper?.rawValue ?? "connected"),
            service: AppRuntimeServiceState(rawValue: payload.service.rawValue), readiness: AppRuntimeReadinessState(rawValue: payload.readiness.rawValue),
            authentication: AppRuntimeAuthentication(
                state: AppRuntimeAuthenticationState(rawValue: payload.auth.rawValue),
                source: AppRuntimeAuthenticationSource(rawValue: payload.authSource?.rawValue ?? "none")
            ),
            operation: operation,
            configuration: AppRuntimeConfigurationState(
                mode: AppRuntimeConfigurationMode(rawValue: config?.mode.rawValue ?? "legacy"),
                displayName: config?.selectedPath.map { URL(fileURLWithPath: $0).lastPathComponent } ?? (config?.mode == .managed ? "Managed providers" : "Copilot default"),
                selectedExternalPath: config?.selectedPath, selectedRevision: config?.selectedRevision, activeRevision: config?.activeRevision,
                drift: AppRuntimeConfigurationDrift(rawValue: config?.drift.rawValue ?? "none"), requiresGitHubAuthentication: payload.auth != .notRequired
            ), baseURL: baseURL, lastError: config?.lastError.map(map)
        )
    }
    private static func map(_ error: RuntimeStructuredError) -> AppRuntimeStructuredError {
        AppRuntimeStructuredError(code: error.code, userMessage: error.userMessage, retryable: error.retryable, recoveryAction: error.recoveryAction, fieldErrors: error.fieldErrors.map { AppRuntimeFieldError(path: $0.path, code: $0.code, message: $0.message) })
    }
}
