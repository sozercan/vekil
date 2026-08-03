import Foundation

public enum VekilDestination: String, CaseIterable, Codable, Equatable, Sendable {
  case overview
  case traffic
  case requests
  case providers
  case settings
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
