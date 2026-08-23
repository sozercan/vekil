import Foundation
import XCTest
@testable import VekilCore

private struct PlannedDataSourceError: Error, Sendable, CustomStringConvertible {
    let message: String
    var description: String { message }
}

private struct StatsPlan: Sendable {
    var value: StatsSnapshot?
    var error: String?
    var delayNanoseconds: UInt64 = 0
    var ignoreCancellation: Bool = false
}

private struct ReadinessPlan: Sendable {
    var value: ReadinessResponse?
    var error: String?
    var delayNanoseconds: UInt64 = 0
}

private struct ModelsPlan: Sendable {
    var value: RuntimeModelCatalog?
    var error: String?
    var delayNanoseconds: UInt64 = 0
}

private actor StubAnalyticsDataSource: AnalyticsDataSource {
    private var statsPlans: [StatsPlan] = []
    private var readinessPlans: [ReadinessPlan] = []
    private var modelsPlans: [ModelsPlan] = []
    private(set) var statsCalls = 0
    private(set) var readinessCalls = 0
    private(set) var modelsCalls = 0
    private(set) var statsCancellations = 0

    func enqueueStats(
        _ value: StatsSnapshot,
        delayNanoseconds: UInt64 = 0,
        ignoreCancellation: Bool = false
    ) {
        statsPlans.append(StatsPlan(
            value: value,
            delayNanoseconds: delayNanoseconds,
            ignoreCancellation: ignoreCancellation
        ))
    }

    func enqueueStatsError(_ message: String, delayNanoseconds: UInt64 = 0) {
        statsPlans.append(StatsPlan(value: nil, error: message, delayNanoseconds: delayNanoseconds))
    }

    func enqueueReadiness(_ value: ReadinessResponse, delayNanoseconds: UInt64 = 0) {
        readinessPlans.append(ReadinessPlan(value: value, delayNanoseconds: delayNanoseconds))
    }

    func enqueueModels(_ value: RuntimeModelCatalog, delayNanoseconds: UInt64 = 0) {
        modelsPlans.append(ModelsPlan(value: value, delayNanoseconds: delayNanoseconds))
    }

    func counts() -> (stats: Int, readiness: Int, models: Int) {
        (statsCalls, readinessCalls, modelsCalls)
    }

    func fetchStats() async throws -> StatsSnapshot {
        statsCalls += 1
        let plan = statsPlans.isEmpty ? StatsPlan(value: StatsSnapshot()) : statsPlans.removeFirst()
        if plan.delayNanoseconds > 0 {
            do {
                try await Task.sleep(nanoseconds: plan.delayNanoseconds)
            } catch {
                statsCancellations += 1
                if !plan.ignoreCancellation { throw error }
            }
        }
        if let error = plan.error { throw PlannedDataSourceError(message: error) }
        return plan.value ?? StatsSnapshot()
    }

    func fetchReadiness() async throws -> ReadinessResponse {
        readinessCalls += 1
        let plan = readinessPlans.isEmpty
            ? ReadinessPlan(value: ReadinessResponse(status: "ready", httpStatusCode: 200))
            : readinessPlans.removeFirst()
        if plan.delayNanoseconds > 0 {
            try await Task.sleep(nanoseconds: plan.delayNanoseconds)
        }
        if let error = plan.error { throw PlannedDataSourceError(message: error) }
        return plan.value ?? ReadinessResponse(status: "ready", httpStatusCode: 200)
    }

    func fetchModels() async throws -> RuntimeModelCatalog {
        modelsCalls += 1
        let plan = modelsPlans.isEmpty ? ModelsPlan(value: RuntimeModelCatalog()) : modelsPlans.removeFirst()
        if plan.delayNanoseconds > 0 {
            try await Task.sleep(nanoseconds: plan.delayNanoseconds)
        }
        if let error = plan.error { throw PlannedDataSourceError(message: error) }
        return plan.value ?? RuntimeModelCatalog()
    }
}

private final class LockedTestClock: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Date

    init(_ value: Date) {
        self.value = value
    }

    func now() -> Date {
        lock.lock()
        defer { lock.unlock() }
        return value
    }

    func advance(_ interval: TimeInterval) {
        lock.lock()
        value = value.addingTimeInterval(interval)
        lock.unlock()
    }
}

private func eventually(
    timeout: TimeInterval = 1,
    condition: @escaping () async -> Bool
) async -> Bool {
    let deadline = Date().addingTimeInterval(timeout)
    while Date() < deadline {
        if await condition() { return true }
        try? await Task.sleep(nanoseconds: 5_000_000)
    }
    return await condition()
}

final class StatsStoreTests: XCTestCase {
    func testStatsRefreshIsSingleFlight() async throws {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(
            StatsSnapshot(uptimeSeconds: 10, totals: StatsTotals(requests: 1)),
            delayNanoseconds: 50_000_000
        )
        let store = StatsStore(
            dataSource: source,
            configuration: StatsStoreConfiguration(automaticPolling: false)
        )
        await store.updateRuntime(generation: 1, serviceState: .running)

        async let first = store.refreshStats(trigger: .manual)
        async let second = store.refreshStats(trigger: .manual)
        _ = await (first, second)

        let statsCalls = await source.counts().stats
        XCTAssertEqual(statsCalls, 1)
        let state = await store.state()
        guard case let .current(capture) = state.snapshotState else {
            return XCTFail("expected current snapshot, got \(state.snapshotState)")
        }
        XCTAssertEqual(capture.runtimeGeneration, 1)
        XCTAssertEqual(capture.snapshot.totals.requests, 1)
        await store.shutdown()
    }

    func testPollingFailureMarksOnlyRunningCurrentDataStaleAndStopArchivesIt() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 100, totals: StatsTotals(requests: 4)))
        await source.enqueueStatsError("proxy unavailable")
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 4, serviceState: .running)

        _ = await store.refreshStats()
        _ = await store.refreshStats(trigger: .polling)
        var state = await store.state()
        guard case let .stale(capture, failure) = state.snapshotState else {
            return XCTFail("expected stale state, got \(state.snapshotState)")
        }
        XCTAssertEqual(capture.snapshot.totals.requests, 4)
        XCTAssertEqual(failure.message, "proxy unavailable")

        await store.runtimeDidStop()
        state = await store.state()
        guard case let .previousRun(previous) = state.snapshotState else {
            return XCTFail("expected previous-run state, got \(state.snapshotState)")
        }
        XCTAssertEqual(previous.reason, .serviceStopped)
        XCTAssertEqual(previous.capture.runtimeGeneration, 4)

        _ = await store.refreshStats()
        let stoppedStatsCalls = await source.counts().stats
        XCTAssertEqual(stoppedStatsCalls, 2, "stopped stores must not poll or become stale")
        await store.shutdown()
    }

    func testFailureWithoutCurrentCaptureDoesNotClaimStaleData() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStatsError("not listening")
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 1, serviceState: .running)

        _ = await store.refreshStats()
        let state = await store.state()
        XCTAssertEqual(state.snapshotState, .empty)
        XCTAssertEqual(state.lastStatsFailure?.message, "not listening")
        await store.shutdown()
    }

    func testOldInFlightResponseCannotCrossRuntimeGeneration() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(
            StatsSnapshot(uptimeSeconds: 900, totals: StatsTotals(requests: 99)),
            delayNanoseconds: 80_000_000,
            ignoreCancellation: true
        )
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 5, totals: StatsTotals(requests: 1)))
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 1, serviceState: .running)

        let oldRefresh = Task { await store.refreshStats() }
        try? await Task.sleep(nanoseconds: 10_000_000)
        await store.updateRuntime(generation: 2, serviceState: .running, reason: .helperRestarted)
        _ = await oldRefresh.value
        _ = await store.refreshStats()

        let state = await store.state()
        guard case let .current(capture) = state.snapshotState else {
            return XCTFail("expected generation-2 current data, got \(state.snapshotState)")
        }
        XCTAssertEqual(capture.runtimeGeneration, 2)
        XCTAssertEqual(capture.snapshot.uptimeSeconds, 5)
        XCTAssertEqual(capture.snapshot.totals.requests, 1)
        let generationStatsCalls = await source.counts().stats
        XCTAssertEqual(generationStatsCalls, 2)
        await store.shutdown()
    }

    func testUptimeRegressionStartsNewLocalRunAndRetainsPreviousRun() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 600, totals: StatsTotals(requests: 10)))
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 3, totals: StatsTotals(requests: 1)))
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 8, serviceState: .running)

        _ = await store.refreshStats()
        let first = await store.state()
        let firstOrdinal = first.runOrdinal
        _ = await store.refreshStats()
        let state = await store.state()

        guard case let .current(current) = state.snapshotState else {
            return XCTFail("expected new current run, got \(state.snapshotState)")
        }
        XCTAssertEqual(current.runtimeGeneration, 8)
        XCTAssertEqual(current.snapshot.uptimeSeconds, 3)
        XCTAssertEqual(current.scope.runOrdinal, firstOrdinal + 1)
        XCTAssertEqual(state.previousRun?.reason, .uptimeRegressed)
        XCTAssertEqual(state.previousRun?.capture.snapshot.uptimeSeconds, 600)
        XCTAssertNotEqual(state.previousRun?.capture.scope, current.scope)
        await store.shutdown()
    }

    func testVisibilityStartsImmediateAndPeriodicStatsPollingThenCancelsOnHide() async {
        let source = StubAnalyticsDataSource()
        let store = StatsStore(
            dataSource: source,
            configuration: StatsStoreConfiguration(
                statsPollInterval: 0.02,
                readinessPollInterval: 1,
                automaticPolling: true
            )
        )
        await store.updateRuntime(generation: 1, serviceState: .running)
        await store.setVisibility(.traffic, isVisible: true)

        let polled = await eventually {
            await source.counts().stats >= 2
        }
        XCTAssertTrue(polled, "visibility should fetch immediately and continue polling")

        await store.setVisibility(.traffic, isVisible: false)
        try? await Task.sleep(nanoseconds: 30_000_000)
        let callsAfterCancellation = await source.counts().stats
        try? await Task.sleep(nanoseconds: 70_000_000)
        let callsAfterWait = await source.counts().stats
        XCTAssertEqual(callsAfterWait, callsAfterCancellation)

        await store.setVisibility(.traffic, isVisible: true)
        let restarted = await eventually {
            await source.counts().stats > callsAfterCancellation
        }
        XCTAssertTrue(restarted)
        await store.shutdown()
    }

    func testReadinessChecksStartupManualAndAtMostOncePerIntervalWhileOverviewVisible() async {
        let source = StubAnalyticsDataSource()
        let clock = LockedTestClock(Date(timeIntervalSince1970: 1_000))
        let store = StatsStore(
            dataSource: source,
            configuration: StatsStoreConfiguration(
                statsPollInterval: 5,
                readinessPollInterval: 60,
                automaticPolling: false
            ),
            now: { clock.now() }
        )
        await store.updateRuntime(generation: 1, serviceState: .running)

        let startupReadiness = await eventually { await source.counts().readiness == 1 }
        XCTAssertTrue(startupReadiness)
        await store.setVisibility(.overview, isVisible: true)
        try? await Task.sleep(nanoseconds: 20_000_000)
        var readinessCalls = await source.counts().readiness
        XCTAssertEqual(readinessCalls, 1, "visibility must honor the one-minute readiness gate")

        await store.pollVisibleSurfacesNow()
        readinessCalls = await source.counts().readiness
        XCTAssertEqual(readinessCalls, 1)

        await store.manualRefresh()
        readinessCalls = await source.counts().readiness
        XCTAssertEqual(readinessCalls, 2, "manual refresh forces readiness")

        clock.advance(61)
        await store.pollVisibleSurfacesNow()
        readinessCalls = await source.counts().readiness
        XCTAssertEqual(readinessCalls, 3)
        let state = await store.state()
        guard case let .result(capture) = state.readiness else {
            return XCTFail("expected readiness result, got \(state.readiness)")
        }
        XCTAssertTrue(capture.response.isReady)
        await store.shutdown()
    }

    func testModelsLoadOnlyForProvidersVisibilityOrSuccessfulApplyAndClearOnRestart() async {
        let source = StubAnalyticsDataSource()
        let model = RuntimeModel(id: "gpt-5.4", ownedBy: "azure", supportedEndpoints: ["/responses"])
        await source.enqueueModels(RuntimeModelCatalog(data: [model]))
        await source.enqueueModels(RuntimeModelCatalog(data: [model]))
        await source.enqueueModels(RuntimeModelCatalog(data: [RuntimeModel(id: "gpt-5.5")]))
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 1, serviceState: .running)
        try? await Task.sleep(nanoseconds: 20_000_000)
        var modelCalls = await source.counts().models
        XCTAssertEqual(modelCalls, 0)

        await store.setVisibility(.providers, isVisible: true)
        let initialModelsLoaded = await eventually { await source.counts().models == 1 }
        XCTAssertTrue(initialModelsLoaded)
        await store.configurationApplyDidSucceed()
        modelCalls = await source.counts().models
        XCTAssertEqual(modelCalls, 2)

        await store.configurationDidRestart(generation: 2)
        var state = await store.state()
        XCTAssertEqual(state.models, .idle)
        await store.updateRuntime(generation: 2, serviceState: .running, reason: .configurationRestarted)
        let restartedModelsLoaded = await eventually { await source.counts().models == 3 }
        XCTAssertTrue(restartedModelsLoaded)

        state = await store.state()
        guard case let .current(capture) = state.models else {
            return XCTFail("expected current model catalog, got \(state.models)")
        }
        XCTAssertEqual(capture.runtimeGeneration, 2)
        XCTAssertEqual(capture.catalog.data.first?.id, "gpt-5.5")
        await store.shutdown()
    }

    func testExplicitVisibilityPollHookDoesNothingAfterStop() async {
        let source = StubAnalyticsDataSource()
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 1, serviceState: .running)
        await store.setVisibility(.requests, isVisible: true)
        let initialStatsLoaded = await eventually { await source.counts().stats == 1 }
        XCTAssertTrue(initialStatsLoaded)

        await store.runtimeDidStop()
        await store.pollVisibleSurfacesNow(includeModels: true)
        let finalStatsCalls = await source.counts().stats
        XCTAssertEqual(finalStatsCalls, 1)
        let state = await store.state()
        XCTAssertEqual(state.snapshotState.presentationLabel, "Previous proxy run")
        await store.shutdown()
    }

    func testGenerationChangeWhileRunningArchivesAndImmediatelyRefreshesVisibleStats() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 100, totals: StatsTotals(requests: 5)))
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 1, totals: StatsTotals(requests: 1)))
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 1, serviceState: .running)
        await store.setVisibility(.traffic, isVisible: true)
        let firstLoaded = await eventually { await source.counts().stats == 1 }
        XCTAssertTrue(firstLoaded)

        await store.updateRuntime(generation: 2, serviceState: .running, reason: .helperRestarted)
        let secondLoaded = await eventually { await source.counts().stats == 2 }
        XCTAssertTrue(secondLoaded)

        let state = await store.state()
        guard case let .current(current) = state.snapshotState else {
            return XCTFail("expected generation-2 current data, got \(state.snapshotState)")
        }
        XCTAssertEqual(current.runtimeGeneration, 2)
        XCTAssertEqual(current.snapshot.totals.requests, 1)
        XCTAssertEqual(state.previousRun?.capture.runtimeGeneration, 1)
        XCTAssertEqual(state.previousRun?.reason, .helperRestarted)
        await store.shutdown()
    }

    func testSameGenerationStopStartStillCreatesDistinctRunScope() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 50, totals: StatsTotals(requests: 2)))
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 1, totals: StatsTotals(requests: 1)))
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 9, serviceState: .running)
        _ = await store.refreshStats()
        let oldOrdinal = await store.state().runOrdinal

        await store.updateRuntime(generation: 9, serviceState: .starting, reason: .configurationRestarted)
        await store.updateRuntime(generation: 9, serviceState: .running, reason: .configurationRestarted)
        _ = await store.refreshStats()

        let state = await store.state()
        guard case let .current(current) = state.snapshotState else {
            return XCTFail("expected restarted current data, got \(state.snapshotState)")
        }
        XCTAssertEqual(current.scope.runOrdinal, oldOrdinal + 1)
        XCTAssertEqual(state.previousRun?.reason, .configurationRestarted)
        XCTAssertNotEqual(state.previousRun?.capture.scope, current.scope)
        await store.shutdown()
    }

    func testReadinessAndModelRefreshesAreAlsoSingleFlight() async {
        let source = StubAnalyticsDataSource()
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        await store.updateRuntime(generation: 1, serviceState: .running)
        let startupReady = await eventually { await source.counts().readiness == 1 }
        XCTAssertTrue(startupReady)

        await source.enqueueReadiness(
            ReadinessResponse(status: "ready", httpStatusCode: 200),
            delayNanoseconds: 40_000_000
        )
        async let firstReady = store.refreshReadiness(force: true)
        async let secondReady = store.refreshReadiness(force: true)
        _ = await (firstReady, secondReady)
        let readinessCalls = await source.counts().readiness
        XCTAssertEqual(readinessCalls, 2)

        await source.enqueueModels(
            RuntimeModelCatalog(data: [RuntimeModel(id: "single-flight")]),
            delayNanoseconds: 40_000_000
        )
        async let firstModels = store.refreshModels()
        async let secondModels = store.refreshModels()
        _ = await (firstModels, secondModels)
        let modelCalls = await source.counts().models
        XCTAssertEqual(modelCalls, 1)
        await store.shutdown()
    }
}

extension StatsStoreTests {
    func testInFlightStatsFromPriorLaunchAreDiscardedWhenEpochAndGenerationRepeat() async {
        let source = StubAnalyticsDataSource()
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 10, totals: StatsTotals(requests: 99)), delayNanoseconds: 80_000_000, ignoreCancellation: true)
        await source.enqueueStats(StatsSnapshot(uptimeSeconds: 1, totals: StatsTotals(requests: 1)))
        let store = StatsStore(dataSource: source, configuration: .init(automaticPolling: false))
        let old = AnalyticsRuntimeIdentity(launchIdentity: .init(launchToken: UUID(uuidString: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")!, helperEpoch: "hep_repeat"), runtimeGeneration: 7)
        let new = AnalyticsRuntimeIdentity(launchIdentity: .init(launchToken: UUID(uuidString: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")!, helperEpoch: "hep_repeat"), runtimeGeneration: 7)
        await store.updateRuntime(identity: old, serviceState: .running)
        let oldFlight = Task { await store.refreshStats() }
        _ = await eventually { await source.counts().stats == 1 }
        await store.updateRuntime(identity: new, serviceState: .running, reason: .helperRestarted)
        _ = await oldFlight.value
        var state = await store.state()
        XCTAssertNil(state.snapshotState.activeCapture)
        _ = await store.refreshStats()
        state = await store.state()
        XCTAssertEqual(state.snapshotState.activeCapture?.scope.launchToken, new.launchIdentity.launchToken)
        XCTAssertEqual(state.snapshotState.activeCapture?.snapshot.totals.requests, 1)
        await store.shutdown()
    }
}
