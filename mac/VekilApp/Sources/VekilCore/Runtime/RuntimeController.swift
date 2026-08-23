import Foundation

public struct RuntimeRestartPolicy: Sendable, Equatable {
    public var maximumAutomaticRestarts: Int
    public var window: TimeInterval
    public var delays: [TimeInterval]

    public init(
        maximumAutomaticRestarts: Int = 3,
        window: TimeInterval = 60,
        delays: [TimeInterval] = [0.5, 1, 2]
    ) {
        self.maximumAutomaticRestarts = max(0, maximumAutomaticRestarts)
        self.window = max(0, window)
        self.delays = delays.map { max(0, $0) }
    }

    func delay(forAttempt attempt: Int) -> TimeInterval {
        guard attempt > 0, !delays.isEmpty else { return 0 }
        return delays[min(attempt - 1, delays.count - 1)]
    }
}

public struct RuntimeControllerConfiguration: Sendable, Equatable {
    public var process: RuntimeProcessConfiguration
    public var expectedBundleBuildID: String
    public var supportedProtocolVersions: ClosedRange<Int>
    public var maximumFrameSize: Int
    public var diagnosticsByteLimit: Int
    public var handshakeTimeout: TimeInterval
    public var requestTimeout: TimeInterval
    public var shutdownGracePeriod: TimeInterval
    public var forceTerminationGracePeriod: TimeInterval
    public var restartPolicy: RuntimeRestartPolicy

    public init(
        process: RuntimeProcessConfiguration,
        expectedBundleBuildID: String,
        supportedProtocolVersions: ClosedRange<Int> = 1...1,
        maximumFrameSize: Int = JSONLFrameDecoder.protocolMaximumFrameSize,
        diagnosticsByteLimit: Int = 64 * 1_024,
        handshakeTimeout: TimeInterval = 5,
        requestTimeout: TimeInterval = 15,
        shutdownGracePeriod: TimeInterval = 13,
        forceTerminationGracePeriod: TimeInterval = 2,
        restartPolicy: RuntimeRestartPolicy = RuntimeRestartPolicy()
    ) {
        self.process = process
        self.expectedBundleBuildID = expectedBundleBuildID
        self.supportedProtocolVersions = supportedProtocolVersions
        self.maximumFrameSize = maximumFrameSize
        self.diagnosticsByteLimit = max(0, diagnosticsByteLimit)
        self.handshakeTimeout = max(0, handshakeTimeout)
        self.requestTimeout = max(0, requestTimeout)
        self.shutdownGracePeriod = max(0, shutdownGracePeriod)
        self.forceTerminationGracePeriod = max(0, forceTerminationGracePeriod)
        self.restartPolicy = restartPolicy
    }

    public static func bundled(
        expectedBundleBuildID: String,
        bundle: Bundle = .main,
        arguments: [String] = []
    ) -> RuntimeControllerConfiguration {
        RuntimeControllerConfiguration(
            process: RuntimeProcessConfiguration(
                executableURL: RuntimeHelperLocator.bundledHelperURL(in: bundle),
                arguments: arguments
            ),
            expectedBundleBuildID: expectedBundleBuildID
        )
    }
}

public enum RuntimeShutdownReason: Sendable, Equatable {
    case quit
    case updateInstallation
    case manualRestart
    case intentional
}

public enum RuntimeControllerError: Error, Sendable, Equatable, LocalizedError {
    case invalidConfiguration
    case notConnected
    case controllerStopping
    case helloMustBeFirst
    case duplicateHello
    case handshakeTimedOut
    case invalidHello
    case incompatibleProtocol(app: ClosedRange<Int>, helper: ClosedRange<Int>)
    case bundleBuildMismatch(expected: String, actual: String)
    case unexpectedHelperRequest
    case protocolVersionMismatch(expected: Int, actual: Int)
    case protocolViolation
    case processLaunchFailed
    case transportWriteFailed
    case requestEncodingFailed
    case invalidResponsePayload
    case missingResponseResult
    case requestTimedOut(id: String, command: RuntimeCommand)
    case requestCancelled(id: String)
    case duplicateRequestID(String)
    case helperExited(status: Int32, reason: RuntimeProcessTerminationReason)
    case restartLimitExceeded(maximum: Int, window: TimeInterval)
    case launchPreparationFailed
    case invalidLaunchPreparationCommand(RuntimeCommand)
    case operationAlreadyActive(String)
    case invalidOperationAdmission
    case unknownOperation(String)
    case shuttingDown

    public var errorDescription: String? {
        switch self {
        case .invalidConfiguration:
            return "The runtime controller configuration is invalid."
        case .notConnected:
            return "The runtime helper is not connected."
        case .controllerStopping:
            return "The runtime helper is stopping."
        case .helloMustBeFirst:
            return "The runtime helper did not send hello as its first frame."
        case .duplicateHello:
            return "The runtime helper sent hello more than once."
        case .handshakeTimedOut:
            return "The runtime helper did not complete its handshake in time."
        case .invalidHello:
            return "The runtime helper sent an invalid hello frame."
        case let .incompatibleProtocol(app, helper):
            return "The app protocol range \(app) is incompatible with helper range \(helper)."
        case let .bundleBuildMismatch(expected, actual):
            return "The runtime helper build ID \(actual) does not match app build ID \(expected)."
        case .unexpectedHelperRequest:
            return "The runtime helper sent a request on its output stream."
        case let .protocolVersionMismatch(expected, actual):
            return "The runtime frame version \(actual) does not match negotiated version \(expected)."
        case .protocolViolation:
            return "The runtime helper violated the internal protocol."
        case .processLaunchFailed:
            return "The runtime helper could not be launched."
        case .transportWriteFailed:
            return "The app could not write to the runtime helper."
        case .requestEncodingFailed:
            return "The runtime request could not be encoded."
        case .invalidResponsePayload:
            return "The runtime helper returned an invalid response payload."
        case .missingResponseResult:
            return "The runtime helper response did not contain a result."
        case let .requestTimedOut(id, command):
            return "Runtime request \(id) (\(command.rawValue)) timed out."
        case let .requestCancelled(id):
            return "Runtime request \(id) was canceled locally."
        case let .duplicateRequestID(id):
            return "Runtime request ID \(id) is already pending."
        case let .helperExited(status, _):
            return "The runtime helper exited unexpectedly with status \(status)."
        case let .restartLimitExceeded(maximum, window):
            return "The runtime helper exceeded \(maximum) automatic restarts in \(window) seconds."
        case .launchPreparationFailed:
            return "The runtime helper could not be prepared after launch."
        case let .invalidLaunchPreparationCommand(command):
            return "Launch preparation cannot issue \(command.rawValue)."
        case let .operationAlreadyActive(id):
            return "Runtime operation \(id) is still active."
        case .invalidOperationAdmission:
            return "The runtime helper returned an invalid operation admission."
        case let .unknownOperation(id):
            return "Runtime operation \(id) is not being tracked."
        case .shuttingDown:
            return "The runtime controller is shutting down."
        }
    }
}

public enum RuntimeConnectionState: Sendable, Equatable {
    case idle
    case launching
    case awaitingHello
    case reconciling
    case connected
    case restarting(attempt: Int, delay: TimeInterval)
    case stopping
    case stopped
    case failed(RuntimeControllerError)
}

public struct RuntimeOperationHandle: Sendable, Equatable {
    public var id: String
    public var requestID: String
    public var kind: RuntimeOperationKind
    public var launchIdentity: RuntimeLaunchIdentity

    public init(id: String, requestID: String, kind: RuntimeOperationKind, launchIdentity: RuntimeLaunchIdentity) {
        self.id = id
        self.requestID = requestID
        self.kind = kind
        self.launchIdentity = launchIdentity
    }
}

public struct RuntimeTrackedOperation: Sendable, Equatable {
    public var id: String
    public var requestID: String?
    public var kind: RuntimeOperationKind
    public var phase: RuntimeOperationPhase?
    public var status: RuntimeOperationStatus
    public var helperEpoch: String
    public var stateRevision: UInt64?
    public var runtimeGeneration: UInt64?
    public var error: RuntimeStructuredError?
    public var launchToken: UUID

    public init(
        id: String,
        requestID: String? = nil,
        kind: RuntimeOperationKind,
        phase: RuntimeOperationPhase? = nil,
        status: RuntimeOperationStatus,
        helperEpoch: String,
        stateRevision: UInt64? = nil,
        runtimeGeneration: UInt64? = nil,
        error: RuntimeStructuredError? = nil,
        launchToken: UUID = RuntimeLaunchIdentity.zero.launchToken
    ) {
        self.id = id
        self.requestID = requestID
        self.kind = kind
        self.phase = phase
        self.status = status
        self.helperEpoch = helperEpoch
        self.stateRevision = stateRevision
        self.runtimeGeneration = runtimeGeneration
        self.error = error
        self.launchToken = launchToken
    }

    public var isTerminal: Bool { status.isTerminal }
}

public struct RuntimeControllerSnapshot: Sendable, Equatable {
    public var launchIdentity: RuntimeLaunchIdentity?
    public var connectionState: RuntimeConnectionState
    public var hello: RuntimeHelloPayload?
    public var negotiatedProtocolVersion: Int?
    public var helperEpoch: String?
    public var lastStateRevision: UInt64?
    public var currentState: RuntimeStateEvent?
    public var previousState: RuntimeStateEvent?
    public var activeOperationID: String?
    public var operations: [String: RuntimeTrackedOperation]
    public var automaticRestartsInWindow: Int
    public var diagnostics: RuntimeDiagnosticsSnapshot

    public init(
        launchIdentity: RuntimeLaunchIdentity?,
        connectionState: RuntimeConnectionState,
        hello: RuntimeHelloPayload?,
        negotiatedProtocolVersion: Int?,
        helperEpoch: String?,
        lastStateRevision: UInt64?,
        currentState: RuntimeStateEvent?,
        previousState: RuntimeStateEvent?,
        activeOperationID: String?,
        operations: [String: RuntimeTrackedOperation],
        automaticRestartsInWindow: Int,
        diagnostics: RuntimeDiagnosticsSnapshot
    ) {
        self.launchIdentity = launchIdentity
        self.connectionState = connectionState
        self.hello = hello
        self.negotiatedProtocolVersion = negotiatedProtocolVersion
        self.helperEpoch = helperEpoch
        self.lastStateRevision = lastStateRevision
        self.currentState = currentState
        self.previousState = previousState
        self.activeOperationID = activeOperationID
        self.operations = operations
        self.automaticRestartsInWindow = automaticRestartsInWindow
        self.diagnostics = diagnostics
    }
}

public enum RuntimeControllerNotification: Sendable, Equatable {
    case connectionStateChanged(RuntimeConnectionState)
    case hello(RuntimeHelloPayload)
    case state(RuntimeStateEvent)
    case operation(RuntimeTrackedOperation)
    case deviceCode(RuntimeDeviceCodeEvent)
    case unknownEvent(RuntimeEventEnvelope)
}

public struct RuntimeScopedNotification: Sendable, Equatable {
    public var launchToken: UUID
    public var helperEpoch: String?
    public var notification: RuntimeControllerNotification

    public init(launchToken: UUID, helperEpoch: String?, notification: RuntimeControllerNotification) {
        self.launchToken = launchToken
        self.helperEpoch = helperEpoch
        self.notification = notification
    }

    public var launchIdentity: RuntimeLaunchIdentity? {
        guard let helperEpoch, !helperEpoch.isEmpty else { return nil }
        return RuntimeLaunchIdentity(launchToken: launchToken, helperEpoch: helperEpoch)
    }
}

/// Actor-isolated owner of one helper process and its JSONL protocol session.
/// It deliberately has no UI, analytics, Keychain, or application-state dependency.
public actor RuntimeController {
    public private(set) var connectionState: RuntimeConnectionState = .idle
    public private(set) var launchToken: UUID?
    public private(set) var hello: RuntimeHelloPayload?
    public private(set) var negotiatedProtocolVersion: Int?
    public private(set) var helperEpoch: String?
    public private(set) var lastStateRevision: UInt64?
    public private(set) var currentState: RuntimeStateEvent?
    public private(set) var previousState: RuntimeStateEvent?
    public private(set) var activeOperationID: String?
    public private(set) var operations: [String: RuntimeTrackedOperation] = [:]
    public private(set) var automaticRestartsInWindow = 0

    private let configuration: RuntimeControllerConfiguration
    private let processFactory: any RuntimeProcessFactory
    private let clock: any RuntimeClock
    private let idGenerator: any RuntimeIDGenerator
    private let launchPreparation: RuntimeLaunchPreparation
    private let frameCodec: RuntimeFrameCodec

    private var diagnostics: BoundedRuntimeDiagnostics
    private var session: Session?
    private var pendingRequests: [String: PendingRequest] = [:]
    private var connectionWaiters: [CheckedContinuation<RuntimeHelloPayload, Error>] = []
    private var operationWaiters: [String: [CheckedContinuation<RuntimeTrackedOperation, Error>]] = [:]
    private var notificationContinuations: [UUID: AsyncStream<RuntimeControllerNotification>.Continuation] = [:]
    private var scopedNotificationContinuations: [UUID: AsyncStream<RuntimeScopedNotification>.Continuation] = [:]
    private var lastNotificationIdentity: RuntimeLaunchIdentity?
    private var terminationWaiters: [UUID: [UUID: TerminationWaiter]] = [:]
    private var completedTerminations: [UUID: RuntimeProcessTermination] = [:]
    private var restartFailureDates: [Date] = []
    private var scheduledRestartTask: Task<Void, Never>?
    private var scheduledRestartID: UUID?
    private var suppression: RestartSuppression = .none
    private var currentRuntimeGeneration: UInt64?
    private var restoreProxyAfterReconnect = false

    public init(
        configuration: RuntimeControllerConfiguration,
        processFactory: any RuntimeProcessFactory = FoundationRuntimeProcessFactory(),
        clock: any RuntimeClock = SystemRuntimeClock(),
        idGenerator: any RuntimeIDGenerator = UUIDRuntimeIDGenerator(),
        launchPreparation: @escaping RuntimeLaunchPreparation = { _ in [] }
    ) {
        self.configuration = configuration
        self.processFactory = processFactory
        self.clock = clock
        self.idGenerator = idGenerator
        self.launchPreparation = launchPreparation
        frameCodec = RuntimeFrameCodec(maximumFrameSize: configuration.maximumFrameSize)
        diagnostics = BoundedRuntimeDiagnostics(capacity: configuration.diagnosticsByteLimit)
    }

    public func snapshot() -> RuntimeControllerSnapshot {
        pruneRestartHistory(now: clock.now())
        let identity = launchToken.flatMap { token in helperEpoch.map { RuntimeLaunchIdentity(launchToken: token, helperEpoch: $0) } }
        return RuntimeControllerSnapshot(
            launchIdentity: identity,
            connectionState: connectionState,
            hello: hello,
            negotiatedProtocolVersion: negotiatedProtocolVersion,
            helperEpoch: helperEpoch,
            lastStateRevision: lastStateRevision,
            currentState: currentState,
            previousState: previousState,
            activeOperationID: activeOperationID,
            operations: operations,
            automaticRestartsInWindow: automaticRestartsInWindow,
            diagnostics: diagnostics.snapshot()
        )
    }

    public func diagnosticsSnapshot() -> RuntimeDiagnosticsSnapshot {
        diagnostics.snapshot()
    }

    public func notificationStream() -> AsyncStream<RuntimeControllerNotification> {
        let id = UUID()
        var continuation: AsyncStream<RuntimeControllerNotification>.Continuation!
        let stream = AsyncStream<RuntimeControllerNotification> { continuation = $0 }
        notificationContinuations[id] = continuation
        continuation.onTermination = { [weak self] _ in
            Task { await self?.removeNotificationContinuation(id) }
        }
        return stream
    }

    public func scopedNotificationStream() -> AsyncStream<RuntimeScopedNotification> {
        let id = UUID()
        var continuation: AsyncStream<RuntimeScopedNotification>.Continuation!
        let stream = AsyncStream<RuntimeScopedNotification> { continuation = $0 }
        scopedNotificationContinuations[id] = continuation
        continuation.onTermination = { [weak self] _ in
            Task { await self?.removeScopedNotificationContinuation(id) }
        }
        return stream
    }

    public func connect() async throws -> RuntimeHelloPayload {
        switch connectionState {
        case .connected:
            guard let hello else { throw RuntimeControllerError.protocolViolation }
            return hello
        case .launching, .awaitingHello, .reconciling, .restarting:
            return try await waitForConnection()
        case .stopping:
            throw RuntimeControllerError.controllerStopping
        case let .failed(error) where session != nil:
            throw error
        case .idle, .stopped, .failed:
            suppression = .none
            scheduledRestartTask?.cancel()
            scheduledRestartTask = nil
            scheduledRestartID = nil
            restartFailureDates.removeAll()
            automaticRestartsInWindow = 0
            let shouldLaunch = session == nil
            return try await withCheckedThrowingContinuation { continuation in
                connectionWaiters.append(continuation)
                if shouldLaunch {
                    beginLaunch(isAutomaticRestart: false, automaticRestartAttempt: nil)
                }
            }
        }
    }

    @discardableResult
    public func send(
        command: RuntimeCommand,
        payload: JSONValue? = nil,
        timeout: TimeInterval? = nil
    ) async throws -> RuntimeResponseEnvelope {
        try await sendInternal(
            command: command,
            payload: payload,
            timeout: timeout ?? configuration.requestTimeout,
            allowDuringReconciliation: false
        )
    }

    @discardableResult
    public func send<Payload: Encodable>(
        command: RuntimeCommand,
        encodablePayload payload: Payload,
        timeout: TimeInterval? = nil
    ) async throws -> RuntimeResponseEnvelope {
        let value: JSONValue
        do {
            value = try JSONValue.encode(payload)
        } catch {
            throw RuntimeControllerError.requestEncodingFailed
        }
        return try await send(command: command, payload: value, timeout: timeout)
    }

    public func request<Result: Decodable>(
        command: RuntimeCommand,
        payload: JSONValue? = nil,
        as resultType: Result.Type,
        timeout: TimeInterval? = nil
    ) async throws -> Result {
        let response = try await send(command: command, payload: payload, timeout: timeout)
        guard let result = response.result else { throw RuntimeControllerError.missingResponseResult }
        do {
            return try result.decode(resultType)
        } catch {
            throw RuntimeControllerError.invalidResponsePayload
        }
    }

    public func refreshSnapshot(timeout: TimeInterval? = nil) async throws -> RuntimeControllerSnapshot {
        let response = try await send(command: .getState, timeout: timeout)
        let responseState = try stateEventFromGetStateResponse(response)
        if let currentState,
           currentState.helperEpoch == responseState.helperEpoch,
           currentState.stateRevision >= responseState.stateRevision {
            return snapshot()
        }
        guard acceptRevision(responseState.stateRevision) else {
            throw RuntimeControllerError.protocolViolation
        }
        applyState(responseState)
        guard let currentState,
              currentState.helperEpoch == responseState.helperEpoch,
              currentState.stateRevision >= responseState.stateRevision else {
            throw RuntimeControllerError.protocolViolation
        }
        return snapshot()
    }

    public func submitOperation(
        command: RuntimeCommand,
        payload: JSONValue? = nil,
        timeout: TimeInterval? = nil
    ) async throws -> RuntimeOperationHandle {
        if let activeOperationID,
           let operation = operations[activeOperationID],
           !operation.isTerminal {
            throw RuntimeControllerError.operationAlreadyActive(activeOperationID)
        }

        let response = try await send(command: command, payload: payload, timeout: timeout)
        guard let result = response.result,
              let admission = try? result.decode(RuntimeAdmissionResult.self),
              admission.accepted,
              let operationID = admission.operationID,
              !operationID.isEmpty else {
            throw RuntimeControllerError.invalidOperationAdmission
        }

        let tracked = operations[operationID] ?? RuntimeTrackedOperation(
            id: operationID,
            requestID: response.id,
            kind: RuntimeOperationKind(command.rawValue),
            status: .accepted,
            helperEpoch: response.helperEpoch,
            launchToken: launchToken ?? RuntimeLaunchIdentity.zero.launchToken
        )
        operations[operationID] = tracked
        if !tracked.isTerminal {
            activeOperationID = operationID
            publish(.operation(tracked))
        }

        return RuntimeOperationHandle(
            id: operationID,
            requestID: response.id,
            kind: tracked.kind,
            launchIdentity: RuntimeLaunchIdentity(launchToken: tracked.launchToken, helperEpoch: tracked.helperEpoch)
        )
    }

    public func waitForOperation(id: String) async throws -> RuntimeTrackedOperation {
        guard let operation = operations[id] else {
            throw RuntimeControllerError.unknownOperation(id)
        }
        if operation.isTerminal { return operation }

        return try await withCheckedThrowingContinuation { continuation in
            operationWaiters[id, default: []].append(continuation)
        }
    }

    public func performOperation(
        command: RuntimeCommand,
        payload: JSONValue? = nil,
        requestTimeout: TimeInterval? = nil
    ) async throws -> RuntimeTrackedOperation {
        let handle = try await submitOperation(
            command: command,
            payload: payload,
            timeout: requestTimeout
        )
        return try await waitForOperation(id: handle.id)
    }

    public func cancelOperation(id: String, timeout: TimeInterval? = nil) async throws {
        let payload = JSONValue.object(["operation_id": .string(id)])
        _ = try await send(command: .cancelOperation, payload: payload, timeout: timeout)
    }

    public func restartHelper() async throws -> RuntimeHelloPayload {
        let shouldRestoreProxy = restoreProxyAfterReconnect
            || currentState?.payload.service == .running
        await shutdown(reason: .manualRestart)
        restoreProxyAfterReconnect = shouldRestoreProxy
        suppression = .none
        return try await connect()
    }

    public func shutdown(
        reason: RuntimeShutdownReason = .quit,
        gracePeriod: TimeInterval? = nil
    ) async {
        scheduledRestartTask?.cancel()
        scheduledRestartTask = nil
        scheduledRestartID = nil
        suppression = .intentional(reason)

        guard let session else {
            failConnectionWaiters(with: RuntimeControllerError.shuttingDown)
            setConnectionState(.stopped)
            return
        }

        setConnectionState(.stopping)
        let sessionID = session.id
        let process = session.process
        session.failureTerminationTask?.cancel()
        session.failureTerminationTask = nil
        let totalGrace = max(0, gracePeriod ?? configuration.shutdownGracePeriod)
        var cooperativeWait = totalGrace

        if session.firstFrameReceived, negotiatedProtocolVersion != nil {
            let commandTimeout = min(configuration.requestTimeout, totalGrace, 0.5)
            cooperativeWait = max(0, totalGrace - commandTimeout)
            _ = try? await sendInternal(
                command: .shutdown,
                payload: nil,
                timeout: commandTimeout,
                allowDuringReconciliation: true,
                allowDuringStopping: true
            )
        }

        process.closeStandardInput()
        if await waitForTermination(sessionID: sessionID, timeout: cooperativeWait) == nil {
            process.terminate()
            if await waitForTermination(
                sessionID: sessionID,
                timeout: configuration.forceTerminationGracePeriod
            ) == nil {
                process.forceTerminate()
                _ = await waitForTermination(
                    sessionID: sessionID,
                    timeout: configuration.forceTerminationGracePeriod
                )
            }
        }

        if self.session?.id == sessionID {
            invalidateSession(
                sessionID: sessionID,
                requestError: .shuttingDown,
                preserveFailureState: false
            )
        }
        setConnectionState(.stopped)
    }

    // MARK: Launch and handshake

    private func waitForConnection() async throws -> RuntimeHelloPayload {
        try await withCheckedThrowingContinuation { continuation in
            connectionWaiters.append(continuation)
        }
    }

    private func beginLaunch(isAutomaticRestart: Bool, automaticRestartAttempt: Int?) {
        guard session == nil else { return }
        guard configuration.maximumFrameSize > 0 else {
            failLaunch(.invalidConfiguration, isAutomaticRestart: isAutomaticRestart)
            return
        }

        if isAutomaticRestart {
            let attempt = automaticRestartAttempt ?? 1
            setConnectionState(.restarting(attempt: attempt, delay: 0))
        } else {
            setConnectionState(.launching)
        }

        let process: any RuntimeProcess
        let decoder: JSONLFrameDecoder
        do {
            process = try processFactory.makeProcess(configuration: configuration.process)
            decoder = try JSONLFrameDecoder(maximumFrameSize: configuration.maximumFrameSize)
        } catch {
            failLaunch(.processLaunchFailed, isAutomaticRestart: isAutomaticRestart)
            return
        }

        let session = Session(
            process: process,
            frameDecoder: decoder,
            isAutomaticRestart: isAutomaticRestart,
            automaticRestartAttempt: automaticRestartAttempt
        )
        self.session = session
        launchToken = session.id

        do {
            try process.run()
        } catch {
            session.acceptingFrames = false
            session.stdoutTask?.cancel()
            session.stderrTask?.cancel()
            session.terminationTask?.cancel()
            self.session = nil
            launchToken = nil
            failLaunch(.processLaunchFailed, isAutomaticRestart: isAutomaticRestart)
            return
        }

        // A validating process may inspect the running code identity before
        // run() returns. Start consuming helper output only after that check
        // succeeds so an unvalidated replacement cannot enter reconciliation.
        installDrains(for: session)
        setConnectionState(.awaitingHello)
        let sessionID = session.id
        let timeout = configuration.handshakeTimeout
        session.handshakeTimeoutTask = Task { [weak self, clock] in
            do {
                try await clock.sleep(for: timeout)
            } catch {
                return
            }
            await self?.handshakeDidTimeOut(sessionID: sessionID)
        }
    }

    private func installDrains(for session: Session) {
        let sessionID = session.id
        let output = session.process.standardOutput
        let error = session.process.standardError
        let termination = session.process.termination

        let stdoutTask = Task.detached(priority: .userInitiated) { [weak self] in
            for await chunk in output {
                await self?.receiveStandardOutput(chunk, sessionID: sessionID)
            }
            await self?.standardOutputDidClose(sessionID: sessionID)
        }
        session.stdoutTask = stdoutTask

        session.stderrTask = Task.detached(priority: .utility) { [weak self] in
            for await chunk in error {
                await self?.receiveStandardError(chunk, sessionID: sessionID)
            }
        }

        session.terminationTask = Task.detached(priority: .userInitiated) { [weak self] in
            for await result in termination {
                await stdoutTask.value
                guard !Task.isCancelled else { return }
                await self?.processDidTerminate(result, sessionID: sessionID)
                return
            }
        }
    }

    private func handshakeDidTimeOut(sessionID: UUID) {
        guard let session, session.id == sessionID, !session.firstFrameReceived else { return }
        failSession(sessionID: sessionID, error: .handshakeTimedOut, suppressRestart: false)
    }

    private func receiveStandardOutput(_ data: Data, sessionID: UUID) {
        guard let session, session.id == sessionID, session.acceptingFrames else { return }

        do {
            let frames = try session.frameDecoder.append(data)
            for frame in frames where session.acceptingFrames {
                try handleFrame(frame, session: session)
            }
        } catch let error as RuntimeControllerError {
            failSession(
                sessionID: sessionID,
                error: error,
                suppressRestart: shouldSuppressAutomaticRestart(after: error)
            )
        } catch {
            failSession(sessionID: sessionID, error: .protocolViolation, suppressRestart: false)
        }
    }

    private func standardOutputDidClose(sessionID: UUID) {
        guard let session, session.id == sessionID, session.acceptingFrames else { return }
        do {
            try session.frameDecoder.finish()
        } catch {
            failSession(sessionID: sessionID, error: .protocolViolation, suppressRestart: false)
        }
    }

    private func receiveStandardError(_ data: Data, sessionID: UUID) {
        guard session?.id == sessionID else { return }
        diagnostics.append(data)
    }

    private func handleFrame(_ data: Data, session: Session) throws {
        let frame = try frameCodec.decode(data)
        if !session.firstFrameReceived {
            try handleFirstFrame(frame, session: session)
            return
        }

        guard let negotiatedProtocolVersion else { throw RuntimeControllerError.protocolViolation }
        switch frame {
        case .request:
            throw RuntimeControllerError.unexpectedHelperRequest
        case let .response(response):
            guard response.version == negotiatedProtocolVersion else {
                throw RuntimeControllerError.protocolVersionMismatch(
                    expected: negotiatedProtocolVersion,
                    actual: response.version
                )
            }
            handleResponse(response, sessionID: session.id)
        case let .event(event):
            guard event.event != .hello else { throw RuntimeControllerError.duplicateHello }
            guard event.version == negotiatedProtocolVersion else {
                throw RuntimeControllerError.protocolVersionMismatch(
                    expected: negotiatedProtocolVersion,
                    actual: event.version
                )
            }
            try handleEvent(event)
        }
    }

    private func handleFirstFrame(_ frame: RuntimeWireFrame, session: Session) throws {
        guard case let .event(event) = frame, event.event == .hello else {
            throw RuntimeControllerError.helloMustBeFirst
        }
        let decoded = try event.decodePayload()
        guard case let .hello(hello) = decoded else {
            throw RuntimeControllerError.invalidHello
        }

        guard hello.protocolMin > 0,
              hello.protocolMin <= hello.protocolMax,
              !hello.helperBuild.isEmpty,
              !hello.helperEpoch.isEmpty,
              hello.pid > 0,
              event.stateRevision == nil,
              event.helperEpoch == nil || event.helperEpoch == hello.helperEpoch else {
            throw RuntimeControllerError.invalidHello
        }

        let helperRange = hello.protocolMin...hello.protocolMax
        let appRange = configuration.supportedProtocolVersions
        guard appRange.overlaps(helperRange) else {
            throw RuntimeControllerError.incompatibleProtocol(app: appRange, helper: helperRange)
        }
        guard appRange.contains(event.version), helperRange.contains(event.version) else {
            throw RuntimeControllerError.protocolVersionMismatch(
                expected: min(appRange.upperBound, helperRange.upperBound),
                actual: event.version
            )
        }
        guard hello.bundleBuildID == configuration.expectedBundleBuildID else {
            throw RuntimeControllerError.bundleBuildMismatch(
                expected: configuration.expectedBundleBuildID,
                actual: hello.bundleBuildID
            )
        }

        session.firstFrameReceived = true
        session.handshakeTimeoutTask?.cancel()
        self.hello = hello
        helperEpoch = hello.helperEpoch
        let replacementIdentity = RuntimeLaunchIdentity(
            launchToken: session.id,
            helperEpoch: hello.helperEpoch
        )
        if !session.isAutomaticRestart || lastNotificationIdentity == nil {
            lastNotificationIdentity = replacementIdentity
        }
        negotiatedProtocolVersion = event.version
        lastStateRevision = nil
        currentRuntimeGeneration = nil
        setConnectionState(.reconciling)
        publish(.hello(hello))

        let sessionID = session.id
        let isAutomaticRestart = session.isAutomaticRestart
        let automaticRestartAttempt = session.automaticRestartAttempt
        Task { [weak self] in
            await self?.reconcile(
                sessionID: sessionID,
                hello: hello,
                isAutomaticRestart: isAutomaticRestart,
                automaticRestartAttempt: automaticRestartAttempt
            )
        }
    }

    private func reconcile(
        sessionID: UUID,
        hello: RuntimeHelloPayload,
        isAutomaticRestart: Bool,
        automaticRestartAttempt: Int?
    ) async {
        do {
            let response = try await sendInternal(
                command: .getState,
                payload: nil,
                timeout: configuration.requestTimeout,
                allowDuringReconciliation: true
            )
            guard session?.id == sessionID else { return }
            let responseState = try stateEventFromGetStateResponse(response)
            let preparationState: RuntimeStateEvent
            if let currentState,
               currentState.helperEpoch == responseState.helperEpoch,
               currentState.stateRevision >= responseState.stateRevision {
                preparationState = currentState
            } else {
                preparationState = responseState
            }
            do {
                let context = RuntimeLaunchContext(
                    hello: hello,
                    state: preparationState.payload,
                    isAutomaticRestart: isAutomaticRestart,
                    automaticRestartAttempt: automaticRestartAttempt
                )
                let prepared = try await launchPreparation(context)
                guard session?.id == sessionID else { return }

                for request in prepared {
                    guard request.command == .setSecretProjection else {
                        throw RuntimeControllerError.invalidLaunchPreparationCommand(request.command)
                    }
                    _ = try await sendInternal(
                        command: request.command,
                        payload: request.payload,
                        timeout: configuration.requestTimeout,
                        allowDuringReconciliation: true
                    )
                }
            } catch {
                try applyReconciliationStateIfNeeded(preparationState)
                throw error
            }
            try applyReconciliationStateIfNeeded(preparationState)
            do {
                try await restoreProxyIfNeeded(preparationState)
            } catch is RuntimeStructuredError {
                // The replacement helper is healthy. A rejected or failed
                // proxy start is represented by its operation and state
                // events, so keep the helper connected for manual recovery.
                setConnectionState(.connected)
                resumeConnectionWaiters()
                return
            }
            setConnectionState(.connected)
            resumeConnectionWaiters()
        } catch let error as RuntimeControllerError {
            failSession(sessionID: sessionID, error: error, suppressRestart: true)
        } catch {
            failSession(sessionID: sessionID, error: .launchPreparationFailed, suppressRestart: true)
        }
    }

    private func applyReconciliationStateIfNeeded(_ state: RuntimeStateEvent) throws {
        if let currentState,
           currentState.helperEpoch == state.helperEpoch,
           currentState.stateRevision >= state.stateRevision {
            return
        }
        guard acceptRevision(state.stateRevision) else {
            throw RuntimeControllerError.protocolViolation
        }
        applyState(state)
    }

    private func restoreProxyIfNeeded(_ state: RuntimeStateEvent) async throws {
        guard restoreProxyAfterReconnect else { return }
        if state.payload.service == .running {
            restoreProxyAfterReconnect = false
            return
        }
        guard state.payload.service == .stopped else {
            throw RuntimeControllerError.protocolViolation
        }

        let payload = JSONValue.object([
            "expected_config_revision": .string(state.payload.expectedStartConfigRevision ?? "")
        ])
        let response = try await sendInternal(
            command: .start,
            payload: payload,
            timeout: configuration.requestTimeout,
            allowDuringReconciliation: true
        )
        guard let result = response.result,
              let admission = try? result.decode(RuntimeAdmissionResult.self),
              admission.accepted,
              let operationID = admission.operationID,
              !operationID.isEmpty else {
            throw RuntimeControllerError.invalidOperationAdmission
        }
        let operation = try await waitForOperation(id: operationID)
        guard operation.status == .succeeded else {
            if let error = operation.error { throw error }
            throw RuntimeControllerError.protocolViolation
        }
        restoreProxyAfterReconnect = false
    }

    // MARK: Requests and responses

    private func sendInternal(
        command: RuntimeCommand,
        payload: JSONValue?,
        timeout: TimeInterval,
        allowDuringReconciliation: Bool,
        allowDuringStopping: Bool = false
    ) async throws -> RuntimeResponseEnvelope {
        guard let session, session.firstFrameReceived,
              let version = negotiatedProtocolVersion,
              helperEpoch != nil else {
            throw RuntimeControllerError.notConnected
        }

        switch connectionState {
        case .connected:
            break
        case .reconciling where allowDuringReconciliation:
            break
        case .stopping where allowDuringStopping:
            break
        case .stopping:
            throw RuntimeControllerError.controllerStopping
        default:
            throw RuntimeControllerError.notConnected
        }

        let requestID = idGenerator.nextRequestID()
        guard !requestID.isEmpty, pendingRequests[requestID] == nil else {
            throw RuntimeControllerError.duplicateRequestID(requestID)
        }
        let envelope = RuntimeRequestEnvelope(
            version: version,
            id: requestID,
            command: command,
            payload: payload
        )
        let line: Data
        do {
            line = try frameCodec.encodeLine(envelope)
        } catch {
            throw RuntimeControllerError.requestEncodingFailed
        }

        try Task.checkCancellation()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { continuation in
                let timeoutTask = Task { [weak self, clock] in
                    do {
                        try await clock.sleep(for: max(0, timeout))
                    } catch {
                        return
                    }
                    await self?.requestDidTimeOut(id: requestID)
                }
                pendingRequests[requestID] = PendingRequest(
                    command: command,
                    launchToken: session.id,
                    helperEpoch: helperEpoch ?? "",
                    continuation: continuation,
                    timeoutTask: timeoutTask
                )

                let previousWrite = session.writeTail
                let writeTask = Task { [weak self] in
                    if let previousWrite { await previousWrite.value }
                    guard !Task.isCancelled else { return }
                    await self?.writePendingRequest(
                        id: requestID, sessionID: session.id, data: line
                    )
                }
                session.writeTail = writeTask
            }
        } onCancel: { [weak self] in
            Task { await self?.cancelPendingRequest(id: requestID) }
        }
    }

    private func writePendingRequest(id: String, sessionID: UUID, data: Data) async {
        guard let pending = pendingRequests[id], pending.launchToken == sessionID,
              let session, session.id == sessionID else { return }
        do {
            try await session.process.writeStandardInput(data)
        } catch {
            guard let failed = pendingRequests.removeValue(forKey: id) else { return }
            failed.timeoutTask.cancel()
            failed.continuation.resume(throwing: RuntimeControllerError.transportWriteFailed)
        }
    }

    private func handleResponse(_ response: RuntimeResponseEnvelope, sessionID: UUID) {
        guard session?.id == sessionID, response.helperEpoch == helperEpoch else { return }
        guard let pending = pendingRequests[response.id], pending.launchToken == sessionID, pending.helperEpoch == response.helperEpoch else { return }
        pendingRequests.removeValue(forKey: response.id)
        pending.timeoutTask.cancel()

        if response.ok {
            trackAdmissionIfPresent(response: response, command: pending.command)
            pending.continuation.resume(returning: response)
        } else if let error = response.error {
            pending.continuation.resume(throwing: error)
        } else {
            pending.continuation.resume(throwing: RuntimeControllerError.protocolViolation)
        }
    }

    private func trackAdmissionIfPresent(response: RuntimeResponseEnvelope, command: RuntimeCommand) {
        guard let result = response.result,
              let admission = try? result.decode(RuntimeAdmissionResult.self),
              admission.accepted,
              let operationID = admission.operationID,
              !operationID.isEmpty else { return }

        let tracked = RuntimeTrackedOperation(
            id: operationID,
            requestID: response.id,
            kind: RuntimeOperationKind(command.rawValue),
            status: .accepted,
            helperEpoch: response.helperEpoch,
            launchToken: launchToken ?? RuntimeLaunchIdentity.zero.launchToken
        )
        operations[operationID] = tracked
        activeOperationID = operationID
        publish(.operation(tracked))
    }

    private func requestDidTimeOut(id: String) {
        guard let pending = pendingRequests.removeValue(forKey: id) else { return }
        pending.timeoutTask.cancel()
        pending.continuation.resume(
            throwing: RuntimeControllerError.requestTimedOut(id: id, command: pending.command)
        )
    }

    private func cancelPendingRequest(id: String) {
        guard let pending = pendingRequests.removeValue(forKey: id) else { return }
        pending.timeoutTask.cancel()
        pending.continuation.resume(throwing: RuntimeControllerError.requestCancelled(id: id))
    }

    // MARK: Events and filtering

    private func handleEvent(_ envelope: RuntimeEventEnvelope) throws {
        guard envelope.helperEpoch == helperEpoch else { return }
        let decoded = try envelope.decodePayload()

        switch decoded {
        case .hello:
            throw RuntimeControllerError.duplicateHello
        case let .state(event):
            guard acceptRevision(event.stateRevision) else { return }
            applyState(event)
        case let .operation(event):
            guard acceptRevision(event.stateRevision) else { return }
            applyOperation(event)
        case let .deviceCode(event):
            guard acceptRevision(event.stateRevision) else { return }
            guard activeOperationID == nil || activeOperationID == event.payload.operationID else { return }
            publish(.deviceCode(event))
        case let .unknown(event):
            if let revision = event.stateRevision, !acceptRevision(revision) { return }
            publish(.unknownEvent(event))
        }
    }

    private func acceptRevision(_ revision: UInt64) -> Bool {
        if let lastStateRevision, revision <= lastStateRevision { return false }
        lastStateRevision = revision
        return true
    }

    private func applyState(_ event: RuntimeStateEvent) {
        if let generation = event.payload.runtimeGeneration,
           let currentRuntimeGeneration,
           generation < currentRuntimeGeneration {
            return
        }

        currentState = event
        if let launchToken, event.helperEpoch == helperEpoch {
            lastNotificationIdentity = RuntimeLaunchIdentity(
                launchToken: launchToken,
                helperEpoch: event.helperEpoch
            )
        }
        if let generation = event.payload.runtimeGeneration {
            currentRuntimeGeneration = generation
        }

        if case let .active(summary) = event.payload.operation {
            if activeOperationID == nil || activeOperationID == summary.id {
                if let existing = operations[summary.id], existing.isTerminal {
                    publish(.state(event))
                    return
                }
                activeOperationID = summary.id
                var tracked = operations[summary.id] ?? RuntimeTrackedOperation(
                    id: summary.id,
                    kind: summary.kind,
                    status: .running,
                    helperEpoch: event.helperEpoch,
                    launchToken: launchToken ?? RuntimeLaunchIdentity.zero.launchToken
                )
                tracked.kind = summary.kind
                tracked.phase = summary.phase
                if !tracked.isTerminal { tracked.status = .running }
                tracked.stateRevision = event.stateRevision
                tracked.runtimeGeneration = event.payload.runtimeGeneration
                operations[summary.id] = tracked
            }
        }

        publish(.state(event))
    }

    private func applyOperation(_ event: RuntimeOperationEvent) {
        let payload = event.payload
        if let generation = payload.runtimeGeneration,
           let currentRuntimeGeneration,
           generation < currentRuntimeGeneration {
            return
        }
        if let activeOperationID, activeOperationID != payload.operationID {
            return
        }
        if let existing = operations[payload.operationID], existing.isTerminal {
            return
        }

        if let generation = payload.runtimeGeneration,
           currentRuntimeGeneration.map({ generation > $0 }) ?? true {
            currentRuntimeGeneration = generation
        }

        var tracked = operations[payload.operationID] ?? RuntimeTrackedOperation(
            id: payload.operationID,
            kind: payload.kind ?? RuntimeOperationKind("unknown"),
            status: payload.status,
            helperEpoch: event.helperEpoch,
            launchToken: launchToken ?? RuntimeLaunchIdentity.zero.launchToken
        )
        if let kind = payload.kind { tracked.kind = kind }
        tracked.phase = payload.phase ?? tracked.phase
        tracked.status = payload.status
        tracked.stateRevision = event.stateRevision
        tracked.runtimeGeneration = payload.runtimeGeneration ?? tracked.runtimeGeneration
        tracked.error = payload.error
        operations[payload.operationID] = tracked

        if tracked.isTerminal {
            if activeOperationID == payload.operationID { activeOperationID = nil }
            resumeOperationWaiters(for: tracked)
        } else {
            activeOperationID = payload.operationID
        }
        publish(.operation(tracked))
    }

    private func stateEventFromGetStateResponse(
        _ response: RuntimeResponseEnvelope
    ) throws -> RuntimeStateEvent {
        guard let result = response.result else {
            throw RuntimeControllerError.missingResponseResult
        }
        let decoded: GetStateResult
        do {
            decoded = try result.decode(GetStateResult.self)
        } catch {
            throw RuntimeControllerError.invalidResponsePayload
        }
        guard let payload = decoded.payload else {
            throw RuntimeControllerError.invalidResponsePayload
        }
        return RuntimeStateEvent(
            helperEpoch: response.helperEpoch,
            stateRevision: decoded.stateRevision,
            payload: payload
        )
    }

    // MARK: Exit, restart, and shutdown

    private func processDidTerminate(_ result: RuntimeProcessTermination, sessionID: UUID) {
        if !resumeTerminationWaiters(sessionID: sessionID, result: result) {
            completedTerminations[sessionID] = result
            if completedTerminations.count > 16, let oldest = completedTerminations.keys.first {
                completedTerminations.removeValue(forKey: oldest)
            }
        }

        guard let session, session.id == sessionID else { return }
        if suppression == .none, currentState?.payload.service == .running {
            restoreProxyAfterReconnect = true
        }
        session.handshakeTimeoutTask?.cancel()
        session.failureTerminationTask?.cancel()
        session.writeTail?.cancel()
        session.acceptingFrames = false
        self.session = nil

        let exitError = RuntimeControllerError.helperExited(status: result.status, reason: result.reason)
        failPendingRequests(with: exitError)
        failActiveOperationsForHelperExit()
        moveCurrentStateToPreviousRun()
        clearSessionIdentity()

        switch suppression {
        case .none:
            scheduleAutomaticRestart()
        case .intentional:
            setConnectionState(.stopped)
            failConnectionWaiters(with: RuntimeControllerError.shuttingDown)
        case let .failure(error):
            setConnectionState(.failed(error))
            failConnectionWaiters(with: error)
        }
    }

    private func scheduleAutomaticRestart() {
        let policy = configuration.restartPolicy
        let now = clock.now()
        pruneRestartHistory(now: now)

        guard restartFailureDates.count < policy.maximumAutomaticRestarts else {
            let error = RuntimeControllerError.restartLimitExceeded(
                maximum: policy.maximumAutomaticRestarts,
                window: policy.window
            )
            suppression = .failure(error)
            setConnectionState(.failed(error))
            failConnectionWaiters(with: error)
            return
        }

        restartFailureDates.append(now)
        automaticRestartsInWindow = restartFailureDates.count
        let attempt = restartFailureDates.count
        let delay = policy.delay(forAttempt: attempt)
        let scheduleID = UUID()
        scheduledRestartID = scheduleID
        setConnectionState(.restarting(attempt: attempt, delay: delay))

        scheduledRestartTask?.cancel()
        scheduledRestartTask = Task { [weak self, clock] in
            do {
                try await clock.sleep(for: delay)
            } catch {
                return
            }
            await self?.runScheduledRestart(id: scheduleID, attempt: attempt)
        }
    }

    private func runScheduledRestart(id: UUID, attempt: Int) {
        guard scheduledRestartID == id, suppression == .none, session == nil else { return }
        scheduledRestartID = nil
        scheduledRestartTask = nil
        beginLaunch(isAutomaticRestart: true, automaticRestartAttempt: attempt)
    }

    private func failLaunch(_ error: RuntimeControllerError, isAutomaticRestart: Bool) {
        if isAutomaticRestart, suppression == .none {
            scheduleAutomaticRestart()
        } else {
            suppression = .failure(error)
            setConnectionState(.failed(error))
            failConnectionWaiters(with: error)
        }
    }

    private func failSession(sessionID: UUID, error: RuntimeControllerError, suppressRestart: Bool) {
        guard let session, session.id == sessionID else { return }
        if suppressRestart { suppression = .failure(error) }
        guard session.failureTerminationTask == nil else { return }
        let helperCompletedHandshake = connectionState == .connected || connectionState == .reconciling
        let cooperativeTerminationGracePeriod = helperCompletedHandshake
            ? configuration.shutdownGracePeriod
            : configuration.forceTerminationGracePeriod
        session.acceptingFrames = false
        session.handshakeTimeoutTask?.cancel()
        session.writeTail?.cancel()
        setConnectionState(.failed(error))
        failConnectionWaiters(with: error)
        failPendingRequests(with: error)
        session.process.closeStandardInput()
        session.process.terminate()
        session.failureTerminationTask = Task { [weak self] in
            await self?.escalateFailedSessionTermination(
                sessionID: sessionID,
                error: error,
                cooperativeGracePeriod: cooperativeTerminationGracePeriod
            )
        }
    }

    private func escalateFailedSessionTermination(
        sessionID: UUID,
        error: RuntimeControllerError,
        cooperativeGracePeriod: TimeInterval
    ) async {
        guard session?.id == sessionID else { return }

        if await waitForTermination(
            sessionID: sessionID,
            timeout: cooperativeGracePeriod
        ) != nil {
            return
        }
        guard !Task.isCancelled,
              let activeSession = self.session,
              activeSession.id == sessionID else { return }

        activeSession.process.forceTerminate()
        if await waitForTermination(
            sessionID: sessionID,
            timeout: configuration.forceTerminationGracePeriod
        ) != nil {
            return
        }
        guard !Task.isCancelled,
              let activeSession = self.session,
              activeSession.id == sessionID else { return }

        // A process that reports neither graceful nor forced termination must
        // not keep the controller's session occupied forever. Retire the stale
        // session after both bounded waits so a suppressed failure can be
        // retried manually and a retryable failure can follow restart policy.
        activeSession.failureTerminationTask = nil
        invalidateSession(
            sessionID: sessionID,
            requestError: error,
            preserveFailureState: true
        )
        switch suppression {
        case .none:
            scheduleAutomaticRestart()
        case .intentional:
            setConnectionState(.stopped)
            failConnectionWaiters(with: RuntimeControllerError.shuttingDown)
        case let .failure(failure):
            setConnectionState(.failed(failure))
            failConnectionWaiters(with: failure)
        }
    }

    private func shouldSuppressAutomaticRestart(after error: RuntimeControllerError) -> Bool {
        switch error {
        case .bundleBuildMismatch, .incompatibleProtocol, .protocolVersionMismatch:
            return true
        default:
            return false
        }
    }

    private func invalidateSession(
        sessionID: UUID,
        requestError: RuntimeControllerError,
        preserveFailureState: Bool
    ) {
        guard let session, session.id == sessionID else { return }
        session.acceptingFrames = false
        session.handshakeTimeoutTask?.cancel()
        session.failureTerminationTask?.cancel()
        session.writeTail?.cancel()
        self.session = nil
        failPendingRequests(with: requestError)
        moveCurrentStateToPreviousRun()
        clearSessionIdentity()
        if !preserveFailureState { setConnectionState(.stopped) }
    }

    private func clearSessionIdentity() {
        hello = nil
        launchToken = nil
        helperEpoch = nil
        negotiatedProtocolVersion = nil
        lastStateRevision = nil
        currentRuntimeGeneration = nil
    }

    private func moveCurrentStateToPreviousRun() {
        if let currentState { previousState = currentState }
        currentState = nil
    }

    private func pruneRestartHistory(now: Date) {
        let cutoff = now.addingTimeInterval(-configuration.restartPolicy.window)
        restartFailureDates.removeAll { $0 < cutoff }
        automaticRestartsInWindow = restartFailureDates.count
    }

    private func failPendingRequests(with error: Error) {
        let pending = pendingRequests
        pendingRequests.removeAll()
        for request in pending.values {
            request.timeoutTask.cancel()
            request.continuation.resume(throwing: error)
        }
    }

    private func failActiveOperationsForHelperExit() {
        let structured = RuntimeStructuredError(
            code: "helper_exited",
            userMessage: "The runtime helper exited before the operation completed.",
            retryable: true,
            recoveryAction: "restart_helper"
        )
        for (id, var operation) in operations where !operation.isTerminal {
            operation.status = .failed
            operation.error = structured
            operations[id] = operation
            resumeOperationWaiters(for: operation)
            publish(.operation(operation))
        }
        activeOperationID = nil
    }

    // MARK: Waiters and notifications

    private func setConnectionState(_ state: RuntimeConnectionState) {
        guard connectionState != state else { return }
        connectionState = state
        publish(.connectionStateChanged(state))
    }

    private func resumeConnectionWaiters() {
        guard let hello else { return }
        let waiters = connectionWaiters
        connectionWaiters.removeAll()
        for waiter in waiters { waiter.resume(returning: hello) }
    }

    private func failConnectionWaiters(with error: Error) {
        let waiters = connectionWaiters
        connectionWaiters.removeAll()
        for waiter in waiters { waiter.resume(throwing: error) }
    }

    private func resumeOperationWaiters(for operation: RuntimeTrackedOperation) {
        let waiters = operationWaiters.removeValue(forKey: operation.id) ?? []
        for waiter in waiters { waiter.resume(returning: operation) }
    }

    private func waitForTermination(
        sessionID: UUID,
        timeout: TimeInterval
    ) async -> RuntimeProcessTermination? {
        if let result = completedTerminations.removeValue(forKey: sessionID) { return result }

        let waiterID = UUID()
        return await withCheckedContinuation { continuation in
            let timeoutTask = Task { [weak self, clock] in
                do {
                    try await clock.sleep(for: max(0, timeout))
                } catch {
                    return
                }
                await self?.terminationWaiterDidTimeOut(sessionID: sessionID, waiterID: waiterID)
            }
            var sessionWaiters = terminationWaiters[sessionID] ?? [:]
            sessionWaiters[waiterID] = TerminationWaiter(
                continuation: continuation,
                timeoutTask: timeoutTask
            )
            terminationWaiters[sessionID] = sessionWaiters
        }
    }

    private func terminationWaiterDidTimeOut(sessionID: UUID, waiterID: UUID) {
        guard var sessionWaiters = terminationWaiters[sessionID],
              let waiter = sessionWaiters.removeValue(forKey: waiterID) else { return }
        terminationWaiters[sessionID] = sessionWaiters.isEmpty ? nil : sessionWaiters
        waiter.timeoutTask.cancel()
        waiter.continuation.resume(returning: nil)
    }

    @discardableResult
    private func resumeTerminationWaiters(
        sessionID: UUID,
        result: RuntimeProcessTermination
    ) -> Bool {
        let waiters = terminationWaiters.removeValue(forKey: sessionID) ?? [:]
        for waiter in waiters.values {
            waiter.timeoutTask.cancel()
            waiter.continuation.resume(returning: result)
        }
        return !waiters.isEmpty
    }

    private func publish(_ notification: RuntimeControllerNotification) {
        for continuation in notificationContinuations.values { continuation.yield(notification) }
        let activeIdentity = launchToken.flatMap { token in
            helperEpoch.map { RuntimeLaunchIdentity(launchToken: token, helperEpoch: $0) }
        }
        if let identity = lastNotificationIdentity ?? activeIdentity {
            let scoped = RuntimeScopedNotification(
                launchToken: identity.launchToken,
                helperEpoch: identity.helperEpoch,
                notification: notification
            )
            for continuation in scopedNotificationContinuations.values { continuation.yield(scoped) }
        }
    }

    private func removeNotificationContinuation(_ id: UUID) { notificationContinuations.removeValue(forKey: id) }
    private func removeScopedNotificationContinuation(_ id: UUID) { scopedNotificationContinuations.removeValue(forKey: id) }

    // MARK: Private support types

    private final class Session: @unchecked Sendable {
        let id = UUID()
        let process: any RuntimeProcess
        var frameDecoder: JSONLFrameDecoder
        let isAutomaticRestart: Bool
        let automaticRestartAttempt: Int?
        var acceptingFrames = true
        var firstFrameReceived = false
        var stdoutTask: Task<Void, Never>?
        var stderrTask: Task<Void, Never>?
        var terminationTask: Task<Void, Never>?
        var handshakeTimeoutTask: Task<Void, Never>?
        var failureTerminationTask: Task<Void, Never>?
        var writeTail: Task<Void, Never>?

        init(
            process: any RuntimeProcess,
            frameDecoder: JSONLFrameDecoder,
            isAutomaticRestart: Bool,
            automaticRestartAttempt: Int?
        ) {
            self.process = process
            self.frameDecoder = frameDecoder
            self.isAutomaticRestart = isAutomaticRestart
            self.automaticRestartAttempt = automaticRestartAttempt
        }
    }

    private struct PendingRequest {
        let command: RuntimeCommand
        let launchToken: UUID
        let helperEpoch: String
        let continuation: CheckedContinuation<RuntimeResponseEnvelope, Error>
        let timeoutTask: Task<Void, Never>
    }

    private struct TerminationWaiter {
        let continuation: CheckedContinuation<RuntimeProcessTermination?, Never>
        let timeoutTask: Task<Void, Never>
    }

    private enum RestartSuppression: Equatable {
        case none
        case intentional(RuntimeShutdownReason)
        case failure(RuntimeControllerError)
    }

    private struct GetStateResult: Decodable {
        let stateRevision: UInt64
        let payload: RuntimeStatePayload?

        private enum CodingKeys: String, CodingKey {
            case stateRevision = "state_revision"
            case payload
            case state
        }

        init(from decoder: Decoder) throws {
            let container = try decoder.container(keyedBy: CodingKeys.self)
            stateRevision = try container.decode(UInt64.self, forKey: .stateRevision)
            if let payload = try container.decodeIfPresent(RuntimeStatePayload.self, forKey: .payload) {
                self.payload = payload
            } else if let state = try container.decodeIfPresent(RuntimeStatePayload.self, forKey: .state) {
                payload = state
            } else {
                payload = try? RuntimeStatePayload(from: decoder)
            }
        }
    }
}
