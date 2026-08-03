import Foundation

public enum VekilInitializationState: Equatable, Sendable {
  case notStarted
  case initializing
  case initialized
  case failed
}

public enum VekilPresentationKind: String, Codable, Equatable, Sendable {
  case initializing
  case stopped
  case starting
  case ready
  case degraded
  case authenticationRequired
  case stopping
  case failed
  case helperUnavailable
}

public struct VekilPresentation: Equatable, Sendable {
  public var kind: VekilPresentationKind
  public var title: String
  public var detail: String?
  public var isWarning: Bool

  public init(
    kind: VekilPresentationKind, title: String, detail: String? = nil, isWarning: Bool = false
  ) {
    self.kind = kind
    self.title = title
    self.detail = detail
    self.isWarning = isWarning
  }
}

public enum VekilPrimaryActionKind: String, Codable, Equatable, Sendable {
  case startProxy
  case cancelStarting
  case stopProxy
  case none
}

public struct VekilPrimaryAction: Equatable, Sendable {
  public var kind: VekilPrimaryActionKind
  public var title: String
  public var isEnabled: Bool
  public var operationID: String?

  public init(
    kind: VekilPrimaryActionKind,
    title: String,
    isEnabled: Bool,
    operationID: String? = nil
  ) {
    self.kind = kind
    self.title = title
    self.isEnabled = isEnabled
    self.operationID = operationID
  }
}

public enum VekilPresentationProjector {
  public static func presentation(
    for state: AppRuntimeStateSnapshot,
    initialization: VekilInitializationState,
    persistentError: AppRuntimeStructuredError? = nil
  ) -> VekilPresentation {
    let error = persistentError ?? state.lastError

    switch initialization {
    case .notStarted, .initializing:
      return VekilPresentation(kind: .initializing, title: "Initializing Vekil…")
    case .failed:
      return VekilPresentation(
        kind: .failed,
        title: "Vekil Could Not Start",
        detail: error?.userMessage,
        isWarning: true
      )
    case .initialized:
      break
    }

    if state.helper == .launching {
      return VekilPresentation(kind: .initializing, title: "Starting Runtime…")
    }
    if state.helper == .restarting {
      return VekilPresentation(
        kind: .helperUnavailable,
        title: "Reconnecting to Runtime…",
        detail: error?.userMessage,
        isWarning: true
      )
    }
    if state.helper == .failed || state.helper == .stopped {
      return VekilPresentation(
        kind: .helperUnavailable,
        title: "Runtime Unavailable",
        detail: error?.userMessage,
        isWarning: true
      )
    }

    if state.service == .starting {
      return VekilPresentation(
        kind: .starting,
        title: "Starting Proxy…",
        detail: operationDetail(state.operation)
      )
    }
    if state.service == .stopping {
      return VekilPresentation(kind: .stopping, title: "Stopping Proxy…")
    }
    if state.service == .running {
      if state.readiness == .ready {
        return VekilPresentation(
          kind: .ready, title: "Proxy Ready", detail: state.baseURL?.absoluteString)
      }
      return VekilPresentation(
        kind: .degraded,
        title: state.readiness == .checking
          ? "Proxy Running — Checking Readiness" : "Proxy Running — Not Ready",
        detail: error?.userMessage,
        isWarning: true
      )
    }

    if state.configuration.requiresGitHubAuthentication,
      state.authentication.state == .signedOut || state.authentication.state == .failed
    {
      return VekilPresentation(
        kind: .authenticationRequired,
        title: "GitHub Sign In Required",
        detail: error?.userMessage,
        isWarning: true
      )
    }

    if state.service == .failed {
      return VekilPresentation(
        kind: .failed,
        title: "Proxy Failed",
        detail: error?.userMessage,
        isWarning: true
      )
    }

    return VekilPresentation(
      kind: .stopped,
      title: "Proxy Stopped",
      detail: error?.userMessage,
      isWarning: error != nil
    )
  }

  public static func primaryAction(
    for state: AppRuntimeStateSnapshot,
    initialization: VekilInitializationState,
    isSubmittingCommand: Bool,
    cancellationRequestedOperationID: String?
  ) -> VekilPrimaryAction {
    guard initialization == .initialized, state.helper == .connected else {
      return VekilPrimaryAction(kind: .startProxy, title: "Start Proxy", isEnabled: false)
    }

    let operation = state.operation
    if let cancellationRequestedOperationID,
      operation?.id == cancellationRequestedOperationID
    {
      return VekilPrimaryAction(kind: .none, title: "Stopping…", isEnabled: false)
    }

    if state.service == .stopping || operation?.kind == .stop || operation?.kind == .restart {
      return VekilPrimaryAction(kind: .none, title: "Stopping…", isEnabled: false)
    }

    if state.service == .starting || operation?.kind == .start {
      return VekilPrimaryAction(
        kind: operation == nil ? .none : .cancelStarting,
        title: operation == nil ? "Starting…" : "Cancel Starting",
        isEnabled: operation != nil && !isSubmittingCommand,
        operationID: operation?.id
      )
    }

    if operation != nil {
      let title = state.service == .running ? "Stop Proxy" : "Start Proxy"
      let kind: VekilPrimaryActionKind = state.service == .running ? .stopProxy : .startProxy
      return VekilPrimaryAction(kind: kind, title: title, isEnabled: false)
    }

    if state.service == .running {
      return VekilPrimaryAction(
        kind: .stopProxy,
        title: "Stop Proxy",
        isEnabled: !isSubmittingCommand
      )
    }

    return VekilPrimaryAction(
      kind: .startProxy,
      title: "Start Proxy",
      isEnabled: !isSubmittingCommand
    )
  }

  private static func operationDetail(_ operation: AppRuntimeOperation?) -> String? {
    guard let phase = operation?.phase else {
      return nil
    }
    switch phase {
    case .loadingConfiguration:
      return "Loading configuration"
    case .constructingServer:
      return "Constructing server"
    case .listenerStartup:
      return "Starting listener"
    case .startupAuthentication:
      return "Checking authentication"
    case .dynamicProviderModelValidation:
      return "Validating provider models"
    case .policyRoutingPreflight:
      return "Checking policy routing"
    case .readinessCheck:
      return "Checking readiness"
    case .cleanup:
      return "Cleaning up"
    default:
      return phase.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
    }
  }
}
