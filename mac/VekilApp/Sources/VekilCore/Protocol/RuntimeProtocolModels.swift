import Foundation

// MARK: - Extensible string tokens

/// A protocol token that preserves unknown values for forward-compatible decoding.
public protocol RuntimeStringToken: Codable, Hashable, Sendable, CustomStringConvertible {
    var rawValue: String { get }
    init(_ rawValue: String)
}

public extension RuntimeStringToken {
    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        self.init(try container.decode(String.self))
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(rawValue)
    }

    var description: String { rawValue }
}

public struct RuntimeCommand: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let getState = Self("get_state")
    public static let describeConfig = Self("describe_config")
    public static let start = Self("start")
    public static let stop = Self("stop")
    public static let validateManagedDraft = Self("validate_managed_draft")
    public static let applyManagedDraft = Self("apply_managed_draft")
    public static let ensureManagedConfig = Self("ensure_managed_config")
    public static let selectExternalConfig = Self("select_external_config")
    public static let reloadExternalConfig = Self("reload_external_config")
    public static let useManagedConfig = Self("use_managed_config")
    public static let authDeviceStart = Self("auth_device_start")
    public static let authGitHubCLI = Self("auth_github_cli")
    public static let authSignOut = Self("auth_sign_out")
    public static let cancelOperation = Self("cancel_operation")
    public static let setSecretProjection = Self("set_secret_projection")
    public static let shutdown = Self("shutdown")
}

public struct RuntimeEventName: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let hello = Self("hello")
    public static let state = Self("state")
    public static let operation = Self("operation")
    public static let deviceCode = Self("device_code")
}

public struct RuntimeHelperLifecycle: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let stopped = Self("stopped")
    public static let launching = Self("launching")
    public static let connected = Self("connected")
    public static let restarting = Self("restarting")
    public static let failed = Self("failed")
}

public struct RuntimeServiceLifecycle: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let stopped = Self("stopped")
    public static let starting = Self("starting")
    public static let running = Self("running")
    public static let stopping = Self("stopping")
    public static let failed = Self("failed")
}

public struct RuntimeReadiness: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let unknown = Self("unknown")
    public static let checking = Self("checking")
    public static let ready = Self("ready")
    public static let notReady = Self("not_ready")
    public static let stale = Self("stale")
}

public struct RuntimeAuthenticationState: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let notRequired = Self("not_required")
    public static let signedOut = Self("signed_out")
    public static let signingIn = Self("signing_in")
    public static let signedIn = Self("signed_in")
    public static let failed = Self("failed")
}

public struct RuntimeAuthenticationSource: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let none = Self("none")
    public static let environment = Self("environment")
    public static let vekil = Self("vekil")
    public static let githubCLI = Self("github_cli")
}

public struct RuntimeOperationKind: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }
}

public struct RuntimeOperationPhase: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let loadingConfiguration = Self("loading_configuration")
    public static let constructingServer = Self("constructing_server")
    public static let listenerStartup = Self("listener_startup")
    public static let startupAuthentication = Self("startup_authentication")
    public static let dynamicProviderModelValidation = Self("dynamic_provider_model_validation")
    public static let policyRoutingPreflight = Self("policy_routing_preflight")
    public static let readinessCheck = Self("readiness_check")
    public static let cleanup = Self("cleanup")
}

public struct RuntimeOperationStatus: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let accepted = Self("accepted")
    public static let running = Self("running")
    public static let succeeded = Self("succeeded")
    public static let canceled = Self("canceled")
    public static let failed = Self("failed")

    public var isTerminal: Bool {
        self == .succeeded || self == .canceled || self == .failed
    }
}

public struct RuntimeConfigurationMode: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let legacy = Self("legacy")
    public static let managed = Self("managed")
    public static let external = Self("external")
}

public struct RuntimeConfigurationDrift: RuntimeStringToken {
    public let rawValue: String
    public init(_ rawValue: String) { self.rawValue = rawValue }

    public static let none = Self("none")
    public static let drifted = Self("drifted")
    public static let missing = Self("missing")
    public static let unsafe = Self("unsafe")
    public static let invalid = Self("invalid")
}

// MARK: - Envelopes

public struct RuntimeRequestEnvelope: Codable, Sendable, Equatable {
    public var version: Int
    public var id: String
    public var command: RuntimeCommand
    public var payload: JSONValue?

    public init(version: Int, id: String, command: RuntimeCommand, payload: JSONValue? = nil) {
        self.version = version
        self.id = id
        self.command = command
        self.payload = payload
    }

    private enum CodingKeys: String, CodingKey {
        case version = "v"
        case id
        case command
        case payload
    }
}

public struct RuntimeResponseEnvelope: Codable, Sendable, Equatable {
    public var version: Int
    public var id: String
    public var helperEpoch: String
    public var ok: Bool
    public var result: JSONValue?
    public var error: RuntimeStructuredError?

    public init(
        version: Int,
        id: String,
        helperEpoch: String,
        ok: Bool,
        result: JSONValue? = nil,
        error: RuntimeStructuredError? = nil
    ) {
        self.version = version
        self.id = id
        self.helperEpoch = helperEpoch
        self.ok = ok
        self.result = result
        self.error = error
    }

    public func validate() throws {
        guard !id.isEmpty else { throw RuntimeEnvelopeValidationError.emptyRequestID }
        guard !helperEpoch.isEmpty else { throw RuntimeEnvelopeValidationError.emptyHelperEpoch }

        if ok {
            guard error == nil else { throw RuntimeEnvelopeValidationError.successContainsError }
        } else {
            guard error != nil else { throw RuntimeEnvelopeValidationError.failureMissingError }
            guard result == nil else { throw RuntimeEnvelopeValidationError.failureContainsResult }
        }
    }

    private enum CodingKeys: String, CodingKey {
        case version = "v"
        case id
        case helperEpoch = "helper_epoch"
        case ok
        case result
        case error
    }
}

public struct RuntimeEventEnvelope: Codable, Sendable, Equatable {
    public var version: Int
    public var event: RuntimeEventName
    public var helperEpoch: String?
    public var stateRevision: UInt64?
    public var payload: JSONValue

    public init(
        version: Int,
        event: RuntimeEventName,
        helperEpoch: String? = nil,
        stateRevision: UInt64? = nil,
        payload: JSONValue
    ) {
        self.version = version
        self.event = event
        self.helperEpoch = helperEpoch
        self.stateRevision = stateRevision
        self.payload = payload
    }

    private enum CodingKeys: String, CodingKey {
        case version = "v"
        case event
        case helperEpoch = "helper_epoch"
        case stateRevision = "state_revision"
        case payload
    }
}

public enum RuntimeWireFrame: Sendable, Equatable {
    case request(RuntimeRequestEnvelope)
    case response(RuntimeResponseEnvelope)
    case event(RuntimeEventEnvelope)
}

public enum RuntimeEnvelopeValidationError: Error, Sendable, Equatable {
    case emptyRequestID
    case emptyHelperEpoch
    case missingStateRevision
    case successContainsError
    case failureMissingError
    case failureContainsResult
}

// MARK: - Structured errors

public struct RuntimeFieldError: Codable, Sendable, Equatable, Hashable {
    public var path: String
    public var code: String
    public var message: String

    public init(path: String, code: String, message: String) {
        self.path = path
        self.code = code
        self.message = message
    }
}

public struct RuntimeStructuredError: Error, Codable, Sendable, Equatable, LocalizedError {
    public var code: String
    public var userMessage: String
    public var retryable: Bool
    public var recoveryAction: String?
    public var fieldErrors: [RuntimeFieldError]

    public init(
        code: String,
        userMessage: String,
        retryable: Bool,
        recoveryAction: String? = nil,
        fieldErrors: [RuntimeFieldError] = []
    ) {
        self.code = code
        self.userMessage = userMessage
        self.retryable = retryable
        self.recoveryAction = recoveryAction
        self.fieldErrors = fieldErrors
    }

    public var errorDescription: String? { userMessage }

    private enum CodingKeys: String, CodingKey {
        case code
        case userMessage = "user_message"
        case retryable
        case recoveryAction = "recovery_action"
        case fieldErrors = "field_errors"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        code = try container.decode(String.self, forKey: .code)
        userMessage = try container.decode(String.self, forKey: .userMessage)
        retryable = try container.decode(Bool.self, forKey: .retryable)
        recoveryAction = try container.decodeIfPresent(String.self, forKey: .recoveryAction)
        fieldErrors = try container.decodeIfPresent([RuntimeFieldError].self, forKey: .fieldErrors) ?? []
    }
}

// MARK: - Event payloads

public struct RuntimeHelloPayload: Codable, Sendable, Equatable {
    public var protocolMin: Int
    public var protocolMax: Int
    public var helperBuild: String
    public var bundleBuildID: String
    public var pid: Int32
    public var helperEpoch: String

    public init(
        protocolMin: Int,
        protocolMax: Int,
        helperBuild: String,
        bundleBuildID: String,
        pid: Int32,
        helperEpoch: String
    ) {
        self.protocolMin = protocolMin
        self.protocolMax = protocolMax
        self.helperBuild = helperBuild
        self.bundleBuildID = bundleBuildID
        self.pid = pid
        self.helperEpoch = helperEpoch
    }

    private enum CodingKeys: String, CodingKey {
        case protocolMin = "protocol_min"
        case protocolMax = "protocol_max"
        case helperBuild = "helper_build"
        case bundleBuildID = "bundle_build_id"
        case pid
        case helperEpoch = "helper_epoch"
    }
}

private struct RuntimeHelloWirePayload: Decodable {
    var protocolMin: Int
    var protocolMax: Int
    var helperBuild: String
    var bundleBuildID: String
    var pid: Int32
    var helperEpoch: String?

    private enum CodingKeys: String, CodingKey {
        case protocolMin = "protocol_min"
        case protocolMax = "protocol_max"
        case helperBuild = "helper_build"
        case bundleBuildID = "bundle_build_id"
        case pid
        case helperEpoch = "helper_epoch"
    }
}

public struct RuntimeOperationSummary: Codable, Sendable, Equatable {
    public var id: String
    public var kind: RuntimeOperationKind
    public var phase: RuntimeOperationPhase?

    public init(id: String, kind: RuntimeOperationKind, phase: RuntimeOperationPhase? = nil) {
        self.id = id
        self.kind = kind
        self.phase = phase
    }
}

public enum RuntimeOperationState: Codable, Sendable, Equatable {
    case idle
    case active(RuntimeOperationSummary)

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .idle
            return
        }
        if let token = try? container.decode(String.self) {
            guard token == "idle" else {
                throw DecodingError.dataCorruptedError(
                    in: container,
                    debugDescription: "Unknown operation state token"
                )
            }
            self = .idle
            return
        }
        self = .active(try container.decode(RuntimeOperationSummary.self))
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .idle:
            try container.encode("idle")
        case let .active(summary):
            try container.encode(summary)
        }
    }
}

public struct RuntimeConfigurationState: Codable, Sendable, Equatable {
    public var mode: RuntimeConfigurationMode
    public var selectedPath: String?
    public var selectedRevision: String?
    public var activeRevision: String?
    public var drift: RuntimeConfigurationDrift
    public var lastError: RuntimeStructuredError?
    public var secretProjections: [RuntimeSecretProjectionRequirement]

    public init(
        mode: RuntimeConfigurationMode,
        selectedPath: String? = nil,
        selectedRevision: String? = nil,
        activeRevision: String? = nil,
        drift: RuntimeConfigurationDrift = .none,
        lastError: RuntimeStructuredError? = nil,
        secretProjections: [RuntimeSecretProjectionRequirement] = []
    ) {
        self.mode = mode
        self.selectedPath = selectedPath
        self.selectedRevision = selectedRevision
        self.activeRevision = activeRevision
        self.drift = drift
        self.lastError = lastError
        self.secretProjections = secretProjections
    }

    private enum CodingKeys: String, CodingKey {
        case mode
        case selectedMode = "selected_mode"
        case selectedPath = "selected_path"
        case selectedRevision = "selected_revision"
        case activeRevision = "active_revision"
        case drift
        case driftStatus = "drift_status"
        case drifted
        case lastError = "last_error"
        case errorCode = "error_code"
        case secretProjections = "secret_projections"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        if let mode = try container.decodeIfPresent(RuntimeConfigurationMode.self, forKey: .mode) {
            self.mode = mode
        } else {
            mode = try container.decode(RuntimeConfigurationMode.self, forKey: .selectedMode)
        }
        selectedPath = try container.decodeIfPresent(String.self, forKey: .selectedPath)
        selectedRevision = try container.decodeIfPresent(String.self, forKey: .selectedRevision)
        activeRevision = try container.decodeIfPresent(String.self, forKey: .activeRevision)
        drift = try container.decodeIfPresent(RuntimeConfigurationDrift.self, forKey: .driftStatus)
            ?? container.decodeIfPresent(RuntimeConfigurationDrift.self, forKey: .drift)
            ?? ((try container.decodeIfPresent(Bool.self, forKey: .drifted) ?? false) ? .drifted : .none)
        lastError = try container.decodeIfPresent(RuntimeStructuredError.self, forKey: .lastError)
        secretProjections = try container.decodeIfPresent(
            [RuntimeSecretProjectionRequirement].self,
            forKey: .secretProjections
        ) ?? []
        if lastError == nil, let code = try container.decodeIfPresent(String.self, forKey: .errorCode), !code.isEmpty {
            lastError = RuntimeStructuredError(code: code, userMessage: "The selected configuration is unavailable.", retryable: false, fieldErrors: [])
        }
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(mode, forKey: .mode)
        try container.encodeIfPresent(selectedPath, forKey: .selectedPath)
        try container.encodeIfPresent(selectedRevision, forKey: .selectedRevision)
        try container.encodeIfPresent(activeRevision, forKey: .activeRevision)
        try container.encode(drift, forKey: .drift)
        try container.encodeIfPresent(lastError, forKey: .lastError)
        if !secretProjections.isEmpty {
            try container.encode(secretProjections, forKey: .secretProjections)
        }
    }
}

public struct RuntimeSecretProjectionRequirement: Codable, Sendable, Equatable {
    public var configRevision: String
    public var secretGeneration: UInt64
    public var secrets: [RuntimeManagedSecretRequirement]

    public init(
        configRevision: String,
        secretGeneration: UInt64,
        secrets: [RuntimeManagedSecretRequirement]
    ) {
        self.configRevision = configRevision
        self.secretGeneration = secretGeneration
        self.secrets = secrets
    }

    private enum CodingKeys: String, CodingKey {
        case configRevision = "config_revision"
        case secretGeneration = "secret_generation"
        case secrets
    }
}

public struct RuntimeManagedSecretRequirement: Codable, Sendable, Equatable, Hashable {
    public var providerID: String
    public var providerUUID: String
    public var role: String
    public var reference: String

    public init(providerID: String, providerUUID: String, role: String, reference: String) {
        self.providerID = providerID
        self.providerUUID = providerUUID
        self.role = role
        self.reference = reference
    }

    private enum CodingKeys: String, CodingKey {
        case providerID = "provider_id"
        case providerUUID = "provider_uuid"
        case role
        case reference
    }
}

public struct RuntimeStatePayload: Codable, Sendable, Equatable {
    public var helper: RuntimeHelperLifecycle?
    public var runtimeGeneration: UInt64?
    public var configRevision: String?
    public var secretGeneration: UInt64?
    public var service: RuntimeServiceLifecycle
    public var readiness: RuntimeReadiness
    public var auth: RuntimeAuthenticationState
    public var authSource: RuntimeAuthenticationSource?
    public var operation: RuntimeOperationState
    public var configuration: RuntimeConfigurationState?
    public var baseURL: String?

    public init(
        helper: RuntimeHelperLifecycle? = nil,
        runtimeGeneration: UInt64? = nil,
        configRevision: String? = nil,
        secretGeneration: UInt64? = nil,
        service: RuntimeServiceLifecycle,
        readiness: RuntimeReadiness,
        auth: RuntimeAuthenticationState,
        authSource: RuntimeAuthenticationSource? = nil,
        operation: RuntimeOperationState = .idle,
        configuration: RuntimeConfigurationState? = nil,
        baseURL: String? = nil
    ) {
        self.helper = helper
        self.runtimeGeneration = runtimeGeneration
        self.configRevision = configRevision
        self.secretGeneration = secretGeneration
        self.service = service
        self.readiness = readiness
        self.auth = auth
        self.authSource = authSource
        self.operation = operation
        self.configuration = configuration
        self.baseURL = baseURL
    }

    private enum CodingKeys: String, CodingKey {
        case helper
        case runtimeGeneration = "runtime_generation"
        case configRevision = "config_revision"
        case secretGeneration = "secret_generation"
        case service
        case readiness
        case auth
        case authSource = "auth_source"
        case operation
        case configuration
        case baseURL = "base_url"
        case addr
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        helper = try container.decodeIfPresent(RuntimeHelperLifecycle.self, forKey: .helper)
        runtimeGeneration = try container.decodeIfPresent(UInt64.self, forKey: .runtimeGeneration)
        configRevision = try container.decodeIfPresent(String.self, forKey: .configRevision)
        secretGeneration = try container.decodeIfPresent(UInt64.self, forKey: .secretGeneration)
        service = try container.decode(RuntimeServiceLifecycle.self, forKey: .service)
        readiness = try container.decode(RuntimeReadiness.self, forKey: .readiness)
        auth = try container.decode(RuntimeAuthenticationState.self, forKey: .auth)
        authSource = try container.decodeIfPresent(RuntimeAuthenticationSource.self, forKey: .authSource)
        operation = try container.decodeIfPresent(RuntimeOperationState.self, forKey: .operation) ?? .idle
        configuration = try container.decodeIfPresent(RuntimeConfigurationState.self, forKey: .configuration)
        baseURL = try container.decodeIfPresent(String.self, forKey: .baseURL)
            ?? container.decodeIfPresent(String.self, forKey: .addr)
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encodeIfPresent(helper, forKey: .helper)
        try container.encodeIfPresent(runtimeGeneration, forKey: .runtimeGeneration)
        try container.encodeIfPresent(configRevision, forKey: .configRevision)
        try container.encodeIfPresent(secretGeneration, forKey: .secretGeneration)
        try container.encode(service, forKey: .service)
        try container.encode(readiness, forKey: .readiness)
        try container.encode(auth, forKey: .auth)
        try container.encodeIfPresent(authSource, forKey: .authSource)
        try container.encode(operation, forKey: .operation)
        try container.encodeIfPresent(configuration, forKey: .configuration)
        try container.encodeIfPresent(baseURL, forKey: .baseURL)
    }
}

public struct RuntimeStateEvent: Sendable, Equatable {
    public var helperEpoch: String
    public var stateRevision: UInt64
    public var payload: RuntimeStatePayload

    public init(helperEpoch: String, stateRevision: UInt64, payload: RuntimeStatePayload) {
        self.helperEpoch = helperEpoch
        self.stateRevision = stateRevision
        self.payload = payload
    }
}

public struct RuntimeOperationEventPayload: Codable, Sendable, Equatable {
    public var operationID: String
    public var kind: RuntimeOperationKind?
    public var phase: RuntimeOperationPhase?
    public var status: RuntimeOperationStatus
    public var runtimeGeneration: UInt64?
    public var error: RuntimeStructuredError?

    public init(
        operationID: String,
        kind: RuntimeOperationKind? = nil,
        phase: RuntimeOperationPhase? = nil,
        status: RuntimeOperationStatus,
        runtimeGeneration: UInt64? = nil,
        error: RuntimeStructuredError? = nil
    ) {
        self.operationID = operationID
        self.kind = kind
        self.phase = phase
        self.status = status
        self.runtimeGeneration = runtimeGeneration
        self.error = error
    }

    private enum CodingKeys: String, CodingKey {
        case operationID = "operation_id"
        case kind
        case phase
        case status
        case runtimeGeneration = "runtime_generation"
        case error
    }
}

public struct RuntimeOperationEvent: Sendable, Equatable {
    public var helperEpoch: String
    public var stateRevision: UInt64
    public var payload: RuntimeOperationEventPayload

    public init(helperEpoch: String, stateRevision: UInt64, payload: RuntimeOperationEventPayload) {
        self.helperEpoch = helperEpoch
        self.stateRevision = stateRevision
        self.payload = payload
    }
}

public struct RuntimeDeviceCodePayload: Codable, Sendable, Equatable {
    public var operationID: String
    public var verificationURL: String
    public var userCode: String
    public var expiresAt: String?
    public var expiresInSeconds: Int?

    public init(
        operationID: String,
        verificationURL: String,
        userCode: String,
        expiresAt: String? = nil,
        expiresInSeconds: Int? = nil
    ) {
        self.operationID = operationID
        self.verificationURL = verificationURL
        self.userCode = userCode
        self.expiresAt = expiresAt
        self.expiresInSeconds = expiresInSeconds
    }

    private enum CodingKeys: String, CodingKey {
        case operationID = "operation_id"
        case verificationURL = "verification_url"
        case userCode = "user_code"
        case expiresAt = "expires_at"
        case expiresInSeconds = "expires_in"
    }
}

public struct RuntimeDeviceCodeEvent: Sendable, Equatable {
    public var helperEpoch: String
    public var stateRevision: UInt64
    public var payload: RuntimeDeviceCodePayload

    public init(helperEpoch: String, stateRevision: UInt64, payload: RuntimeDeviceCodePayload) {
        self.helperEpoch = helperEpoch
        self.stateRevision = stateRevision
        self.payload = payload
    }
}

public struct RuntimeAdmissionResult: Codable, Sendable, Equatable {
    public var accepted: Bool
    public var operationID: String?

    public init(accepted: Bool, operationID: String? = nil) {
        self.accepted = accepted
        self.operationID = operationID
    }

    private enum CodingKeys: String, CodingKey {
        case accepted
        case operationID = "operation_id"
    }
}

public enum RuntimeDecodedEvent: Sendable, Equatable {
    case hello(RuntimeHelloPayload)
    case state(RuntimeStateEvent)
    case operation(RuntimeOperationEvent)
    case deviceCode(RuntimeDeviceCodeEvent)
    case unknown(RuntimeEventEnvelope)
}

public extension RuntimeEventEnvelope {
    func decodePayload() throws -> RuntimeDecodedEvent {
        switch event {
        case .hello:
            let body = try payload.decode(RuntimeHelloWirePayload.self)
            guard let epoch = body.helperEpoch ?? helperEpoch, !epoch.isEmpty else {
                throw RuntimeEnvelopeValidationError.emptyHelperEpoch
            }
            return .hello(
                RuntimeHelloPayload(
                    protocolMin: body.protocolMin,
                    protocolMax: body.protocolMax,
                    helperBuild: body.helperBuild,
                    bundleBuildID: body.bundleBuildID,
                    pid: body.pid,
                    helperEpoch: epoch
                )
            )
        case .state:
            guard let helperEpoch else { throw RuntimeEnvelopeValidationError.emptyHelperEpoch }
            guard let stateRevision else { throw RuntimeEnvelopeValidationError.missingStateRevision }
            return .state(
                RuntimeStateEvent(
                    helperEpoch: helperEpoch,
                    stateRevision: stateRevision,
                    payload: try payload.decode(RuntimeStatePayload.self)
                )
            )
        case .operation:
            guard let helperEpoch else { throw RuntimeEnvelopeValidationError.emptyHelperEpoch }
            guard let stateRevision else { throw RuntimeEnvelopeValidationError.missingStateRevision }
            return .operation(
                RuntimeOperationEvent(
                    helperEpoch: helperEpoch,
                    stateRevision: stateRevision,
                    payload: try payload.decode(RuntimeOperationEventPayload.self)
                )
            )
        case .deviceCode:
            guard let helperEpoch else { throw RuntimeEnvelopeValidationError.emptyHelperEpoch }
            guard let stateRevision else { throw RuntimeEnvelopeValidationError.missingStateRevision }
            return .deviceCode(
                RuntimeDeviceCodeEvent(
                    helperEpoch: helperEpoch,
                    stateRevision: stateRevision,
                    payload: try payload.decode(RuntimeDeviceCodePayload.self)
                )
            )
        default:
            return .unknown(self)
        }
    }
}

/// Process-local identity used in addition to helper_epoch. A fresh token is
/// created before every helper Process launch, so stale output is rejected even
/// if a later helper repeats epochs, request IDs, revisions, or generations.
public struct RuntimeLaunchIdentity: Codable, Hashable, Sendable {
    public var launchToken: UUID
    public var helperEpoch: String

    public init(launchToken: UUID, helperEpoch: String) {
        self.launchToken = launchToken
        self.helperEpoch = helperEpoch
    }

    public static let zero = RuntimeLaunchIdentity(
        launchToken: UUID(uuidString: "00000000-0000-0000-0000-000000000000")!,
        helperEpoch: ""
    )
}
