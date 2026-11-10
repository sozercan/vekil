import XCTest

@testable import VekilCore

final class VekilPresentationTests: XCTestCase {
  func testReadyAndDegradedAreDerivedFromServiceAndReadinessAxes() {
    var state = AppRuntimeStateSnapshot.connectedStopped
    state.service = .running
    state.readiness = .ready

    XCTAssertEqual(
      VekilPresentationProjector.presentation(for: state, initialization: .initialized).kind,
      .ready
    )
    XCTAssertEqual(
      VekilPresentationProjector.primaryAction(
        for: state,
        initialization: .initialized,
        isSubmittingCommand: false,
        cancellationRequestedOperationID: nil
      ),
      VekilPrimaryAction(kind: .stopProxy, title: "Stop Proxy", isEnabled: true)
    )

    state.readiness = .stale
    XCTAssertEqual(
      VekilPresentationProjector.presentation(for: state, initialization: .initialized).kind,
      .degraded
    )
  }

  func testAuthenticationRequiredDependsOnConfigurationAsWellAsAuthAxis() {
    var state = AppRuntimeStateSnapshot.connectedStopped
    state.authentication = AppRuntimeAuthentication(state: .signedOut, source: .none)
    state.configuration.requiresGitHubAuthentication = true

    XCTAssertEqual(
      VekilPresentationProjector.presentation(for: state, initialization: .initialized).kind,
      .authenticationRequired
    )

    state.configuration.requiresGitHubAuthentication = false
    XCTAssertEqual(
      VekilPresentationProjector.presentation(for: state, initialization: .initialized).kind,
      .stopped
    )
  }

  func testStartingPrimaryActionTargetsTheProtocolOperation() {
    var state = AppRuntimeStateSnapshot.connectedStopped
    state.service = .starting
    state.operation = AppRuntimeOperation(
      id: "op_start",
      kind: .start,
      phase: .dynamicProviderModelValidation
    )

    let presentation = VekilPresentationProjector.presentation(
      for: state, initialization: .initialized)
    XCTAssertEqual(presentation.kind, .starting)
    XCTAssertEqual(presentation.detail, "Validating provider models")

    XCTAssertEqual(
      VekilPresentationProjector.primaryAction(
        for: state,
        initialization: .initialized,
        isSubmittingCommand: false,
        cancellationRequestedOperationID: nil
      ),
      VekilPrimaryAction(
        kind: .cancelStarting,
        title: "Cancel Starting",
        isEnabled: true,
        operationID: "op_start"
      )
    )
  }

  func testCancelAcknowledgementAndStoppingAreDisabledPrimaryActions() {
    var state = AppRuntimeStateSnapshot.connectedStopped
    state.service = .starting
    state.operation = AppRuntimeOperation(id: "op_start", kind: .start)

    XCTAssertEqual(
      VekilPresentationProjector.primaryAction(
        for: state,
        initialization: .initialized,
        isSubmittingCommand: false,
        cancellationRequestedOperationID: "op_start"
      ),
      VekilPrimaryAction(kind: .none, title: "Stopping…", isEnabled: false)
    )

    state.service = .stopping
    state.operation = AppRuntimeOperation(id: "op_stop", kind: .stop)
    XCTAssertEqual(
      VekilPresentationProjector.primaryAction(
        for: state,
        initialization: .initialized,
        isSubmittingCommand: false,
        cancellationRequestedOperationID: nil
      ).title,
      "Stopping…"
    )
  }

  func testHelperFailureDoesNotMasqueradeAsAStoppedProxy() {
    var state = AppRuntimeStateSnapshot.connectedStopped
    state.helper = .failed

    let presentation = VekilPresentationProjector.presentation(
      for: state, initialization: .initialized)
    XCTAssertEqual(presentation.kind, .helperUnavailable)
    XCTAssertTrue(presentation.isWarning)
    XCTAssertEqual(
      VekilPresentationProjector.primaryAction(
        for: state,
        initialization: .initialized,
        isSubmittingCommand: false,
        cancellationRequestedOperationID: nil
      ),
      VekilPrimaryAction(kind: .restartHelper, title: "Restart Runtime", isEnabled: true)
    )
  }

  func testUnknownOperationPhaseRemainsPresentable() {
    var state = AppRuntimeStateSnapshot.connectedStopped
    state.service = .starting
    state.operation = AppRuntimeOperation(
      id: "op_future",
      kind: .start,
      phase: AppRuntimeOperationPhase(rawValue: "future_phase")
    )

    XCTAssertEqual(
      VekilPresentationProjector.presentation(for: state, initialization: .initialized).detail,
      "Future Phase"
    )
  }
}
