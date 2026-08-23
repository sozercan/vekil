import Foundation

public struct AnalyticsRuntimeIdentity: Hashable, Sendable {
    public var launchIdentity: RuntimeLaunchIdentity
    public var runtimeGeneration: UInt64

    public init(launchIdentity: RuntimeLaunchIdentity, runtimeGeneration: UInt64) {
        self.launchIdentity = launchIdentity
        self.runtimeGeneration = runtimeGeneration
    }
}

public enum StatsVisibility: String, CaseIterable, Hashable, Sendable {
    case menu
    case overview
    case traffic
    case requests
    case providers

    public var requiresStatsPolling: Bool {
        switch self {
        case .menu, .overview, .traffic, .requests: return true
        case .providers: return false
        }
    }
}

public enum AnalyticsServiceState: String, Codable, Sendable {
    case stopped
    case starting
    case running
    case stopping
    case failed
}

public enum StatsRefreshTrigger: String, Codable, Sendable {
    case visibility
    case polling
    case manual
    case startup
    case configurationApplied
}

public enum StatsPreviousRunReason: String, Codable, Sendable {
    case serviceStopped
    case serviceRestarted
    case runtimeGenerationChanged
    case helperRestarted
    case configurationRestarted
    case uptimeRegressed
}

public struct StatsRefreshFailure: Error, Equatable, Sendable, CustomStringConvertible {
    public var message: String
    public var occurredAt: Date

    public init(message: String, occurredAt: Date) {
        let trimmed = message.trimmingCharacters(in: .whitespacesAndNewlines)
        self.message = String((trimmed.isEmpty ? "Analytics refresh failed." : trimmed).prefix(512))
        self.occurredAt = occurredAt
    }

    public var description: String { message }
}

public struct StatsPreviousRun: Equatable, Sendable {
    public var capture: StatsCapture
    public var reason: StatsPreviousRunReason
    public var endedAt: Date

    public init(capture: StatsCapture, reason: StatsPreviousRunReason, endedAt: Date) {
        self.capture = capture
        self.reason = reason
        self.endedAt = endedAt
    }
}

public enum StatsSnapshotState: Equatable, Sendable {
    case empty
    case current(StatsCapture)
    case stale(StatsCapture, StatsRefreshFailure)
    case previousRun(StatsPreviousRun)

    public var capture: StatsCapture? {
        switch self {
        case .empty: return nil
        case let .current(capture), let .stale(capture, _): return capture
        case let .previousRun(previous): return previous.capture
        }
    }

    public var activeCapture: StatsCapture? {
        switch self {
        case let .current(capture), let .stale(capture, _): return capture
        case .empty, .previousRun: return nil
        }
    }

    public var isStale: Bool {
        if case .stale = self { return true }
        return false
    }

    public var presentationLabel: String {
        switch self {
        case .empty: return "No data"
        case .current: return "Current proxy run"
        case .stale: return "Stale — current proxy run"
        case .previousRun: return "Previous proxy run"
        }
    }
}

public struct ReadinessCapture: Equatable, Sendable {
    public var runtimeIdentity: AnalyticsRuntimeIdentity
    public var runtimeGeneration: UInt64 { runtimeIdentity.runtimeGeneration }
    public var response: ReadinessResponse
    public var checkedAt: Date

    public init(
        runtimeGeneration: UInt64,
        response: ReadinessResponse,
        checkedAt: Date,
        launchIdentity: RuntimeLaunchIdentity = .zero
    ) {
        self.runtimeIdentity = AnalyticsRuntimeIdentity(launchIdentity: launchIdentity, runtimeGeneration: runtimeGeneration)
        self.response = response
        self.checkedAt = checkedAt
    }
}

public enum ReadinessRefreshState: Equatable, Sendable {
    case idle
    case checking(previous: ReadinessCapture?)
    case result(ReadinessCapture)
    case failed(previous: ReadinessCapture?, StatsRefreshFailure)

    public var capture: ReadinessCapture? {
        switch self {
        case .idle: return nil
        case let .checking(previous), let .failed(previous, _): return previous
        case let .result(capture): return capture
        }
    }
}

public struct ModelCatalogCapture: Equatable, Sendable {
    public var runtimeIdentity: AnalyticsRuntimeIdentity
    public var runtimeGeneration: UInt64 { runtimeIdentity.runtimeGeneration }
    public var catalog: RuntimeModelCatalog
    public var fetchedAt: Date

    public init(
        runtimeGeneration: UInt64,
        catalog: RuntimeModelCatalog,
        fetchedAt: Date,
        launchIdentity: RuntimeLaunchIdentity = .zero
    ) {
        self.runtimeIdentity = AnalyticsRuntimeIdentity(launchIdentity: launchIdentity, runtimeGeneration: runtimeGeneration)
        self.catalog = catalog
        self.fetchedAt = fetchedAt
    }
}

public enum ModelCatalogRefreshState: Equatable, Sendable {
    case idle
    case loading(previous: ModelCatalogCapture?)
    case current(ModelCatalogCapture)
    case failed(previous: ModelCatalogCapture?, StatsRefreshFailure)

    public var capture: ModelCatalogCapture? {
        switch self {
        case .idle: return nil
        case let .loading(previous), let .failed(previous, _): return previous
        case let .current(capture): return capture
        }
    }
}

public struct StatsStoreState: Equatable, Sendable {
    public var runtimeIdentity: AnalyticsRuntimeIdentity?
    public var runtimeGeneration: UInt64?
    public var runOrdinal: UInt64
    public var serviceState: AnalyticsServiceState
    public var snapshotState: StatsSnapshotState
    public var previousRun: StatsPreviousRun?
    public var lastStatsFailure: StatsRefreshFailure?
    public var readiness: ReadinessRefreshState
    public var models: ModelCatalogRefreshState
    public var visibleSurfaces: Set<StatsVisibility>
    public var isRefreshingStats: Bool
    public var isRefreshingReadiness: Bool
    public var isRefreshingModels: Bool

    public init(
        runtimeIdentity: AnalyticsRuntimeIdentity? = nil,
        runtimeGeneration: UInt64? = nil,
        runOrdinal: UInt64 = 0,
        serviceState: AnalyticsServiceState = .stopped,
        snapshotState: StatsSnapshotState = .empty,
        previousRun: StatsPreviousRun? = nil,
        lastStatsFailure: StatsRefreshFailure? = nil,
        readiness: ReadinessRefreshState = .idle,
        models: ModelCatalogRefreshState = .idle,
        visibleSurfaces: Set<StatsVisibility> = [],
        isRefreshingStats: Bool = false,
        isRefreshingReadiness: Bool = false,
        isRefreshingModels: Bool = false
    ) {
        self.runtimeIdentity = runtimeIdentity
        self.runtimeGeneration = runtimeGeneration
        self.runOrdinal = runOrdinal
        self.serviceState = serviceState
        self.snapshotState = snapshotState
        self.previousRun = previousRun
        self.lastStatsFailure = lastStatsFailure
        self.readiness = readiness
        self.models = models
        self.visibleSurfaces = visibleSurfaces
        self.isRefreshingStats = isRefreshingStats
        self.isRefreshingReadiness = isRefreshingReadiness
        self.isRefreshingModels = isRefreshingModels
    }
}

public struct StatsStoreConfiguration: Equatable, Sendable {
    public var statsPollInterval: TimeInterval
    public var readinessPollInterval: TimeInterval
    public var automaticPolling: Bool

    public init(
        statsPollInterval: TimeInterval = 5,
        readinessPollInterval: TimeInterval = 60,
        automaticPolling: Bool = true
    ) {
        self.statsPollInterval = max(0.001, statsPollInterval)
        self.readinessPollInterval = max(0.001, readinessPollInterval)
        self.automaticPolling = automaticPolling
    }
}

/// Shared analytics coordinator. All endpoint refreshes are independently
/// single-flight, and every result is checked against the lifecycle revision,
/// runtime generation, and local run ordinal captured before the request began.
public actor StatsStore {
    private struct FetchContext: Equatable, Sendable {
        var lifecycleRevision: UInt64
        var runtimeIdentity: AnalyticsRuntimeIdentity
        var runtimeGeneration: UInt64 { runtimeIdentity.runtimeGeneration }
        var runOrdinal: UInt64
    }

    private struct FetchFailure: Error, Sendable {
        var message: String
    }

    private struct Flight<Value: Sendable>: Sendable {
        var id: UInt64
        var context: FetchContext
        var trigger: StatsRefreshTrigger
        var task: Task<Result<Value, FetchFailure>, Never>
    }

    private let dataSource: any AnalyticsDataSource
    private let configuration: StatsStoreConfiguration
    private let now: @Sendable () -> Date

    private var runtimeIdentity: AnalyticsRuntimeIdentity?
    private var runtimeGeneration: UInt64?
    private var runOrdinal: UInt64 = 0
    private var lifecycleRevision: UInt64 = 0
    private var serviceState: AnalyticsServiceState = .stopped
    private var snapshotState: StatsSnapshotState = .empty
    private var previousRun: StatsPreviousRun?
    private var lastStatsFailure: StatsRefreshFailure?
    private var readinessState: ReadinessRefreshState = .idle
    private var modelCatalogState: ModelCatalogRefreshState = .idle
    private var visibleSurfaces: Set<StatsVisibility> = []
    private var lastReadinessCheck: Date?
    private var snapshotSequence: UInt64 = 0
    private var nextFlightID: UInt64 = 0
    private var isShutdown = false

    private var statsFlight: Flight<StatsSnapshot>?
    private var readinessFlight: Flight<ReadinessResponse>?
    private var modelsFlight: Flight<RuntimeModelCatalog>?
    private var statsPollingTask: Task<Void, Never>?
    private var readinessPollingTask: Task<Void, Never>?

    public init(
        dataSource: any AnalyticsDataSource,
        configuration: StatsStoreConfiguration = .init(),
        now: @escaping @Sendable () -> Date = { Date() }
    ) {
        self.dataSource = dataSource
        self.configuration = configuration
        self.now = now
    }
}

extension StatsStore {
    public func state() -> StatsStoreState {
        StatsStoreState(
            runtimeIdentity: runtimeIdentity,
            runtimeGeneration: runtimeGeneration,
            runOrdinal: runOrdinal,
            serviceState: serviceState,
            snapshotState: snapshotState,
            previousRun: previousRun,
            lastStatsFailure: lastStatsFailure,
            readiness: readinessState,
            models: modelCatalogState,
            visibleSurfaces: visibleSurfaces,
            isRefreshingStats: statsFlight != nil,
            isRefreshingReadiness: readinessFlight != nil,
            isRefreshingModels: modelsFlight != nil
        )
    }

    public func requests(filter: StatsRequestFilter = .all) -> [StatsProjectedRequest] {
        guard let capture = snapshotState.capture else { return [] }
        return StatsRequestProjector.project(capture: capture, filter: filter)
    }

    public func setVisibility(_ surface: StatsVisibility, isVisible: Bool) {
        guard !isShutdown else { return }
        let changed: Bool
        if isVisible {
            changed = visibleSurfaces.insert(surface).inserted
        } else {
            changed = visibleSurfaces.remove(surface) != nil
        }
        guard changed else { return }

        reconcilePollingTasks()
        if !isVisible {
            cancelFlightsNoLongerNeeded()
            return
        }
        guard serviceState == .running, runtimeGeneration != nil else { return }

        if surface.requiresStatsPolling {
            launchStatsRefresh(trigger: .visibility)
        }
        if surface == .overview {
            launchReadinessRefresh(force: false, trigger: .visibility)
        }
        if surface == .providers {
            launchModelsRefresh(trigger: .visibility)
        }
    }

    public func visibilityDidChange(_ surface: StatsVisibility, visible: Bool) {
        setVisibility(surface, isVisible: visible)
    }

    /// Hook for hosts/tests that own their own timer. Automatic polling uses the
    /// same method, so both paths share single-flight and visibility checks.
    public func pollVisibleSurfacesNow(includeModels: Bool = false) async {
        guard !isShutdown, serviceState == .running else { return }
        if hasVisibleStatsConsumer {
            _ = await refreshStats(trigger: .polling)
        }
        if visibleSurfaces.contains(.overview) {
            _ = await refreshReadiness(force: false, trigger: .polling)
        }
        if includeModels, visibleSurfaces.contains(.providers) {
            _ = await refreshModels(trigger: .polling)
        }
    }

    public func manualRefresh() async {
        guard !isShutdown, serviceState == .running else { return }
        _ = await refreshStats(trigger: .manual)
        _ = await refreshReadiness(force: true, trigger: .manual)
        if visibleSurfaces.contains(.providers) {
            _ = await refreshModels(trigger: .manual)
        }
    }

    public func configurationApplyDidSucceed() async {
        guard !isShutdown, serviceState == .running else { return }
        _ = await refreshModels(trigger: .configurationApplied)
    }

    public func runtimeDidStart(generation: UInt64) {
        updateRuntime(generation: generation, serviceState: .running, reason: .serviceRestarted)
    }

    public func runtimeDidStop(reason: StatsPreviousRunReason = .serviceStopped) {
        updateRuntime(generation: runtimeGeneration, serviceState: .stopped, reason: reason)
    }

    public func helperDidRestart(generation: UInt64?) {
        updateRuntime(generation: generation, serviceState: .starting, reason: .helperRestarted)
    }

    public func configurationDidRestart(generation: UInt64?) {
        updateRuntime(generation: generation, serviceState: .starting, reason: .configurationRestarted)
    }

    public func updateRuntime(
        generation newGeneration: UInt64?,
        serviceState newServiceState: AnalyticsServiceState,
        reason explicitReason: StatsPreviousRunReason? = nil
    ) {
        let identity = newGeneration.map { generation in
            AnalyticsRuntimeIdentity(
                launchIdentity: RuntimeLaunchIdentity(
                    launchToken: RuntimeLaunchIdentity.zero.launchToken,
                    helperEpoch: "legacy"
                ),
                runtimeGeneration: generation
            )
        }
        updateRuntime(identity: identity, serviceState: newServiceState, reason: explicitReason)
    }

    public func updateRuntime(
        identity newIdentity: AnalyticsRuntimeIdentity?,
        serviceState newServiceState: AnalyticsServiceState,
        reason explicitReason: StatsPreviousRunReason? = nil
    ) {
        guard !isShutdown else { return }
        let oldIdentity = runtimeIdentity
        let oldServiceState = serviceState
        let identityChanged = oldIdentity != newIdentity
        let leavingRunning = oldServiceState == .running && newServiceState != .running
        let enteringRunning = oldServiceState != .running && newServiceState == .running
        let anyChange = identityChanged || oldServiceState != newServiceState
        guard anyChange else { return }

        if identityChanged || leavingRunning {
            let reason = explicitReason ?? (identityChanged ? .runtimeGenerationChanged : .serviceStopped)
            archiveActiveSnapshot(reason: reason, at: now())
        }

        let activatedRun = newServiceState == .running && (identityChanged || enteringRunning)
        if activatedRun { runOrdinal &+= 1 }

        runtimeIdentity = newIdentity
        runtimeGeneration = newIdentity?.runtimeGeneration
        serviceState = newServiceState
        invalidateLifecycleFlights()

        if newServiceState != .running {
            readinessState = .idle
            modelCatalogState = .idle
            lastReadinessCheck = nil
        }

        reconcilePollingTasks()

        if activatedRun, newIdentity != nil {
            if hasVisibleStatsConsumer { launchStatsRefresh(trigger: .startup) }
            launchReadinessRefresh(force: true, trigger: .startup)
            if visibleSurfaces.contains(.providers) { launchModelsRefresh(trigger: .startup) }
        }
    }

    @discardableResult
    public func refreshStats(trigger: StatsRefreshTrigger = .manual) async -> StatsSnapshotState {
        guard let context = currentFetchContext else { return snapshotState }
        if let flight = statsFlight {
            let result = await flight.task.value
            return finishStatsFlight(flight, result: result)
        }

        let flight = makeFlight(context: context, trigger: trigger) { source in
            try await source.fetchStats()
        }
        statsFlight = flight
        let result = await flight.task.value
        return finishStatsFlight(flight, result: result)
    }

    @discardableResult
    public func refreshReadiness(
        force: Bool = false,
        trigger: StatsRefreshTrigger = .manual
    ) async -> ReadinessRefreshState {
        guard let context = currentFetchContext else { return readinessState }
        if let flight = readinessFlight {
            let result = await flight.task.value
            return finishReadinessFlight(flight, result: result)
        }

        let checkTime = now()
        if !force, let lastReadinessCheck,
           checkTime.timeIntervalSince(lastReadinessCheck) < configuration.readinessPollInterval {
            return readinessState
        }

        let previous = readinessState.capture
        readinessState = .checking(previous: previous)
        let flight = makeFlight(context: context, trigger: trigger) { source in
            try await source.fetchReadiness()
        }
        readinessFlight = flight
        let result = await flight.task.value
        return finishReadinessFlight(flight, result: result)
    }

    @discardableResult
    public func refreshModels(trigger: StatsRefreshTrigger = .manual) async -> ModelCatalogRefreshState {
        guard let context = currentFetchContext else { return modelCatalogState }
        if let flight = modelsFlight {
            let result = await flight.task.value
            return finishModelsFlight(flight, result: result)
        }

        let previous = modelCatalogState.capture
        modelCatalogState = .loading(previous: previous)
        let flight = makeFlight(context: context, trigger: trigger) { source in
            try await source.fetchModels()
        }
        modelsFlight = flight
        let result = await flight.task.value
        return finishModelsFlight(flight, result: result)
    }

    public func shutdown() {
        guard !isShutdown else { return }
        isShutdown = true
        visibleSurfaces.removeAll()
        statsPollingTask?.cancel()
        readinessPollingTask?.cancel()
        statsPollingTask = nil
        readinessPollingTask = nil
        invalidateLifecycleFlights()
    }
}

private extension StatsStore {
    private var currentFetchContext: FetchContext? {
        guard !isShutdown, serviceState == .running, let runtimeIdentity else { return nil }
        return FetchContext(
            lifecycleRevision: lifecycleRevision,
            runtimeIdentity: runtimeIdentity,
            runOrdinal: runOrdinal
        )
    }

    private var hasVisibleStatsConsumer: Bool {
        visibleSurfaces.contains(where: \.requiresStatsPolling)
    }

    private func makeFlight<Value: Sendable>(
        context: FetchContext,
        trigger: StatsRefreshTrigger,
        operation: @escaping @Sendable (any AnalyticsDataSource) async throws -> Value
    ) -> Flight<Value> {
        nextFlightID &+= 1
        let id = nextFlightID
        let source = dataSource
        let task = Task.detached(priority: nil) { () -> Result<Value, FetchFailure> in
            do {
                return .success(try await operation(source))
            } catch {
                return .failure(FetchFailure(message: String(describing: error)))
            }
        }
        return Flight(id: id, context: context, trigger: trigger, task: task)
    }

    private func finishStatsFlight(
        _ flight: Flight<StatsSnapshot>,
        result: Result<StatsSnapshot, FetchFailure>
    ) -> StatsSnapshotState {
        guard statsFlight?.id == flight.id else { return snapshotState }
        statsFlight = nil
        guard currentFetchContext == flight.context else { return snapshotState }

        switch result {
        case let .success(snapshot):
            accept(snapshot, context: flight.context)
        case let .failure(error):
            let failure = StatsRefreshFailure(message: error.message, occurredAt: now())
            lastStatsFailure = failure
            if let capture = snapshotState.activeCapture,
               capture.scope.launchToken == flight.context.runtimeIdentity.launchIdentity.launchToken,
               capture.scope.helperEpoch == flight.context.runtimeIdentity.launchIdentity.helperEpoch,
               capture.scope.runtimeGeneration == flight.context.runtimeGeneration,
               capture.scope.runOrdinal == flight.context.runOrdinal {
                snapshotState = .stale(capture, failure)
            }
        }
        return snapshotState
    }

    private func accept(_ snapshot: StatsSnapshot, context: FetchContext) {
        if let active = snapshotState.activeCapture,
           active.scope.launchToken == context.runtimeIdentity.launchIdentity.launchToken,
           active.scope.helperEpoch == context.runtimeIdentity.launchIdentity.helperEpoch,
           active.scope.runtimeGeneration == context.runtimeGeneration,
           active.scope.runOrdinal == context.runOrdinal,
           snapshot.uptimeSeconds < active.snapshot.uptimeSeconds {
            archiveActiveSnapshot(reason: .uptimeRegressed, at: now())
            runOrdinal &+= 1
            lifecycleRevision &+= 1
            readinessFlight?.task.cancel()
            modelsFlight?.task.cancel()
            readinessFlight = nil
            modelsFlight = nil
            readinessState = .idle
            modelCatalogState = .idle
            lastReadinessCheck = nil
            launchReadinessRefresh(force: true, trigger: .startup)
        }

        guard runtimeIdentity == context.runtimeIdentity else { return }
        snapshotSequence &+= 1
        let capture = StatsCapture(
            scope: StatsSnapshotScope(
                launchToken: context.runtimeIdentity.launchIdentity.launchToken,
                helperEpoch: context.runtimeIdentity.launchIdentity.helperEpoch,
                runtimeGeneration: context.runtimeGeneration,
                runOrdinal: runOrdinal,
                snapshotSequence: snapshotSequence
            ),
            snapshot: snapshot,
            capturedAt: now()
        )
        snapshotState = .current(capture)
        lastStatsFailure = nil
    }

    private func finishReadinessFlight(
        _ flight: Flight<ReadinessResponse>,
        result: Result<ReadinessResponse, FetchFailure>
    ) -> ReadinessRefreshState {
        guard readinessFlight?.id == flight.id else { return readinessState }
        readinessFlight = nil
        guard currentFetchContext == flight.context else { return readinessState }

        let checkedAt = now()
        lastReadinessCheck = checkedAt
        let previous = readinessState.capture
        switch result {
        case let .success(response):
            readinessState = .result(ReadinessCapture(
                runtimeGeneration: flight.context.runtimeGeneration,
                response: response,
                checkedAt: checkedAt,
                launchIdentity: flight.context.runtimeIdentity.launchIdentity
            ))
        case let .failure(error):
            readinessState = .failed(
                previous: previous,
                StatsRefreshFailure(message: error.message, occurredAt: checkedAt)
            )
        }
        return readinessState
    }

    private func finishModelsFlight(
        _ flight: Flight<RuntimeModelCatalog>,
        result: Result<RuntimeModelCatalog, FetchFailure>
    ) -> ModelCatalogRefreshState {
        guard modelsFlight?.id == flight.id else { return modelCatalogState }
        modelsFlight = nil
        guard currentFetchContext == flight.context else { return modelCatalogState }

        let fetchedAt = now()
        let previous = modelCatalogState.capture
        switch result {
        case let .success(catalog):
            modelCatalogState = .current(ModelCatalogCapture(
                runtimeGeneration: flight.context.runtimeGeneration,
                catalog: catalog,
                fetchedAt: fetchedAt,
                launchIdentity: flight.context.runtimeIdentity.launchIdentity
            ))
        case let .failure(error):
            modelCatalogState = .failed(
                previous: previous,
                StatsRefreshFailure(message: error.message, occurredAt: fetchedAt)
            )
        }
        return modelCatalogState
    }

    private func archiveActiveSnapshot(reason: StatsPreviousRunReason, at date: Date) {
        guard let capture = snapshotState.activeCapture else { return }
        let previous = StatsPreviousRun(capture: capture, reason: reason, endedAt: date)
        previousRun = previous
        snapshotState = .previousRun(previous)
        lastStatsFailure = nil
    }

    private func invalidateLifecycleFlights() {
        lifecycleRevision &+= 1
        statsFlight?.task.cancel()
        readinessFlight?.task.cancel()
        modelsFlight?.task.cancel()
        statsFlight = nil
        readinessFlight = nil
        modelsFlight = nil
    }

    private func cancelFlightsNoLongerNeeded() {
        if !hasVisibleStatsConsumer, let flight = statsFlight,
           flight.trigger == .visibility || flight.trigger == .polling {
            flight.task.cancel()
            statsFlight = nil
        }
        if !visibleSurfaces.contains(.overview), let flight = readinessFlight,
           flight.trigger == .visibility || flight.trigger == .polling {
            flight.task.cancel()
            readinessFlight = nil
            readinessState = readinessState.capture.map(ReadinessRefreshState.result) ?? .idle
        }
        if !visibleSurfaces.contains(.providers), let flight = modelsFlight,
           flight.trigger == .visibility || flight.trigger == .polling {
            flight.task.cancel()
            modelsFlight = nil
            modelCatalogState = modelCatalogState.capture.map(ModelCatalogRefreshState.current) ?? .idle
        }
    }

    private func launchStatsRefresh(trigger: StatsRefreshTrigger) {
        Task { [weak self] in
            _ = await self?.refreshStats(trigger: trigger)
        }
    }

    private func launchReadinessRefresh(force: Bool, trigger: StatsRefreshTrigger) {
        Task { [weak self] in
            _ = await self?.refreshReadiness(force: force, trigger: trigger)
        }
    }

    private func launchModelsRefresh(trigger: StatsRefreshTrigger) {
        Task { [weak self] in
            _ = await self?.refreshModels(trigger: trigger)
        }
    }

    private func reconcilePollingTasks() {
        guard configuration.automaticPolling, !isShutdown, serviceState == .running else {
            statsPollingTask?.cancel()
            readinessPollingTask?.cancel()
            statsPollingTask = nil
            readinessPollingTask = nil
            return
        }

        if hasVisibleStatsConsumer {
            if statsPollingTask == nil {
                let interval = configuration.statsPollInterval
                statsPollingTask = Task { [weak self] in
                    await self?.runStatsPollingLoop(interval: interval)
                }
            }
        } else {
            statsPollingTask?.cancel()
            statsPollingTask = nil
        }

        if visibleSurfaces.contains(.overview) {
            if readinessPollingTask == nil {
                let interval = configuration.readinessPollInterval
                readinessPollingTask = Task { [weak self] in
                    await self?.runReadinessPollingLoop(interval: interval)
                }
            }
        } else {
            readinessPollingTask?.cancel()
            readinessPollingTask = nil
        }
    }

    private func runStatsPollingLoop(interval: TimeInterval) async {
        let nanoseconds = Self.nanoseconds(for: interval)
        while !Task.isCancelled {
            do {
                try await Task.sleep(nanoseconds: nanoseconds)
            } catch {
                return
            }
            guard hasVisibleStatsConsumer, serviceState == .running else { return }
            _ = await refreshStats(trigger: .polling)
        }
    }

    private func runReadinessPollingLoop(interval: TimeInterval) async {
        let nanoseconds = Self.nanoseconds(for: interval)
        while !Task.isCancelled {
            do {
                try await Task.sleep(nanoseconds: nanoseconds)
            } catch {
                return
            }
            guard visibleSurfaces.contains(.overview), serviceState == .running else { return }
            _ = await refreshReadiness(force: false, trigger: .polling)
        }
    }

    private static func nanoseconds(for interval: TimeInterval) -> UInt64 {
        SystemRuntimeClock.nanoseconds(for: max(0.001, interval))
    }
}
