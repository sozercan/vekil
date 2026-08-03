import Foundation

@MainActor
public protocol VekilPreferencesStore: AnyObject {
  var selectedDestination: VekilDestination { get set }
  var mainWindowFrame: VekilWindowFrame? { get set }
  var openAtLogin: Bool { get set }
  var startProxyWhenAppLaunches: Bool { get set }
}

@MainActor
public final class UserDefaultsVekilPreferencesStore: VekilPreferencesStore {
  public struct Keys: Equatable, Sendable {
    public var selectedDestination: String
    public var mainWindowFrame: String
    public var openAtLogin: String
    public var startProxyWhenAppLaunches: String

    public init(prefix: String = "com.vekil.menubar.native") {
      selectedDestination = "\(prefix).selected-destination"
      mainWindowFrame = "\(prefix).main-window-frame"
      openAtLogin = "\(prefix).open-at-login"
      startProxyWhenAppLaunches = "\(prefix).start-proxy-when-app-launches"
    }
  }

  private let defaults: UserDefaults
  private let keys: Keys
  private let encoder = JSONEncoder()
  private let decoder = JSONDecoder()

  public init(defaults: UserDefaults = .standard, keys: Keys = Keys()) {
    self.defaults = defaults
    self.keys = keys
  }

  public var selectedDestination: VekilDestination {
    get {
      guard
        let rawValue = defaults.string(forKey: keys.selectedDestination),
        let destination = VekilDestination(rawValue: rawValue)
      else {
        return .overview
      }
      return destination
    }
    set {
      defaults.set(newValue.rawValue, forKey: keys.selectedDestination)
    }
  }

  public var mainWindowFrame: VekilWindowFrame? {
    get {
      guard
        let data = defaults.data(forKey: keys.mainWindowFrame),
        let frame = try? decoder.decode(VekilWindowFrame.self, from: data),
        frame.isUsable
      else {
        return nil
      }
      return frame
    }
    set {
      guard let newValue, newValue.isUsable, let data = try? encoder.encode(newValue) else {
        defaults.removeObject(forKey: keys.mainWindowFrame)
        return
      }
      defaults.set(data, forKey: keys.mainWindowFrame)
    }
  }

  /// UserDefaults returns false for an absent key, which is the required new
  /// user default. This intent remains separate from launch-time proxy start.
  public var openAtLogin: Bool {
    get { defaults.bool(forKey: keys.openAtLogin) }
    set { defaults.set(newValue, forKey: keys.openAtLogin) }
  }

  /// UserDefaults returns false for an absent key, which is the required new
  /// user default. Enabling Open at Login never changes this value.
  public var startProxyWhenAppLaunches: Bool {
    get { defaults.bool(forKey: keys.startProxyWhenAppLaunches) }
    set { defaults.set(newValue, forKey: keys.startProxyWhenAppLaunches) }
  }
}

@MainActor
public final class InMemoryVekilPreferencesStore: VekilPreferencesStore {
  public var selectedDestination: VekilDestination
  public var mainWindowFrame: VekilWindowFrame?
  public var openAtLogin: Bool
  public var startProxyWhenAppLaunches: Bool

  public init(
    selectedDestination: VekilDestination = .overview,
    mainWindowFrame: VekilWindowFrame? = nil,
    openAtLogin: Bool = false,
    startProxyWhenAppLaunches: Bool = false
  ) {
    self.selectedDestination = selectedDestination
    self.mainWindowFrame = mainWindowFrame
    self.openAtLogin = openAtLogin
    self.startProxyWhenAppLaunches = startProxyWhenAppLaunches
  }
}
