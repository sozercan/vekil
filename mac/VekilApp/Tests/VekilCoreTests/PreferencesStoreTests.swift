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
    XCTAssertNil(store.completedOnboardingVersion)
  }

  func testNavigationWindowAndSeparateLaunchPreferencesPersist() {
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults)
    let frame = VekilWindowFrame(x: 10, y: 20, width: 900, height: 700, screenIdentifier: "main")

    store.selectedDestination = .connection
    store.mainWindowFrame = frame
    store.openAtLogin = true
    store.startProxyWhenAppLaunches = false

    let reloaded = UserDefaultsVekilPreferencesStore(defaults: defaults)
    XCTAssertEqual(reloaded.selectedDestination, .connection)
    XCTAssertEqual(reloaded.mainWindowFrame, frame)
    XCTAssertTrue(reloaded.openAtLogin)
    XCTAssertFalse(reloaded.startProxyWhenAppLaunches)

    reloaded.startProxyWhenAppLaunches = true
    XCTAssertTrue(reloaded.openAtLogin, "Start on Launch must not overwrite Open at Login")
  }

  func testLegacyNavigationDestinationsMigrateToArrangedSections() {
    let keys = UserDefaultsVekilPreferencesStore.Keys()
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults, keys: keys)

    defaults.set("requests", forKey: keys.selectedDestination)
    XCTAssertEqual(store.selectedDestination, .activity)

    defaults.set("models", forKey: keys.selectedDestination)
    XCTAssertEqual(store.selectedDestination, .connection)

    defaults.set("client-setup", forKey: keys.selectedDestination)
    XCTAssertEqual(store.selectedDestination, .clients)
  }

  func testInvalidWindowFrameIsNotPersisted() {
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults)
    store.mainWindowFrame = VekilWindowFrame(x: 0, y: 0, width: 0, height: 100)
    XCTAssertNil(store.mainWindowFrame)
  }

  func testCompletedOnboardingVersionPersistsAndCanReturnToMissing() {
    let store = UserDefaultsVekilPreferencesStore(defaults: defaults)
    XCTAssertNil(store.completedOnboardingVersion)

    store.completedOnboardingVersion = 3
    let reloaded = UserDefaultsVekilPreferencesStore(defaults: defaults)
    XCTAssertEqual(reloaded.completedOnboardingVersion, 3)

    reloaded.completedOnboardingVersion = nil
    XCTAssertNil(UserDefaultsVekilPreferencesStore(defaults: defaults).completedOnboardingVersion)
  }

  func testInMemoryCompletedOnboardingVersionPreservesOptionalState() {
    let missing = InMemoryVekilPreferencesStore()
    XCTAssertNil(missing.completedOnboardingVersion)

    let completed = InMemoryVekilPreferencesStore(completedOnboardingVersion: 2)
    XCTAssertEqual(completed.completedOnboardingVersion, 2)
    completed.completedOnboardingVersion = nil
    XCTAssertNil(completed.completedOnboardingVersion)
  }
}
