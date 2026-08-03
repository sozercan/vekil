import Foundation
import XCTest

@testable import VekilCore

@MainActor
final class PreferencesStoreTests: XCTestCase {
  private var suiteName: String!
  private var defaults: UserDefaults!

  override func setUp() {
    super.setUp()
    suiteName = "VekilCoreTests.\(UUID().uuidString)"
    defaults = UserDefaults(suiteName: suiteName)
    defaults.removePersistentDomain(forName: suiteName)
  }

  override func tearDown() {
    defaults.removePersistentDomain(forName: suiteName)
    defaults = nil
    suiteName = nil
    super.tearDown()
  }

  func testNewUserDefaultsAreOverviewAndBothLaunchPreferencesOff() {
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults)

    XCTAssertEqual(store.selectedDestination, .overview)
    XCTAssertNil(store.mainWindowFrame)
    XCTAssertFalse(store.openAtLogin)
    XCTAssertFalse(store.startProxyWhenAppLaunches)
  }

  func testNavigationWindowAndSeparateLaunchPreferencesPersist() {
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults)
    let frame = VekilWindowFrame(x: 10, y: 20, width: 900, height: 700, screenIdentifier: "main")

    store.selectedDestination = .providers
    store.mainWindowFrame = frame
    store.openAtLogin = true
    store.startProxyWhenAppLaunches = false

    let reloaded = UserDefaultsVekilPreferencesStore(defaults: defaults)
    XCTAssertEqual(reloaded.selectedDestination, .providers)
    XCTAssertEqual(reloaded.mainWindowFrame, frame)
    XCTAssertTrue(reloaded.openAtLogin)
    XCTAssertFalse(reloaded.startProxyWhenAppLaunches)

    reloaded.startProxyWhenAppLaunches = true
    XCTAssertTrue(reloaded.openAtLogin, "Start on Launch must not overwrite Open at Login")
  }

  func testInvalidWindowFrameIsNotPersisted() {
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults)
    store.mainWindowFrame = VekilWindowFrame(x: 0, y: 0, width: 0, height: 100)
    XCTAssertNil(store.mainWindowFrame)
  }
}
