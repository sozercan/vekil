import Foundation

/// Supplies a user-selected path without opening or reading the selected file.
/// The AppRuntimeClient/Go helper remains the only component that reads and
/// validates External Configuration bytes.
@MainActor
public protocol ExternalConfigurationPathSelecting: AnyObject {
  func selectExternalConfigurationPath() async throws -> URL?
}

@MainActor
public final class NoExternalConfigurationPathSelector: ExternalConfigurationPathSelecting {
  public init() {}

  public func selectExternalConfigurationPath() async throws -> URL? {
    nil
  }
}

/// AppKit integration can inject an NSOpenPanel-backed closure from the app
/// target without introducing a view or AppKit dependency into state tests.
@MainActor
public final class ClosureExternalConfigurationPathSelector: ExternalConfigurationPathSelecting {
  public typealias Selection = @MainActor () async throws -> URL?

  private let selection: Selection

  public init(selection: @escaping Selection) {
    self.selection = selection
  }

  public func selectExternalConfigurationPath() async throws -> URL? {
    try await selection()
  }
}
