import Foundation
@testable import VekilCore

final class FakeRuntimeProcess: RuntimeProcess, @unchecked Sendable {
    let standardOutput: AsyncStream<Data>
    let standardError: AsyncStream<Data>
    let termination: AsyncStream<RuntimeProcessTermination>

    private let outputContinuation: AsyncStream<Data>.Continuation
    private let errorContinuation: AsyncStream<Data>.Continuation
    private let terminationContinuation: AsyncStream<RuntimeProcessTermination>.Continuation
    private let lock = NSLock()
    private var writes: [Data] = []
    private var hasExited = false
    private var runError: Error?
    private var writeError: Error?
    private var runCallback: (@Sendable () -> Void)?
    private var writeCallback: (@Sendable (Data) -> Void)?
    private var _runCount = 0
    private var _closeInputCount = 0
    private var _terminateCount = 0
    private var _forceTerminateCount = 0

    init() {
        var outputContinuation: AsyncStream<Data>.Continuation!
        standardOutput = AsyncStream<Data> { outputContinuation = $0 }
        self.outputContinuation = outputContinuation

        var errorContinuation: AsyncStream<Data>.Continuation!
        standardError = AsyncStream<Data> { errorContinuation = $0 }
        self.errorContinuation = errorContinuation

        var terminationContinuation: AsyncStream<RuntimeProcessTermination>.Continuation!
        termination = AsyncStream<RuntimeProcessTermination> { terminationContinuation = $0 }
        self.terminationContinuation = terminationContinuation
    }

    func run() throws {
        let callback: (@Sendable () -> Void)?
        let error: Error?
        lock.lock()
        _runCount += 1
        callback = runCallback
        error = runError
        lock.unlock()
        if let error { throw error }
        callback?()
    }

    func writeStandardInput(_ data: Data) async throws {
        let callback: (@Sendable (Data) -> Void)?
        let error: Error?
        lock.lock()
        writes.append(data)
        callback = writeCallback
        error = writeError
        lock.unlock()
        if let error { throw error }
        callback?(data)
    }

    func closeStandardInput() {
        lock.lock()
        _closeInputCount += 1
        lock.unlock()
    }

    func terminate() {
        lock.lock()
        _terminateCount += 1
        lock.unlock()
    }

    func forceTerminate() {
        lock.lock()
        _forceTerminateCount += 1
        lock.unlock()
    }

    func emitStandardOutput(_ data: Data) {
        outputContinuation.yield(data)
    }

    func emitStandardError(_ data: Data) {
        errorContinuation.yield(data)
    }

    func emitExit(status: Int32 = 1, reason: RuntimeProcessTerminationReason = .exit) {
        lock.lock()
        guard !hasExited else {
            lock.unlock()
            return
        }
        hasExited = true
        lock.unlock()

        outputContinuation.finish()
        errorContinuation.finish()
        terminationContinuation.yield(RuntimeProcessTermination(status: status, reason: reason))
        terminationContinuation.finish()
    }

    func emitTerminationBeforeStandardOutputCloses(
        status: Int32 = 1,
        reason: RuntimeProcessTerminationReason = .exit
    ) {
        lock.lock()
        guard !hasExited else {
            lock.unlock()
            return
        }
        hasExited = true
        lock.unlock()

        errorContinuation.finish()
        terminationContinuation.yield(RuntimeProcessTermination(status: status, reason: reason))
        terminationContinuation.finish()
    }

    func finishStandardOutput() {
        outputContinuation.finish()
    }

    func setRunError(_ error: Error?) {
        lock.lock()
        runError = error
        lock.unlock()
    }

    func setWriteError(_ error: Error?) {
        lock.lock()
        writeError = error
        lock.unlock()
    }

    func onRun(_ callback: (@Sendable () -> Void)?) {
        lock.lock()
        runCallback = callback
        lock.unlock()
    }

    func onWrite(_ callback: (@Sendable (Data) -> Void)?) {
        lock.lock()
        writeCallback = callback
        lock.unlock()
    }

    var writtenData: [Data] {
        lock.lock()
        defer { lock.unlock() }
        return writes
    }

    var runCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _runCount
    }

    var closeInputCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _closeInputCount
    }

    var terminateCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _terminateCount
    }

    var forceTerminateCount: Int {
        lock.lock()
        defer { lock.unlock() }
        return _forceTerminateCount
    }
}

final class FakeRuntimeProcessFactory: RuntimeProcessFactory, @unchecked Sendable {
    enum FactoryError: Error { case exhausted }

    private let lock = NSLock()
    private var processes: [FakeRuntimeProcess]
    private var configurations: [RuntimeProcessConfiguration] = []

    init(_ processes: [FakeRuntimeProcess]) {
        self.processes = processes
    }

    func makeProcess(configuration: RuntimeProcessConfiguration) throws -> any RuntimeProcess {
        lock.lock()
        defer { lock.unlock() }
        configurations.append(configuration)
        guard !processes.isEmpty else { throw FactoryError.exhausted }
        return processes.removeFirst()
    }

    var receivedConfigurations: [RuntimeProcessConfiguration] {
        lock.lock()
        defer { lock.unlock() }
        return configurations
    }
}

final class SequenceRuntimeIDGenerator: RuntimeIDGenerator, @unchecked Sendable {
    private let lock = NSLock()
    private var values: [String]
    private var fallback = 0

    init(_ values: [String]) {
        self.values = values
    }

    func nextRequestID() -> String {
        lock.lock()
        defer { lock.unlock() }
        if !values.isEmpty { return values.removeFirst() }
        fallback += 1
        return "req_fallback_\(fallback)"
    }
}

final class RecordingImmediateClock: RuntimeClock, @unchecked Sendable {
    private let lock = NSLock()
    private var current: Date
    private var sleeps: [TimeInterval] = []

    init(now: Date = Date(timeIntervalSince1970: 1_000)) {
        current = now
    }

    func now() -> Date {
        lock.lock()
        defer { lock.unlock() }
        return current
    }

    func sleep(for interval: TimeInterval) async throws {
        try Task.checkCancellation()
        recordSleep(interval)
        await Task.yield()
        try Task.checkCancellation()
    }

    private func recordSleep(_ interval: TimeInterval) {
        lock.lock()
        sleeps.append(interval)
        current = current.addingTimeInterval(interval)
        lock.unlock()
    }

    var recordedSleeps: [TimeInterval] {
        lock.lock()
        defer { lock.unlock() }
        return sleeps
    }
}

final class ManualRuntimeClock: RuntimeClock, @unchecked Sendable {
    private struct Waiter {
        let deadline: Date
        let continuation: CheckedContinuation<Void, Error>
    }

    private let lock = NSLock()
    private var current: Date
    private var waiters: [UUID: Waiter] = [:]
    private var sleeps: [TimeInterval] = []

    init(now: Date = Date(timeIntervalSince1970: 1_000)) {
        current = now
    }

    func now() -> Date {
        lock.lock()
        defer { lock.unlock() }
        return current
    }

    func sleep(for interval: TimeInterval) async throws {
        guard interval > 0 else {
            try Task.checkCancellation()
            return
        }
        let id = UUID()
        try Task.checkCancellation()
        try await withCheckedThrowingContinuation { continuation in
            addWaiter(id: id, interval: interval, continuation: continuation)
        }
        try Task.checkCancellation()
    }

    func advance(by interval: TimeInterval) {
        let ready: [Waiter]
        lock.lock()
        current = current.addingTimeInterval(interval)
        let readyIDs = waiters.compactMap { key, waiter in
            waiter.deadline <= current ? key : nil
        }
        ready = readyIDs.compactMap { waiters.removeValue(forKey: $0) }
        lock.unlock()
        for waiter in ready { waiter.continuation.resume() }
    }

    var recordedSleeps: [TimeInterval] {
        lock.lock()
        defer { lock.unlock() }
        return sleeps
    }

    private func addWaiter(
        id: UUID,
        interval: TimeInterval,
        continuation: CheckedContinuation<Void, Error>
    ) {
        lock.lock()
        sleeps.append(interval)
        waiters[id] = Waiter(
            deadline: current.addingTimeInterval(interval),
            continuation: continuation
        )
        lock.unlock()
    }
}

enum RuntimeTestError: Error {
    case timedOut
    case synthetic
}

func eventually(
    timeout: TimeInterval = 2,
    pollIntervalNanoseconds: UInt64 = 2_000_000,
    _ condition: @escaping () async -> Bool
) async throws {
    let deadline = Date().addingTimeInterval(timeout)
    while Date() < deadline {
        if await condition() { return }
        try await Task.sleep(nanoseconds: pollIntervalNanoseconds)
    }
    throw RuntimeTestError.timedOut
}

func requestFromLine(_ line: Data) throws -> RuntimeRequestEnvelope {
    var frame = line
    if frame.last == 0x0A { frame.removeLast() }
    guard case let .request(request) = try RuntimeFrameCodec().decode(frame) else {
        throw RuntimeTestError.synthetic
    }
    return request
}

func encodedHello(
    epoch: String = "hep_test",
    buildID: String = "bundle_test",
    protocolMin: Int = 1,
    protocolMax: Int = 1,
    envelopeVersion: Int = 1
) throws -> Data {
    let hello = RuntimeHelloPayload(
        protocolMin: protocolMin,
        protocolMax: protocolMax,
        helperBuild: "helper-test",
        bundleBuildID: buildID,
        pid: 4242,
        helperEpoch: epoch
    )
    return try RuntimeFrameCodec().encodeLine(
        RuntimeEventEnvelope(
            version: envelopeVersion,
            event: .hello,
            payload: try JSONValue.encode(hello)
        )
    )
}

func encodedResponse(
    for request: RuntimeRequestEnvelope,
    epoch: String = "hep_test",
    result: JSONValue? = .object([:])
) throws -> Data {
    try RuntimeFrameCodec().encodeLine(
        RuntimeResponseEnvelope(
            version: request.version,
            id: request.id,
            helperEpoch: epoch,
            ok: true,
            result: result
        )
    )
}

func configureSuccessfulHandshake(
    process: FakeRuntimeProcess,
    epoch: String = "hep_test",
    buildID: String = "bundle_test",
    stateRevision: UInt64 = 1,
    runtimeGeneration: UInt64? = nil
) {
    process.onRun { [weak process] in
        guard let process else { return }
        process.emitStandardOutput(try! encodedHello(epoch: epoch, buildID: buildID))
    }
    process.onWrite { [weak process] data in
        guard let process, let request = try? requestFromLine(data) else { return }
        if request.command == .getState {
            let state = RuntimeStatePayload(
                runtimeGeneration: runtimeGeneration,
                configRevision: "cfg_test",
                service: .stopped,
                readiness: .unknown,
                auth: .signedIn
            )
            let result = JSONValue.object([
                "state_revision": .integer(Int64(stateRevision)),
                "state": try! JSONValue.encode(state),
            ])
            process.emitStandardOutput(try! encodedResponse(for: request, epoch: epoch, result: result))
        }
    }
}

func makeConfiguration(
    buildID: String = "bundle_test",
    restartPolicy: RuntimeRestartPolicy = RuntimeRestartPolicy()
) -> RuntimeControllerConfiguration {
    RuntimeControllerConfiguration(
        process: RuntimeProcessConfiguration(executableURL: URL(fileURLWithPath: "/fake/vekil-runtime")),
        expectedBundleBuildID: buildID,
        handshakeTimeout: 0.5,
        requestTimeout: 0.5,
        shutdownGracePeriod: 0.05,
        forceTerminationGracePeriod: 0.05,
        restartPolicy: restartPolicy
    )
}
