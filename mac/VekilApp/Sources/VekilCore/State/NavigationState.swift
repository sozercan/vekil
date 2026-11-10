import Foundation

public enum VekilDestination: String, CaseIterable, Equatable, Sendable {
  case overview
  case activity
  case connection
  case clients
  case settings
  case about

  /// Maps destinations persisted by the pre-arrangement native shell onto the
  /// smaller durable navigation model.
  public init?(persistedRawValue: String) {
    switch persistedRawValue {
    case "overview": self = .overview
    case "activity", "traffic", "requests": self = .activity
    case "connection", "providers", "models": self = .connection
    case "clients", "client-setup": self = .clients
    case "settings": self = .settings
    case "about": self = .about
    default: return nil
    }
  }
}

/// Framework-neutral window geometry suitable for UserDefaults persistence.
/// Visibility is deliberately not persisted: cold, login, and update launches
/// remain menu-only even if the window was visible before the prior quit.
public struct VekilWindowFrame: Codable, Equatable, Sendable {
  public var x: Double
  public var y: Double
  public var width: Double
  public var height: Double
  public var screenIdentifier: String?

  public init(
    x: Double,
    y: Double,
    width: Double,
    height: Double,
    screenIdentifier: String? = nil
  ) {
    self.x = x
    self.y = y
    self.width = width
    self.height = height
    self.screenIdentifier = screenIdentifier
  }

  public var isUsable: Bool {
    x.isFinite && y.isFinite && width.isFinite && height.isFinite && width > 0 && height > 0
  }
}
