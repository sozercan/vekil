import XCTest
import VekilCore
@testable import VekilUI

final class OnboardingReadinessTests: XCTestCase {
  func testExternalConfigurationMustHaveNoDrift() {
    let configuration = AppRuntimeConfigurationState(
      mode: .external,
      selectedExternalPath: "/tmp/providers.yaml",
      selectedRevision: "cfg_selected",
      activeRevision: "cfg_active",
      drift: .changed,
      requiresGitHubAuthentication: false
    )

    XCTAssertFalse(
      onboardingConfigurationIsReady(
        provider: .openAICompatible,
        configuration: configuration
      )
    )
  }

  func testRevisionMismatchBlocksReadinessEvenWithoutReportedDrift() {
    let configuration = AppRuntimeConfigurationState(
      mode: .external,
      selectedExternalPath: "/tmp/providers.yaml",
      selectedRevision: "cfg_selected",
      activeRevision: "cfg_active",
      drift: .none,
      requiresGitHubAuthentication: false
    )

    XCTAssertFalse(
      onboardingConfigurationIsReady(
        provider: .openAICompatible,
        configuration: configuration
      )
    )
  }
}
