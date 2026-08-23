import XCTest
import VekilCore
@testable import VekilUI

private struct AnalyticsViewModelDataSource: AnalyticsDataSource {
    func fetchStats() async throws -> StatsSnapshot { StatsSnapshot() }
    func fetchReadiness() async throws -> ReadinessResponse { ReadinessResponse(status: "ready") }
    func fetchModels() async throws -> RuntimeModelCatalog { RuntimeModelCatalog() }
}

final class AnalyticsViewModelTests: XCTestCase {
    @MainActor
    func testRuntimeLifecycleUpdatesFinishInSubmissionOrder() async {
        let store = StatsStore(
            dataSource: AnalyticsViewModelDataSource(),
            configuration: StatsStoreConfiguration(automaticPolling: false)
        )
        let viewModel = AnalyticsViewModel(store: store)
        let firstLaunch = UUID(uuidString: "11111111-1111-1111-1111-111111111111")!
        let secondLaunch = UUID(uuidString: "22222222-2222-2222-2222-222222222222")!

        viewModel.applyRuntime(AppRuntimeStateSnapshot(
            launchToken: firstLaunch,
            helperEpoch: "helper-1",
            stateRevision: 1,
            runtimeGeneration: 1,
            service: .running
        ))
        viewModel.applyRuntime(AppRuntimeStateSnapshot(
            launchToken: firstLaunch,
            helperEpoch: "helper-1",
            stateRevision: 2,
            runtimeGeneration: 1,
            service: .stopped
        ))
        viewModel.applyRuntime(AppRuntimeStateSnapshot(
            launchToken: secondLaunch,
            helperEpoch: "helper-2",
            stateRevision: 3,
            runtimeGeneration: 2,
            service: .running
        ))

        await viewModel.waitForRuntimeUpdates()

        let state = await store.state()
        let expectedIdentity = AnalyticsRuntimeIdentity(
            launchIdentity: RuntimeLaunchIdentity(
                launchToken: secondLaunch,
                helperEpoch: "helper-2"
            ),
            runtimeGeneration: 2
        )
        XCTAssertEqual(state.runtimeIdentity, expectedIdentity)
        XCTAssertEqual(state.serviceState, .running)
        XCTAssertEqual(viewModel.state.runtimeIdentity, expectedIdentity)
        XCTAssertEqual(viewModel.state.serviceState, .running)
    }

    @MainActor
    func testVisibilityUpdatesFinishInSubmissionOrder() async {
        let store = StatsStore(
            dataSource: AnalyticsViewModelDataSource(),
            configuration: StatsStoreConfiguration(automaticPolling: false)
        )
        let viewModel = AnalyticsViewModel(store: store)

        viewModel.setVisible(.overview, true)
        viewModel.setVisible(.overview, false)
        viewModel.setVisible(.providers, true)

        await viewModel.waitForVisibilityUpdates()

        let state = await store.state()
        XCTAssertEqual(state.visibleSurfaces, [.providers])
        XCTAssertEqual(viewModel.state.visibleSurfaces, [.providers])
    }
}
