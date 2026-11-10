import Foundation

/// State-side helper lifecycle values. The raw-value representation lets a
/// future Runtime/Protocol adapter preserve values introduced after this shell
/// was compiled instead of collapsing them into an incorrect known state.
public struct AppRuntimeHelperState: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let stopped = Self(rawValue: "stopped")
  public static let launching = Self(rawValue: "launching")
  public static let connected = Self(rawValue: "connected")
  public static let restarting = Self(rawValue: "restarting")
  public static let failed = Self(rawValue: "failed")
}

public struct AppRuntimeServiceState: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let stopped = Self(rawValue: "stopped")
  public static let starting = Self(rawValue: "starting")
  public static let running = Self(rawValue: "running")
  public static let stopping = Self(rawValue: "stopping")
  public static let failed = Self(rawValue: "failed")
}

public struct AppRuntimeReadinessState: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let unknown = Self(rawValue: "unknown")
  public static let checking = Self(rawValue: "checking")
  public static let ready = Self(rawValue: "ready")
  public static let notReady = Self(rawValue: "not_ready")
  public static let stale = Self(rawValue: "stale")
}

public struct AppRuntimeAuthenticationState: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let notRequired = Self(rawValue: "not_required")
  public static let signedOut = Self(rawValue: "signed_out")
  public static let signingIn = Self(rawValue: "signing_in")
  public static let signedIn = Self(rawValue: "signed_in")
  public static let failed = Self(rawValue: "failed")
}

public struct AppRuntimeAuthenticationSource: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let none = Self(rawValue: "none")
  public static let environment = Self(rawValue: "environment")
  public static let vekil = Self(rawValue: "vekil")
  public static let githubCLI = Self(rawValue: "github_cli")
}

public struct AppRuntimeAuthentication: Codable, Equatable, Sendable {
  public var state: AppRuntimeAuthenticationState
  public var source: AppRuntimeAuthenticationSource

  public init(
    state: AppRuntimeAuthenticationState = .signedOut,
    source: AppRuntimeAuthenticationSource = .none
  ) {
    self.state = state
    self.source = source
  }
}

public struct AppRuntimeConfigurationMode: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let legacy = Self(rawValue: "legacy")
  public static let managed = Self(rawValue: "managed")
  public static let external = Self(rawValue: "external")
}

public struct AppRuntimeConfigurationDrift: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let none = Self(rawValue: "none")
  public static let changed = Self(rawValue: "changed")
  public static let missing = Self(rawValue: "missing")
  public static let unsafe = Self(rawValue: "unsafe")
  public static let invalid = Self(rawValue: "invalid")
}

public struct AppRuntimeConfigurationState: Codable, Equatable, Sendable {
  public var mode: AppRuntimeConfigurationMode
  public var displayName: String
  public var selectedExternalPath: String?
  public var selectedRevision: String?
  public var activeRevision: String?
  public var drift: AppRuntimeConfigurationDrift
  public var requiresGitHubAuthentication: Bool

  public init(
    mode: AppRuntimeConfigurationMode = .legacy,
    displayName: String = "Copilot default",
    selectedExternalPath: String? = nil,
    selectedRevision: String? = nil,
    activeRevision: String? = nil,
    drift: AppRuntimeConfigurationDrift = .none,
    requiresGitHubAuthentication: Bool = true
  ) {
    self.mode = mode
    self.displayName = displayName
    self.selectedExternalPath = selectedExternalPath
    self.selectedRevision = selectedRevision
    self.activeRevision = activeRevision
    self.drift = drift
    self.requiresGitHubAuthentication = requiresGitHubAuthentication
  }
}

public struct AppRuntimeOperationKind: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let start = Self(rawValue: "start")
  public static let stop = Self(rawValue: "stop")
  public static let restart = Self(rawValue: "restart")
  public static let validateManagedDraft = Self(rawValue: "validate_managed_draft")
  public static let applyManagedDraft = Self(rawValue: "apply_managed_draft")
  public static let ensureManagedConfig = Self(rawValue: "ensure_managed_config")
  public static let selectExternalConfig = Self(rawValue: "select_external_config")
  public static let reloadExternalConfig = Self(rawValue: "reload_external_config")
  public static let clearExternalConfig = Self(rawValue: "clear_external_config")
  public static let useManagedConfig = Self(rawValue: "use_managed_config")
  public static let authDevice = Self(rawValue: "auth_device_start")
  public static let authGitHubCLI = Self(rawValue: "auth_github_cli")
  public static let authSignOut = Self(rawValue: "auth_sign_out")
}

public struct AppRuntimeOperationPhase: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let loadingConfiguration = Self(rawValue: "loading_configuration")
  public static let constructingServer = Self(rawValue: "constructing_server")
  public static let listenerStartup = Self(rawValue: "listener_startup")
  public static let startupAuthentication = Self(rawValue: "startup_authentication")
  public static let dynamicProviderModelValidation = Self(
    rawValue: "dynamic_provider_model_validation")
  public static let policyRoutingPreflight = Self(rawValue: "policy_routing_preflight")
  public static let readinessCheck = Self(rawValue: "readiness_check")
  public static let cleanup = Self(rawValue: "cleanup")
}

public struct AppRuntimeOperation: Codable, Equatable, Sendable {
  public var id: String
  public var kind: AppRuntimeOperationKind
  public var phase: AppRuntimeOperationPhase?

  public init(id: String, kind: AppRuntimeOperationKind, phase: AppRuntimeOperationPhase? = nil) {
    self.id = id
    self.kind = kind
    self.phase = phase
  }
}

public struct AppRuntimeFieldError: Codable, Equatable, Sendable {
  public var path: String
  public var code: String
  public var message: String

  public init(path: String, code: String, message: String) {
    self.path = path
    self.code = code
    self.message = message
  }
}

/// The only error shape stored by app state. Runtime adapters should map raw
/// implementation failures into this allowlisted, user-safe form.
public struct AppRuntimeStructuredError: Error, Codable, Equatable, LocalizedError, Sendable {
  public var code: String
  public var userMessage: String
  public var retryable: Bool
  public var recoveryAction: String?
  public var fieldErrors: [AppRuntimeFieldError]

  public init(
    code: String,
    userMessage: String,
    retryable: Bool = false,
    recoveryAction: String? = nil,
    fieldErrors: [AppRuntimeFieldError] = []
  ) {
    self.code = code
    self.userMessage = userMessage
    self.retryable = retryable
    self.recoveryAction = recoveryAction
    self.fieldErrors = fieldErrors
  }

  public var errorDescription: String? {
    userMessage
  }
}

public struct AppRuntimeStateSnapshot: Codable, Equatable, Sendable {
  public var launchToken: UUID
  public var helperEpoch: String
  public var stateRevision: UInt64
  public var runtimeGeneration: UInt64?
  public var configRevision: String?
  public var helper: AppRuntimeHelperState
  public var service: AppRuntimeServiceState
  public var readiness: AppRuntimeReadinessState
  public var authentication: AppRuntimeAuthentication
  public var operation: AppRuntimeOperation?
  public var configuration: AppRuntimeConfigurationState
  public var baseURL: URL?
  public var lastError: AppRuntimeStructuredError?

  public init(
    launchToken: UUID = RuntimeLaunchIdentity.zero.launchToken,
    helperEpoch: String = "",
    stateRevision: UInt64 = 0,
    runtimeGeneration: UInt64? = nil,
    configRevision: String? = nil,
    helper: AppRuntimeHelperState = .stopped,
    service: AppRuntimeServiceState = .stopped,
    readiness: AppRuntimeReadinessState = .unknown,
    authentication: AppRuntimeAuthentication = AppRuntimeAuthentication(),
    operation: AppRuntimeOperation? = nil,
    configuration: AppRuntimeConfigurationState = AppRuntimeConfigurationState(),
    baseURL: URL? = nil,
    lastError: AppRuntimeStructuredError? = nil
  ) {
    self.launchToken = launchToken
    self.helperEpoch = helperEpoch
    self.stateRevision = stateRevision
    self.runtimeGeneration = runtimeGeneration
    self.configRevision = configRevision
    self.helper = helper
    self.service = service
    self.readiness = readiness
    self.authentication = authentication
    self.operation = operation
    self.configuration = configuration
    self.baseURL = baseURL
    self.lastError = lastError
  }

  public static let placeholder = AppRuntimeStateSnapshot()
}

public struct AppRuntimeInitialization: Equatable, Sendable {
  public var state: AppRuntimeStateSnapshot
  public var configuration: AppRuntimeConfigurationState
  public var helperBuild: String?
  public var bundleBuildID: String?

  public init(
    state: AppRuntimeStateSnapshot,
    configuration: AppRuntimeConfigurationState? = nil,
    helperBuild: String? = nil,
    bundleBuildID: String? = nil
  ) {
    self.state = state
    self.configuration = configuration ?? state.configuration
    self.helperBuild = helperBuild
    self.bundleBuildID = bundleBuildID
  }
}

public struct AppRuntimeOperationAcceptance: Equatable, Sendable {
  public var accepted: Bool
  public var operation: AppRuntimeOperation?

  public init(accepted: Bool = true, operation: AppRuntimeOperation? = nil) {
    self.accepted = accepted
    self.operation = operation
  }
}

public enum AppRuntimeStartReason: String, Codable, Equatable, Sendable {
  case userInitiated
  case automaticLaunch
}

public struct AppRuntimeStartRequest: Codable, Equatable, Sendable {
  public var reason: AppRuntimeStartReason
  public var allowsInteractiveAuthentication: Bool

  public init(reason: AppRuntimeStartReason, allowsInteractiveAuthentication: Bool) {
    self.reason = reason
    self.allowsInteractiveAuthentication = allowsInteractiveAuthentication
  }

  public static let userInitiated = AppRuntimeStartRequest(
    reason: .userInitiated,
    allowsInteractiveAuthentication: true
  )

  public static let automaticLaunch = AppRuntimeStartRequest(
    reason: .automaticLaunch,
    allowsInteractiveAuthentication: false
  )
}

public struct AppRuntimeOperationEventStatus: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let running = Self(rawValue: "running")
  public static let succeeded = Self(rawValue: "succeeded")
  public static let canceled = Self(rawValue: "canceled")
  public static let failed = Self(rawValue: "failed")

  public var isTerminal: Bool {
    self == .succeeded || self == .canceled || self == .failed
  }
}

public struct AppRuntimeOperationEvent: Codable, Equatable, Sendable {
  public var launchToken: UUID
  public var helperEpoch: String
  public var stateRevision: UInt64
  public var operationID: String
  public var status: AppRuntimeOperationEventStatus
  public var operation: AppRuntimeOperation?
  public var error: AppRuntimeStructuredError?

  public init(
    launchToken: UUID = RuntimeLaunchIdentity.zero.launchToken,
    helperEpoch: String,
    stateRevision: UInt64,
    operationID: String,
    status: AppRuntimeOperationEventStatus,
    operation: AppRuntimeOperation? = nil,
    error: AppRuntimeStructuredError? = nil
  ) {
    self.launchToken = launchToken
    self.helperEpoch = helperEpoch
    self.stateRevision = stateRevision
    self.operationID = operationID
    self.status = status
    self.operation = operation
    self.error = error
  }
}

public struct AppRuntimeDeviceCode: Codable, Equatable, Sendable {
  public var launchToken: UUID
  public var helperEpoch: String
  public var operationID: String
  public var verificationURL: URL
  public var userCode: String
  public var expiresAt: Date

  public init(
    launchToken: UUID = RuntimeLaunchIdentity.zero.launchToken,
    helperEpoch: String,
    operationID: String,
    verificationURL: URL,
    userCode: String,
    expiresAt: Date
  ) {
    self.launchToken = launchToken
    self.helperEpoch = helperEpoch
    self.operationID = operationID
    self.verificationURL = verificationURL
    self.userCode = userCode
    self.expiresAt = expiresAt
  }
}

public struct AppRuntimeConnectionEvent: Equatable, Sendable {
  public var launchToken: UUID
  public var helperEpoch: String
  public var helper: AppRuntimeHelperState
  public var error: AppRuntimeStructuredError?

  public init(
    launchToken: UUID, helperEpoch: String, helper: AppRuntimeHelperState,
    error: AppRuntimeStructuredError? = nil
  ) {
    self.launchToken = launchToken
    self.helperEpoch = helperEpoch
    self.helper = helper
    self.error = error
  }
}

public enum AppRuntimeClientEvent: Equatable, Sendable {
  case connection(AppRuntimeConnectionEvent)
  case state(AppRuntimeStateSnapshot)
  case operation(AppRuntimeOperationEvent)
  case deviceCode(AppRuntimeDeviceCode)
}
