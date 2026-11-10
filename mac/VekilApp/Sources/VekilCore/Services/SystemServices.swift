import Foundation

#if canImport(AppKit)
  import AppKit
#endif

#if canImport(ServiceManagement)
  import ServiceManagement
#endif

public enum LoginItemStatus: String, Codable, Equatable, Sendable {
  case disabled
  case enabled
  case requiresApproval
  case notFound
  case unavailable
}

@MainActor
public protocol LoginItemService: AnyObject {
  func currentStatus() async -> LoginItemStatus
  func setEnabled(_ enabled: Bool) async throws -> LoginItemStatus
  func openSystemSettings() async throws
}

public enum VekilSystemServiceError: Error, Equatable, LocalizedError, Sendable {
  case unavailable(String)
  case actionFailed(String)

  public var errorDescription: String? {
    switch self {
    case .unavailable(let message), .actionFailed(let message):
      return message
    }
  }
}

#if canImport(ServiceManagement) && canImport(AppKit)
  @available(macOS 13.0, *)
  @MainActor
  public final class SystemLoginItemService: LoginItemService {
    private let service: SMAppService
    private let legacyMigrator: any LegacyLoginItemMigrating

    public init(
      service: SMAppService = .mainApp,
      legacyMigrator: any LegacyLoginItemMigrating = LegacyLaunchAgentMigrator()
    ) {
      self.service = service
      self.legacyMigrator = legacyMigrator
    }

    public func currentStatus() async -> LoginItemStatus {
      let legacyIntent = await reconcileLegacyIntent()
      let status = Self.map(service.status)
      if legacyIntent == .enabled, status == .disabled || status == .notFound {
        return .requiresApproval
      }
      return status
    }

    public func setEnabled(_ enabled: Bool) async throws -> LoginItemStatus {
      if enabled {
        switch service.status {
        case .enabled, .requiresApproval:
          break
        case .notRegistered, .notFound:
          try service.register()
        @unknown default:
          try service.register()
        }
      } else {
        switch service.status {
        case .notRegistered, .notFound:
          break
        case .enabled, .requiresApproval:
          try await service.unregister()
        @unknown default:
          try await service.unregister()
        }
      }

      let status = Self.map(service.status)
      if !enabled || status == .enabled {
        try await legacyMigrator.removeOwnedLegacyItem()
      }
      return status
    }

    public func openSystemSettings() async throws {
      guard
        let url = URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"),
        NSWorkspace.shared.open(url)
      else {
        throw VekilSystemServiceError.actionFailed("Could not open Login Items settings.")
      }
    }

    private func reconcileLegacyIntent() async -> LegacyLoginIntent {
      let legacyIntent = await legacyMigrator.inspect()
      if legacyIntent == .enabled {
        switch service.status {
        case .notRegistered, .notFound:
          try? service.register()
        case .enabled, .requiresApproval:
          break
        @unknown default:
          break
        }
      }
      if service.status == .enabled, legacyIntent == .enabled || legacyIntent == .disabled {
        try? await legacyMigrator.removeOwnedLegacyItem()
      }
      return legacyIntent
    }

    private static func map(_ status: SMAppService.Status) -> LoginItemStatus {
      switch status {
      case .notRegistered:
        return .disabled
      case .enabled:
        return .enabled
      case .requiresApproval:
        return .requiresApproval
      case .notFound:
        return .notFound
      @unknown default:
        return .unavailable
      }
    }
  }
#endif

@MainActor
public final class UnavailableLoginItemService: LoginItemService {
  public init() {}

  public func currentStatus() async -> LoginItemStatus {
    .unavailable
  }

  public func setEnabled(_ enabled: Bool) async throws -> LoginItemStatus {
    throw VekilSystemServiceError.unavailable("Open at Login is unavailable in this build.")
  }

  public func openSystemSettings() async throws {
    throw VekilSystemServiceError.unavailable("Login Items settings are unavailable in this build.")
  }
}

@MainActor
public protocol BrowserService: AnyObject {
  func open(_ url: URL) async throws
}

#if canImport(AppKit)
  @MainActor
  public final class SystemBrowserService: BrowserService {
    public init() {}

    public func open(_ url: URL) async throws {
      guard NSWorkspace.shared.open(url) else {
        throw VekilSystemServiceError.actionFailed("Could not open the requested page.")
      }
    }
  }
#endif

@MainActor
public final class UnavailableBrowserService: BrowserService {
  public init() {}

  public func open(_ url: URL) async throws {
    throw VekilSystemServiceError.unavailable("Opening web pages is unavailable in this build.")
  }
}

@MainActor
public protocol ClipboardService: AnyObject {
  func copy(_ string: String) async throws
}

#if canImport(AppKit)
  @MainActor
  public final class SystemClipboardService: ClipboardService {
    public init() {}

    public func copy(_ string: String) async throws {
      let pasteboard = NSPasteboard.general
      pasteboard.clearContents()
      guard pasteboard.setString(string, forType: .string) else {
        throw VekilSystemServiceError.actionFailed("Could not copy to the clipboard.")
      }
    }
  }
#endif

@MainActor
public final class UnavailableClipboardService: ClipboardService {
  public init() {}

  public func copy(_ string: String) async throws {
    throw VekilSystemServiceError.unavailable("The clipboard is unavailable in this build.")
  }
}

/// Kept free of Sparkle types so the core target has no Sparkle dependency.
/// The app target can inject a closure that calls its Sparkle controller.
@MainActor
public protocol UpdaterService: AnyObject {
  var isAvailable: Bool { get }
  func checkForUpdates() async throws
}

@MainActor
public final class UnavailableUpdaterService: UpdaterService {
  public let isAvailable = false

  public init() {}

  public func checkForUpdates() async throws {
    throw VekilSystemServiceError.unavailable("Update checking is unavailable in this build.")
  }
}

@MainActor
public final class ClosureUpdaterService: UpdaterService {
  public typealias Check = @MainActor () async throws -> Void

  public let isAvailable: Bool
  private let check: Check

  public init(isAvailable: Bool = true, check: @escaping Check) {
    self.isAvailable = isAvailable
    self.check = check
  }

  public func checkForUpdates() async throws {
    guard isAvailable else {
      throw VekilSystemServiceError.unavailable("Update checking is unavailable in this build.")
    }
    try await check()
  }
}
