import Foundation
import XCTest
@testable import VekilCore

final class RuntimeControllerTests: XCTestCase {
    func testBundledHelperLocationMatchesAppBundleLayout() {
        let bundleURL = URL(fileURLWithPath: "/Applications/Vekil.app")
        let bundle = Bundle(path: bundleURL.path)

        if let bundle {
            XCTAssertEqual(
                RuntimeHelperLocator.bundledHelperURL(in: bundle).path,
                "/Applications/Vekil.app/Contents/Helpers/vekil-runtime"
            )
        } else {
            XCTAssertEqual(RuntimeHelperLocator.relativePath, "Contents/Helpers/vekil-runtime")
        }
    }

    func testConnectValidatesHelloThenPreparesAndReconcilesBeforeConnected() async throws {
        let process = FakeRuntimeProcess()
        let factory = FakeRuntimeProcessFactory([process])
        let ids = SequenceRuntimeIDGenerator(["req_state", "req_secret"])
        let initialState = RuntimeStatePayload(
            configRevision: "cfg_1",
            service: .stopped,
            readiness: .unknown,
            auth: .signedIn
        )
        let initialStateFrame = try RuntimeFrameCodec().encodeLine(
            RuntimeEventEnvelope(
                version: 1,
                event: .state,
                helperEpoch: "hep_test",
                stateRevision: 4,
                payload: try JSONValue.encode(initialState)
            )
        )

        process.onRun { [weak process] in
            guard let process else { return }
            var frames = try! encodedHello()
            frames.append(initialStateFrame)
            process.emitStandardOutput(frames)
        }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let result = JSONValue.object([
                    "state_revision": .integer(4),
                    "state": try! JSONValue.encode(initialState),
                ])
                process.emitStandardOutput(try! encodedResponse(for: request, result: result))
            } else {
                process.emitStandardOutput(try! encodedResponse(for: request))
            }
        }

        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: factory,
            idGenerator: ids,
            launchPreparation: { context in
                XCTAssertEqual(context.state.configRevision, "cfg_1")
                return [
                    RuntimePreparedRequest(
                        command: .setSecretProjection,
                        payload: .object([
                            "config_revision": .string("cfg_1"),
                            "secret_generation": .integer(2),
                            "secrets": .array([]),
                        ])
                    ),
                ]
            }
        )

        let hello = try await controller.connect()
        XCTAssertEqual(hello.helperEpoch, "hep_test")
        let snapshot = await controller.snapshot()
        XCTAssertEqual(snapshot.connectionState, .connected)
        XCTAssertEqual(snapshot.negotiatedProtocolVersion, 1)
        XCTAssertEqual(snapshot.lastStateRevision, 4)
        XCTAssertEqual(snapshot.currentState?.payload.configRevision, "cfg_1")

        let commands = try process.writtenData.map { try requestFromLine($0).command }
        XCTAssertEqual(commands, [.getState, .setSecretProjection])
    }

    func testRefreshSnapshotQueriesHelperAndAppliesFreshState() async throws {
        let process = FakeRuntimeProcess()
        let initialState = RuntimeStatePayload(
            configRevision: "cfg_initial",
            service: .stopped,
            readiness: .unknown,
            auth: .signedIn,
            configuration: RuntimeConfigurationState(
                mode: .external,
                selectedRevision: "cfg_initial"
            )
        )
        let refreshedState = RuntimeStatePayload(
            configRevision: "cfg_refreshed",
            service: .stopped,
            readiness: .unknown,
            auth: .signedOut,
            configuration: RuntimeConfigurationState(
                mode: .external,
                selectedRevision: "cfg_refreshed",
                drift: .missing
            )
        )
        process.onRun { [weak process] in
            process?.emitStandardOutput(try! encodedHello())
        }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            let isRefresh = request.id == "req_refresh"
            let result = JSONValue.object([
                "state_revision": .integer(isRefresh ? 2 : 1),
                "state": try! JSONValue.encode(isRefresh ? refreshedState : initialState),
            ])
            process.emitStandardOutput(try! encodedResponse(for: request, result: result))
        }
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_initial", "req_refresh"])
        )

        _ = try await controller.connect()
        let snapshot = try await controller.refreshSnapshot()

        XCTAssertEqual(snapshot.currentState?.stateRevision, 2)
        XCTAssertEqual(snapshot.currentState?.payload.configRevision, "cfg_refreshed")
        XCTAssertEqual(snapshot.currentState?.payload.auth, .signedOut)
        XCTAssertEqual(snapshot.currentState?.payload.configuration?.drift, .missing)
        let commands = try process.writtenData.map { try requestFromLine($0).command }
        XCTAssertEqual(commands, [.getState, .getState])
        await controller.shutdown()
    }

    func testHelloMustBeFirstAndFailureSuppressesRestart() async {
        let process = FakeRuntimeProcess()
        let factory = FakeRuntimeProcessFactory([process])
        process.onRun { [weak process] in
            let response = RuntimeResponseEnvelope(
                version: 1,
                id: "unexpected",
                helperEpoch: "hep_test",
                ok: true,
                result: .object([:])
            )
            process?.emitStandardOutput(try! RuntimeFrameCodec().encodeLine(response))
        }

        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: factory
        )

        do {
            _ = try await controller.connect()
            XCTFail("Expected handshake failure")
        } catch {
            XCTAssertEqual(error as? RuntimeControllerError, .helloMustBeFirst)
        }
        let state = await controller.connectionState
        XCTAssertEqual(state, .failed(.helloMustBeFirst))
        XCTAssertEqual(process.terminateCount, 1)
        XCTAssertEqual(factory.receivedConfigurations.count, 1)
    }

    func testHandshakeFailureForceTerminatesUnresponsiveHelperAndAllowsReconnect() async throws {
        let unresponsive = FakeRuntimeProcess()
        unresponsive.onRun { [weak unresponsive] in
            unresponsive?.emitStandardOutput(try! encodedHello(buildID: "wrong-build"))
        }
        let replacement = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: replacement)
        let clock = ManualRuntimeClock()
        var configuration = makeConfiguration(buildID: "bundle_test")
        configuration.handshakeTimeout = 30
        configuration.forceTerminationGracePeriod = 1
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([unresponsive, replacement]),
            clock: clock,
            idGenerator: SequenceRuntimeIDGenerator(["req_state"])
        )

        do {
            _ = try await controller.connect()
            XCTFail("Expected bundle build mismatch")
        } catch {
            XCTAssertEqual(
                error as? RuntimeControllerError,
                .bundleBuildMismatch(expected: "bundle_test", actual: "wrong-build")
            )
        }
        XCTAssertEqual(unresponsive.terminateCount, 1)
        XCTAssertEqual(unresponsive.forceTerminateCount, 0)

        try await eventually {
            clock.recordedSleeps.filter { $0 == 1 }.count == 1
        }
        clock.advance(by: 1)
        try await eventually { unresponsive.forceTerminateCount == 1 }

        try await eventually {
            clock.recordedSleeps.filter { $0 == 1 }.count == 2
        }
        clock.advance(by: 1)
        try await eventually {
            do {
                _ = try await controller.connect()
                return true
            } catch {
                return false
            }
        }

        let connectedState = await controller.connectionState
        XCTAssertEqual(connectedState, .connected)
        XCTAssertEqual(replacement.runCount, 1)
    }

    func testExactBundleBuildIDAndProtocolVersionAreEnforced() async {
        let mismatch = FakeRuntimeProcess()
        mismatch.onRun { [weak mismatch] in
            mismatch?.emitStandardOutput(try! encodedHello(buildID: "other"))
        }
        let mismatchController = RuntimeController(
            configuration: makeConfiguration(buildID: "expected"),
            processFactory: FakeRuntimeProcessFactory([mismatch])
        )

        do {
            _ = try await mismatchController.connect()
            XCTFail("Expected build mismatch")
        } catch {
            XCTAssertEqual(
                error as? RuntimeControllerError,
                .bundleBuildMismatch(expected: "expected", actual: "other")
            )
        }

        let incompatible = FakeRuntimeProcess()
        incompatible.onRun { [weak incompatible] in
            incompatible?.emitStandardOutput(
                try! encodedHello(protocolMin: 2, protocolMax: 3, envelopeVersion: 2)
            )
        }
        let incompatibleController = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([incompatible])
        )
        do {
            _ = try await incompatibleController.connect()
            XCTFail("Expected protocol mismatch")
        } catch {
            XCTAssertEqual(
                error as? RuntimeControllerError,
                .incompatibleProtocol(app: 1...1, helper: 2...3)
            )
        }
    }

    func testResponsesAreCorrelatedOutOfOrderAndWrongEpochIsIgnored() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process)
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_a", "req_b"])
        )
        _ = try await controller.connect()

        let first = Task { try await controller.send(command: .describeConfig) }
        let second = Task { try await controller.send(command: RuntimeCommand("second_read")) }
        try await eventually { process.writtenData.count == 3 }

        let requests = try process.writtenData.dropFirst().map(requestFromLine)
        let firstRequest = try XCTUnwrap(requests.first { $0.command == .describeConfig })
        let secondRequest = try XCTUnwrap(
            requests.first { $0.command == RuntimeCommand("second_read") }
        )

        process.emitStandardOutput(try encodedResponse(for: firstRequest, epoch: "old_epoch"))
        process.emitStandardOutput(
            try encodedResponse(for: secondRequest, result: .object(["value": .string("b")]))
        )
        process.emitStandardOutput(
            try encodedResponse(for: firstRequest, result: .object(["value": .string("a")]))
        )

        let firstResponse = try await first.value
        let secondResponse = try await second.value
        XCTAssertEqual(firstResponse.id, firstRequest.id)
        XCTAssertEqual(secondResponse.id, secondRequest.id)
    }

    func testEpochRevisionAndRuntimeGenerationFilteringPreventRegression() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process, stateRevision: 1, runtimeGeneration: 2)
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process])
        )
        _ = try await controller.connect()

        process.emitStandardOutput(
            try stateEvent(epoch: "hep_test", revision: 2, generation: 3, service: .running)
        )
        process.emitStandardOutput(
            try stateEvent(epoch: "hep_test", revision: 2, generation: 3, service: .failed)
        )
        process.emitStandardOutput(
            try stateEvent(epoch: "hep_test", revision: 1, generation: 3, service: .failed)
        )
        process.emitStandardOutput(
            try stateEvent(epoch: "old_epoch", revision: 100, generation: 100, service: .failed)
        )
        process.emitStandardOutput(
            try stateEvent(epoch: "hep_test", revision: 3, generation: 2, service: .failed)
        )

        try await eventually { await controller.lastStateRevision == 3 }
        let filteredState = await controller.currentState
        XCTAssertEqual(filteredState?.stateRevision, 2)
        XCTAssertEqual(filteredState?.payload.runtimeGeneration, 3)
        XCTAssertEqual(filteredState?.payload.service, .running)
    }

    func testAcceptedOperationIsTrackedThroughProgressAndTerminalEvent() async throws {
        let process = FakeRuntimeProcess()
        process.onRun { [weak process] in
            process?.emitStandardOutput(try! encodedHello())
        }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_start"),
                        ])
                    )
                )
            }
        }

        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_start"])
        )
        _ = try await controller.connect()
        let handle = try await controller.submitOperation(command: .start)
        XCTAssertEqual(handle.id, "op_start")
        let activeOperationID = await controller.activeOperationID
        XCTAssertEqual(activeOperationID, "op_start")

        let terminal = Task { try await controller.waitForOperation(id: handle.id) }
        process.emitStandardOutput(
            try operationEvent(
                revision: 2,
                operationID: "op_start",
                phase: .constructingServer,
                status: .running
            )
        )
        process.emitStandardOutput(
            try operationEvent(
                revision: 3,
                operationID: "op_start",
                phase: .readinessCheck,
                status: .succeeded,
                generation: 1
            )
        )

        let completed = try await terminal.value
        XCTAssertEqual(completed.status, .succeeded)
        XCTAssertEqual(completed.phase, .readinessCheck)
        XCTAssertEqual(completed.stateRevision, 3)
        let finalActiveOperationID = await controller.activeOperationID
        XCTAssertNil(finalActiveOperationID)

        process.emitStandardOutput(
            try operationEvent(
                revision: 4,
                operationID: "op_start",
                phase: .constructingServer,
                status: .running,
                generation: 1
            )
        )
        try await eventually { await controller.lastStateRevision == 4 }
        let stillCompleted = await controller.operations["op_start"]
        XCTAssertEqual(stillCompleted?.status, .succeeded)
        let regressedActiveOperationID = await controller.activeOperationID
        XCTAssertNil(regressedActiveOperationID)
    }

    func testTerminalEventThenNewerPairedStateUpdatesLifecycle() async throws {
        let process = FakeRuntimeProcess()
        process.onRun { [weak process] in process?.emitStandardOutput(try! encodedHello()) }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    runtimeGeneration: 1, configRevision: "cfg_1",
                    service: .stopped, readiness: .unknown, auth: .signedIn
                )
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "state_revision": .integer(1),
                            "payload": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_start"),
                        ])
                    )
                )
            }
        }
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_start"])
        )
        _ = try await controller.connect()
        let handle = try await controller.submitOperation(command: .start)
        process.emitStandardOutput(
            try operationEvent(
                revision: 2, operationID: handle.id, phase: .readinessCheck, status: .succeeded, generation: 2
            )
        )
        process.emitStandardOutput(
            try stateEvent(epoch: "hep_test", revision: 3, generation: 2, service: .running)
        )
        try await eventually { await controller.currentState?.stateRevision == 3 }
        let currentState = await controller.currentState
        let trackedOperation = await controller.operations[handle.id]
        XCTAssertEqual(currentState?.payload.service, .running)
        XCTAssertEqual(trackedOperation?.status, .succeeded)
    }

    func testTerminalFramesArrivingWithAdmissionResponseDoNotResurrectOperation() async throws {
        let process = FakeRuntimeProcess()
        process.onRun { [weak process] in process?.emitStandardOutput(try! encodedHello()) }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    runtimeGeneration: 1, configRevision: "cfg_1",
                    service: .stopped, readiness: .unknown, auth: .signedIn
                )
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "state_revision": .integer(1),
                            "payload": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_fast"),
                        ])
                    )
                )
                process.emitStandardOutput(
                    try! self.operationEvent(
                        revision: 2, operationID: "op_fast", phase: .readinessCheck,
                        status: .succeeded, generation: 2
                    )
                )
                process.emitStandardOutput(
                    try! self.stateEvent(epoch: "hep_test", revision: 3, generation: 2, service: .running)
                )
            }
        }
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_start"])
        )
        _ = try await controller.connect()
        let handle = try await controller.submitOperation(command: .start)
        try await eventually { await controller.currentState?.stateRevision == 3 }
        let snapshot = await controller.snapshot()
        XCTAssertNil(snapshot.activeOperationID)
        XCTAssertEqual(snapshot.operations[handle.id]?.status, .succeeded)
        XCTAssertEqual(snapshot.currentState?.payload.service, .running)
    }

    func testUnexpectedExitFailsPendingRequestsAndOperationsWithoutReplay() async throws {
        let process = FakeRuntimeProcess()
        process.onRun { [weak process] in process?.emitStandardOutput(try! encodedHello()) }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_start"),
                        ])
                    )
                )
            }
        }

        let configuration = makeConfiguration(
            restartPolicy: RuntimeRestartPolicy(maximumAutomaticRestarts: 0)
        )
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_start", "req_pending"])
        )
        _ = try await controller.connect()
        let handle = try await controller.submitOperation(command: .start)
        let operation = Task { try await controller.waitForOperation(id: handle.id) }
        let pending = Task { try await controller.send(command: .describeConfig) }
        try await eventually { process.writtenData.count == 3 }

        process.emitExit(status: 9, reason: .uncaughtSignal)

        do {
            _ = try await pending.value
            XCTFail("Expected pending request failure")
        } catch {
            XCTAssertEqual(
                error as? RuntimeControllerError,
                .helperExited(status: 9, reason: .uncaughtSignal)
            )
        }
        let failedOperation = try await operation.value
        XCTAssertEqual(failedOperation.status, .failed)
        XCTAssertEqual(failedOperation.error?.code, "helper_exited")
        XCTAssertEqual(process.writtenData.count, 3, "Mutation must not be replayed")

        let state = await controller.connectionState
        XCTAssertEqual(state, .failed(.restartLimitExceeded(maximum: 0, window: 60)))
        let currentState = await controller.currentState
        let previousState = await controller.previousState
        XCTAssertNil(currentState)
        XCTAssertNotNil(previousState)
    }

    func testStderrIsContinuouslyDrainedIntoBoundedDiagnostics() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process)
        var configuration = makeConfiguration()
        configuration.diagnosticsByteLimit = 8
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([process])
        )
        _ = try await controller.connect()

        process.emitStandardError(Data("12345".utf8))
        process.emitStandardError(Data("67890".utf8))
        try await eventually { await controller.diagnosticsSnapshot().totalByteCount == 10 }

        let diagnostics = await controller.diagnosticsSnapshot()
        XCTAssertEqual(diagnostics.text, "34567890")
        XCTAssertEqual(diagnostics.retainedByteCount, 8)
        XCTAssertEqual(diagnostics.droppedByteCount, 2)
    }

    func testTerminationWaitsForFinalStandardOutputFrames() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(
            process: process,
            stateRevision: 1,
            runtimeGeneration: 1
        )
        let controller = RuntimeController(
            configuration: makeConfiguration(
                restartPolicy: RuntimeRestartPolicy(maximumAutomaticRestarts: 0)
            ),
            processFactory: FakeRuntimeProcessFactory([process])
        )
        _ = try await controller.connect()

        process.emitTerminationBeforeStandardOutputCloses(status: 0)
        try await Task.sleep(nanoseconds: 20_000_000)
        process.emitStandardOutput(
            try stateEvent(
                epoch: "hep_test",
                revision: 2,
                generation: 2,
                service: .stopped
            )
        )
        process.finishStandardOutput()

        try await eventually {
            await controller.previousState?.stateRevision == 2
        }
        let snapshot = await controller.snapshot()
        XCTAssertEqual(snapshot.previousState?.payload.runtimeGeneration, 2)
        XCTAssertNil(snapshot.currentState)
    }

    func testGracefulShutdownSendsControlFrameClosesInputAndSuppressesRestart() async throws {
        let process = FakeRuntimeProcess()
        process.onRun { [weak process] in process?.emitStandardOutput(try! encodedHello()) }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .shutdown {
                process.emitStandardOutput(try! encodedResponse(for: request))
                process.emitExit(status: 0)
            }
        }

        let factory = FakeRuntimeProcessFactory([process])
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: factory,
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_shutdown"])
        )
        _ = try await controller.connect()
        await controller.shutdown()

        let commands = try process.writtenData.map { try requestFromLine($0).command }
        XCTAssertEqual(commands, [.getState, .shutdown])
        XCTAssertGreaterThanOrEqual(process.closeInputCount, 1)
        let state = await controller.connectionState
        XCTAssertEqual(state, .stopped)
        XCTAssertEqual(factory.receivedConfigurations.count, 1)
    }

    func testDefaultShutdownGraceOutlivesBundledHelperCleanup() {
        let configuration = RuntimeControllerConfiguration(
            process: RuntimeProcessConfiguration(
                executableURL: URL(fileURLWithPath: "/fake/vekil-runtime")
            ),
            expectedBundleBuildID: "bundle_test"
        )
        let commandBudget = min(
            configuration.requestTimeout,
            configuration.shutdownGracePeriod,
            0.5
        )

        XCTAssertGreaterThan(configuration.shutdownGracePeriod - commandBudget, 11)
        XCTAssertGreaterThan(configuration.forceTerminationGracePeriod, 1)
    }

    func testAutomaticRestartPolicyUsesHalfOneTwoSecondDelaysThenStops() async throws {
        let processes = (0..<4).map { _ in FakeRuntimeProcess() }
        for (index, process) in processes.enumerated() {
            configureSuccessfulHandshake(
                process: process,
                epoch: "hep_\(index)",
                stateRevision: 1,
                runtimeGeneration: nil
            )
        }
        let clock = ManualRuntimeClock()
        var configuration = makeConfiguration()
        configuration.handshakeTimeout = 30
        configuration.requestTimeout = 30
        configuration.restartPolicy = RuntimeRestartPolicy(
            maximumAutomaticRestarts: 3,
            window: 60,
            delays: [0.5, 1, 2]
        )
        let factory = FakeRuntimeProcessFactory(processes)
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: factory,
            clock: clock,
            idGenerator: SequenceRuntimeIDGenerator(
                ["req_state_0", "req_state_1", "req_state_2", "req_state_3"]
            )
        )
        _ = try await controller.connect()

        for attempt in 0..<3 {
            processes[attempt].emitExit(status: 1)
            let expectedDelay = [0.5, 1.0, 2.0][attempt]
            try await eventually {
                await controller.connectionState == .restarting(attempt: attempt + 1, delay: expectedDelay)
            }
            clock.advance(by: expectedDelay)
            try await eventually { await controller.connectionState == .connected }
        }

        processes[3].emitExit(status: 1)
        try await eventually {
            await controller.connectionState
                == .failed(.restartLimitExceeded(maximum: 3, window: 60))
        }
        XCTAssertEqual(factory.receivedConfigurations.count, 4)

        let restartSleeps = clock.recordedSleeps.filter { [0.5, 1.0, 2.0].contains($0) }
        XCTAssertEqual(restartSleeps, [0.5, 1.0, 2.0])
        clock.advance(by: 120)
    }

    func testReconciliationRequestTimeoutUsesAutomaticRestartPolicy() async throws {
        let first = FakeRuntimeProcess()
        first.onRun { [weak first] in
            first?.emitStandardOutput(try! encodedHello(epoch: "hep_original"))
        }

        let replacement = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: replacement, epoch: "hep_replacement")

        let clock = ManualRuntimeClock()
        var configuration = makeConfiguration()
        configuration.handshakeTimeout = 30
        configuration.requestTimeout = 1
        configuration.restartPolicy = RuntimeRestartPolicy(
            maximumAutomaticRestarts: 1,
            window: 60,
            delays: [0]
        )
        let factory = FakeRuntimeProcessFactory([first, replacement])
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: factory,
            clock: clock,
            idGenerator: SequenceRuntimeIDGenerator(["req_initial_state", "req_replacement_state"])
        )

        let initialConnect = Task { try await controller.connect() }
        try await eventually {
            first.writtenData.contains { data in
                (try? requestFromLine(data).command) == .getState
            }
        }
        try await eventually { clock.recordedSleeps.contains(1) }
        clock.advance(by: 1)

        do {
            _ = try await initialConnect.value
            XCTFail("Expected the initial reconciliation request to time out")
        } catch {
            XCTAssertEqual(
                error as? RuntimeControllerError,
                .requestTimedOut(id: "req_initial_state", command: .getState)
            )
        }

        try await eventually { first.terminateCount == 1 }
        first.emitExit(status: 1)

        try await eventually { await controller.connectionState == .connected }
        XCTAssertEqual(factory.receivedConfigurations.count, 2)
        XCTAssertEqual(
            try replacement.writtenData.map(requestFromLine).map(\.command),
            [.getState]
        )
    }

    func testAutomaticRestartRestoresPreviouslyRunningProxy() async throws {
        let first = FakeRuntimeProcess()
        first.onRun { [weak first] in
            first?.emitStandardOutput(try! encodedHello(epoch: "hep_original"))
        }
        first.onWrite { [weak first] data in
            guard let first, let request = try? requestFromLine(data), request.command == .getState else {
                return
            }
            let state = RuntimeStatePayload(
                runtimeGeneration: 7,
                configRevision: "cfg_running",
                service: .running,
                readiness: .ready,
                auth: .signedIn
            )
            first.emitStandardOutput(
                try! encodedResponse(
                    for: request,
                    epoch: "hep_original",
                    result: .object([
                        "state_revision": .integer(1),
                        "state": try! JSONValue.encode(state),
                    ])
                )
            )
        }

        let replacement = FakeRuntimeProcess()
        replacement.onRun { [weak replacement] in
            replacement?.emitStandardOutput(try! encodedHello(epoch: "hep_replacement"))
        }
        replacement.onWrite { [weak replacement] data in
            guard let replacement, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    runtimeGeneration: 0,
                    configRevision: "cfg_running",
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_restore"),
                        ])
                    )
                )
                let operation = RuntimeOperationEventPayload(
                    operationID: "op_restore",
                    kind: RuntimeOperationKind("start"),
                    phase: .readinessCheck,
                    status: .succeeded,
                    runtimeGeneration: 1
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .operation,
                            helperEpoch: "hep_replacement",
                            stateRevision: 2,
                            payload: try! JSONValue.encode(operation)
                        )
                    )
                )
                let running = RuntimeStatePayload(
                    runtimeGeneration: 1,
                    configRevision: "cfg_running",
                    service: .running,
                    readiness: .ready,
                    auth: .signedIn
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .state,
                            helperEpoch: "hep_replacement",
                            stateRevision: 3,
                            payload: try! JSONValue.encode(running)
                        )
                    )
                )
            }
        }

        let configuration = makeConfiguration(
            restartPolicy: RuntimeRestartPolicy(
                maximumAutomaticRestarts: 1,
                window: 60,
                delays: [0]
            )
        )
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([first, replacement]),
            clock: ManualRuntimeClock(),
            idGenerator: SequenceRuntimeIDGenerator([
                "req_original_state", "req_replacement_state", "req_restore_start",
            ])
        )
        _ = try await controller.connect()
        let initialState = await controller.currentState
        XCTAssertEqual(initialState?.payload.service, .running)

        first.emitExit(status: 1)

        try await eventually {
            let connectionState = await controller.connectionState
            let currentState = await controller.currentState
            return connectionState == .connected
                && currentState?.payload.service == .running
                && currentState?.helperEpoch == "hep_replacement"
        }
        let replacementCommands = try replacement.writtenData.map(requestFromLine).map(\.command)
        XCTAssertEqual(replacementCommands, [.getState, .start])
    }

    func testAutomaticRestartRestoreFailureKeepsReplacementConnected() async throws {
        let first = FakeRuntimeProcess()
        first.onRun { [weak first] in
            first?.emitStandardOutput(try! encodedHello(epoch: "hep_original"))
        }
        first.onWrite { [weak first] data in
            guard let first, let request = try? requestFromLine(data), request.command == .getState else {
                return
            }
            let state = RuntimeStatePayload(
                runtimeGeneration: 7,
                configRevision: "cfg_running",
                service: .running,
                readiness: .ready,
                auth: .signedIn
            )
            first.emitStandardOutput(
                try! encodedResponse(
                    for: request,
                    epoch: "hep_original",
                    result: .object([
                        "state_revision": .integer(1),
                        "state": try! JSONValue.encode(state),
                    ])
                )
            )
        }

        let replacement = FakeRuntimeProcess()
        replacement.onRun { [weak replacement] in
            replacement?.emitStandardOutput(try! encodedHello(epoch: "hep_replacement"))
        }
        replacement.onWrite { [weak replacement] data in
            guard let replacement, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    runtimeGeneration: 0,
                    configRevision: "cfg_running",
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_restore"),
                        ])
                    )
                )
                let error = RuntimeStructuredError(
                    code: "authentication_failed",
                    userMessage: "Provider authentication failed during startup.",
                    retryable: true,
                    recoveryAction: "retry_start"
                )
                let operation = RuntimeOperationEventPayload(
                    operationID: "op_restore",
                    kind: RuntimeOperationKind("start"),
                    phase: .startupAuthentication,
                    status: .failed,
                    runtimeGeneration: 1,
                    error: error
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .operation,
                            helperEpoch: "hep_replacement",
                            stateRevision: 2,
                            payload: try! JSONValue.encode(operation)
                        )
                    )
                )
                let failed = RuntimeStatePayload(
                    runtimeGeneration: 1,
                    configRevision: "cfg_running",
                    lastFailureCode: "authentication_failed",
                    service: .failed,
                    readiness: .notReady,
                    auth: .signedOut
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .state,
                            helperEpoch: "hep_replacement",
                            stateRevision: 3,
                            payload: try! JSONValue.encode(failed)
                        )
                    )
                )
            }
        }

        let controller = RuntimeController(
            configuration: makeConfiguration(
                restartPolicy: RuntimeRestartPolicy(
                    maximumAutomaticRestarts: 1,
                    window: 60,
                    delays: [0]
                )
            ),
            processFactory: FakeRuntimeProcessFactory([first, replacement]),
            clock: ManualRuntimeClock(),
            idGenerator: SequenceRuntimeIDGenerator([
                "req_original_state", "req_replacement_state", "req_restore_start",
            ])
        )
        _ = try await controller.connect()

        first.emitExit(status: 1)

        try await eventually {
            let snapshot = await controller.snapshot()
            return snapshot.connectionState == .connected
                && snapshot.currentState?.helperEpoch == "hep_replacement"
                && snapshot.currentState?.payload.service == .failed
                && snapshot.currentState?.payload.lastFailureCode == "authentication_failed"
        }
        let replacementCommands = try replacement.writtenData.map(requestFromLine).map(\.command)
        XCTAssertEqual(replacementCommands, [.getState, .start])
        XCTAssertEqual(replacement.terminateCount, 0)
        XCTAssertEqual(replacement.forceTerminateCount, 0)
    }

    func testManualRecoveryAndStopClearPendingAutomaticRestoreIntent() async throws {
        let first = FakeRuntimeProcess()
        first.onRun { [weak first] in
            first?.emitStandardOutput(try! encodedHello(epoch: "hep_original"))
        }
        first.onWrite { [weak first] data in
            guard let first, let request = try? requestFromLine(data), request.command == .getState else {
                return
            }
            let state = RuntimeStatePayload(
                runtimeGeneration: 7,
                configRevision: "cfg_running",
                service: .running,
                readiness: .ready,
                auth: .signedIn
            )
            first.emitStandardOutput(
                try! encodedResponse(
                    for: request,
                    epoch: "hep_original",
                    result: .object([
                        "state_revision": .integer(1),
                        "state": try! JSONValue.encode(state),
                    ])
                )
            )
        }

        let replacement = FakeRuntimeProcess()
        replacement.onRun { [weak replacement] in
            replacement?.emitStandardOutput(try! encodedHello(epoch: "hep_replacement"))
        }
        replacement.onWrite { [weak replacement] data in
            guard let replacement, let request = try? requestFromLine(data) else { return }
            switch request.id {
            case "req_replacement_state":
                let state = RuntimeStatePayload(
                    runtimeGeneration: 0,
                    configRevision: "cfg_running",
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            case "req_restore_start":
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_restore"),
                        ])
                    )
                )
                let error = RuntimeStructuredError(
                    code: "authentication_failed",
                    userMessage: "Provider authentication failed during startup.",
                    retryable: true,
                    recoveryAction: "retry_start"
                )
                let operation = RuntimeOperationEventPayload(
                    operationID: "op_restore",
                    kind: RuntimeOperationKind("start"),
                    phase: .startupAuthentication,
                    status: .failed,
                    runtimeGeneration: 1,
                    error: error
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .operation,
                            helperEpoch: "hep_replacement",
                            stateRevision: 2,
                            payload: try! JSONValue.encode(operation)
                        )
                    )
                )
            case "req_manual_start":
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_manual_start"),
                        ])
                    )
                )
                let operation = RuntimeOperationEventPayload(
                    operationID: "op_manual_start",
                    kind: RuntimeOperationKind("start"),
                    phase: .readinessCheck,
                    status: .succeeded,
                    runtimeGeneration: 2
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .operation,
                            helperEpoch: "hep_replacement",
                            stateRevision: 3,
                            payload: try! JSONValue.encode(operation)
                        )
                    )
                )
                replacement.emitStandardOutput(
                    try! self.stateEvent(
                        epoch: "hep_replacement",
                        revision: 4,
                        generation: 2,
                        service: .running
                    )
                )
            case "req_manual_stop":
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_manual_stop"),
                        ])
                    )
                )
                let operation = RuntimeOperationEventPayload(
                    operationID: "op_manual_stop",
                    kind: RuntimeOperationKind("stop"),
                    phase: .cleanup,
                    status: .succeeded,
                    runtimeGeneration: 3
                )
                replacement.emitStandardOutput(
                    try! RuntimeFrameCodec().encodeLine(
                        RuntimeEventEnvelope(
                            version: 1,
                            event: .operation,
                            helperEpoch: "hep_replacement",
                            stateRevision: 5,
                            payload: try! JSONValue.encode(operation)
                        )
                    )
                )
                replacement.emitStandardOutput(
                    try! self.stateEvent(
                        epoch: "hep_replacement",
                        revision: 6,
                        generation: 3,
                        service: .stopped
                    )
                )
            default:
                break
            }
        }

        let final = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: final, epoch: "hep_final")

        let controller = RuntimeController(
            configuration: makeConfiguration(
                restartPolicy: RuntimeRestartPolicy(
                    maximumAutomaticRestarts: 2,
                    window: 60,
                    delays: [0, 0]
                )
            ),
            processFactory: FakeRuntimeProcessFactory([first, replacement, final]),
            clock: ManualRuntimeClock(),
            idGenerator: SequenceRuntimeIDGenerator([
                "req_original_state",
                "req_replacement_state",
                "req_restore_start",
                "req_manual_start",
                "req_manual_stop",
                "req_final_state",
                "req_unwanted_restore",
            ])
        )
        _ = try await controller.connect()

        first.emitExit(status: 1)
        try await eventually {
            let snapshot = await controller.snapshot()
            return snapshot.connectionState == .connected
                && snapshot.operations["op_restore"]?.status == .failed
        }

        let started = try await controller.performOperation(command: .start)
        XCTAssertEqual(started.status, .succeeded)
        try await eventually { await controller.currentState?.payload.service == .running }

        let stopped = try await controller.performOperation(command: .stop)
        XCTAssertEqual(stopped.status, .succeeded)
        try await eventually { await controller.currentState?.payload.service == .stopped }

        replacement.emitExit(status: 1)
        try await eventually {
            let snapshot = await controller.snapshot()
            return snapshot.connectionState == .connected
                && snapshot.currentState?.helperEpoch == "hep_final"
        }

        XCTAssertEqual(
            try final.writtenData.map(requestFromLine).map(\.command),
            [.getState]
        )
    }

    func testAutomaticRestartRestoreHelperExitDoesNotReportConnected() async throws {
        let first = FakeRuntimeProcess()
        first.onRun { [weak first] in
            first?.emitStandardOutput(try! encodedHello(epoch: "hep_original"))
        }
        first.onWrite { [weak first] data in
            guard let first, let request = try? requestFromLine(data), request.command == .getState else {
                return
            }
            let state = RuntimeStatePayload(
                runtimeGeneration: 7,
                configRevision: "cfg_running",
                service: .running,
                readiness: .ready,
                auth: .signedIn
            )
            first.emitStandardOutput(
                try! encodedResponse(
                    for: request,
                    epoch: "hep_original",
                    result: .object([
                        "state_revision": .integer(1),
                        "state": try! JSONValue.encode(state),
                    ])
                )
            )
        }

        let replacement = FakeRuntimeProcess()
        replacement.onRun { [weak replacement] in
            replacement?.emitStandardOutput(try! encodedHello(epoch: "hep_replacement"))
        }
        replacement.onWrite { [weak replacement] data in
            guard let replacement, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    runtimeGeneration: 0,
                    configRevision: "cfg_running",
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                replacement.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        epoch: "hep_replacement",
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_restore"),
                        ])
                    )
                )
            }
        }

        let controller = RuntimeController(
            configuration: makeConfiguration(
                restartPolicy: RuntimeRestartPolicy(
                    maximumAutomaticRestarts: 1,
                    window: 60,
                    delays: [0]
                )
            ),
            processFactory: FakeRuntimeProcessFactory([first, replacement]),
            clock: ManualRuntimeClock(),
            idGenerator: SequenceRuntimeIDGenerator([
                "req_original_state", "req_replacement_state", "req_restore_start",
            ])
        )
        _ = try await controller.connect()

        first.emitExit(status: 1)
        try await eventually { await controller.snapshot().activeOperationID == "op_restore" }
        replacement.emitExit(status: 1)

        try await eventually {
            await controller.connectionState
                == .failed(.restartLimitExceeded(maximum: 1, window: 60))
        }
        let snapshot = await controller.snapshot()
        XCTAssertNil(snapshot.launchIdentity)
        XCTAssertNil(snapshot.currentState)
        XCTAssertEqual(snapshot.operations["op_restore"]?.error?.code, "helper_exited")
    }

    func testPreHandshakeRestartFailureRetainsPreviousScopedIdentity() async throws {
        let first = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: first, epoch: "hep_original")
        let second = FakeRuntimeProcess()
        second.setRunError(RuntimeTestError.synthetic)
        var configuration = makeConfiguration()
        configuration.restartPolicy = RuntimeRestartPolicy(
            maximumAutomaticRestarts: 1,
            window: 60,
            delays: [0]
        )
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([first, second]),
            clock: ManualRuntimeClock()
        )
        _ = try await controller.connect()
        let originalSnapshot = await controller.snapshot()
        let originalIdentity = try XCTUnwrap(originalSnapshot.launchIdentity)
        let stream = await controller.scopedNotificationStream()
        let failedNotification = Task { () -> RuntimeScopedNotification? in
            for await scoped in stream {
                if case .connectionStateChanged(.failed(_)) = scoped.notification {
                    return scoped
                }
            }
            return nil
        }

        first.emitExit(status: 1)
        try await eventually {
            await controller.connectionState
                == .failed(.restartLimitExceeded(maximum: 1, window: 60))
        }

        let failure = await failedNotification.value
        XCTAssertEqual(failure?.launchIdentity, originalIdentity)
        await controller.shutdown()
    }

    func testPostHandshakeRestartStateFailureRetainsPreviousScopedIdentity() async throws {
        let first = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: first, epoch: "hep_original")
        let second = FakeRuntimeProcess()
        second.onRun { [weak second] in
            second?.emitStandardOutput(try! encodedHello(epoch: "hep_replacement"))
        }
        second.onWrite { [weak second] data in
            guard let second, let request = try? requestFromLine(data), request.command == .getState else { return }
            let error = RuntimeStructuredError(
                code: "state_unavailable",
                userMessage: "The replacement state is unavailable.",
                retryable: false
            )
            second.emitStandardOutput(
                try! RuntimeFrameCodec().encodeLine(
                    RuntimeResponseEnvelope(
                        version: 1,
                        id: request.id,
                        helperEpoch: "hep_replacement",
                        ok: false,
                        error: error
                    )
                )
            )
        }
        var configuration = makeConfiguration()
        configuration.restartPolicy = RuntimeRestartPolicy(
            maximumAutomaticRestarts: 1,
            window: 60,
            delays: [0]
        )
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([first, second]),
            clock: ManualRuntimeClock(),
            idGenerator: SequenceRuntimeIDGenerator(["req_original", "req_replacement"])
        )
        _ = try await controller.connect()
        let originalSnapshot = await controller.snapshot()
        let originalIdentity = try XCTUnwrap(originalSnapshot.launchIdentity)
        let stream = await controller.scopedNotificationStream()

        first.emitExit(status: 1)
        try await eventually {
            await controller.connectionState == .failed(.launchPreparationFailed)
        }

        let failure = try await withThrowingTaskGroup(
            of: RuntimeScopedNotification.self
        ) { group in
            group.addTask {
                for await scoped in stream {
                    try Task.checkCancellation()
                    if case .connectionStateChanged(.failed(_)) = scoped.notification {
                        return scoped
                    }
                }
                throw RuntimeTestError.timedOut
            }
            group.addTask {
                try await Task.sleep(nanoseconds: 2_000_000_000)
                throw RuntimeTestError.timedOut
            }
            guard let result = try await group.next() else {
                throw RuntimeTestError.timedOut
            }
            group.cancelAll()
            return result
        }
        XCTAssertEqual(failure.launchIdentity, originalIdentity)
        second.emitExit(status: 1)
        try await eventually { await controller.snapshot().launchIdentity == nil }
        await controller.shutdown()
    }

    func testPostHandshakeRestartPreparationFailurePublishesReplacementStateBeforeFailure() async throws {
        let first = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: first, epoch: "hep_original")
        let second = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: second, epoch: "hep_replacement")
        var configuration = makeConfiguration()
        configuration.restartPolicy = RuntimeRestartPolicy(
            maximumAutomaticRestarts: 1,
            window: 60,
            delays: [0]
        )
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([first, second]),
            clock: ManualRuntimeClock(),
            idGenerator: SequenceRuntimeIDGenerator(["req_original", "req_replacement"]),
            launchPreparation: { context in
                if context.isAutomaticRestart {
                    throw RuntimeTestError.synthetic
                }
                return []
            }
        )
        _ = try await controller.connect()
        let stream = await controller.scopedNotificationStream()

        first.emitExit(status: 1)
        try await eventually {
            await controller.connectionState == .failed(.launchPreparationFailed)
        }

        let snapshot = await controller.snapshot()
        let replacementIdentity = try XCTUnwrap(snapshot.launchIdentity)
        XCTAssertEqual(replacementIdentity.helperEpoch, "hep_replacement")
        XCTAssertEqual(snapshot.currentState?.helperEpoch, "hep_replacement")
        let notifications = try await withThrowingTaskGroup(
            of: [RuntimeScopedNotification].self
        ) { group in
            group.addTask {
                var notifications: [RuntimeScopedNotification] = []
                for await scoped in stream {
                    try Task.checkCancellation()
                    notifications.append(scoped)
                    if case .connectionStateChanged(.failed(.launchPreparationFailed)) = scoped.notification {
                        return notifications
                    }
                }
                return notifications
            }
            group.addTask {
                try await Task.sleep(nanoseconds: 2_000_000_000)
                throw RuntimeTestError.timedOut
            }
            guard let result = try await group.next() else {
                throw RuntimeTestError.timedOut
            }
            group.cancelAll()
            return result
        }
        let replacementStateIndex = try XCTUnwrap(notifications.firstIndex { scoped in
            guard scoped.launchIdentity == replacementIdentity else { return false }
            if case let .state(state) = scoped.notification {
                return state.helperEpoch == "hep_replacement"
            }
            return false
        })
        let failureIndex = try XCTUnwrap(notifications.firstIndex { scoped in
            guard scoped.launchIdentity == replacementIdentity else { return false }
            if case .connectionStateChanged(.failed(.launchPreparationFailed)) = scoped.notification {
                return true
            }
            return false
        })
        XCTAssertLessThan(replacementStateIndex, failureIndex)

        second.emitExit(status: 1)
        try await eventually { await controller.snapshot().launchIdentity == nil }
        await controller.shutdown()
    }

    func testOversizedOutgoingRequestIsRejectedBeforeTransportWrite() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process)
        var configuration = makeConfiguration()
        configuration.maximumFrameSize = 256
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([process])
        )
        _ = try await controller.connect()
        let writesBefore = process.writtenData.count

        do {
            _ = try await controller.send(
                command: .describeConfig,
                payload: .object(["large": .string(String(repeating: "x", count: 512))])
            )
            XCTFail("Expected encoded frame limit failure")
        } catch {
            XCTAssertEqual(error as? RuntimeControllerError, .requestEncodingFailed)
        }
        XCTAssertEqual(process.writtenData.count, writesBefore)
    }

    func testStructuredCommandErrorIsThrownWithoutParsingStrings() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process)
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_error"])
        )
        _ = try await controller.connect()

        let task = Task { try await controller.send(command: .describeConfig) }
        try await eventually { process.writtenData.count == 2 }
        let request = try requestFromLine(process.writtenData[1])
        let structured = RuntimeStructuredError(
            code: "invalid_config",
            userMessage: "The provider configuration is invalid.",
            retryable: false,
            recoveryAction: "open_providers",
            fieldErrors: [RuntimeFieldError(path: "providers[0].base_url", code: "invalid_url", message: "Invalid URL.")]
        )
        process.emitStandardOutput(
            try RuntimeFrameCodec().encodeLine(
                RuntimeResponseEnvelope(
                    version: 1,
                    id: request.id,
                    helperEpoch: "hep_test",
                    ok: false,
                    error: structured
                )
            )
        )

        do {
            _ = try await task.value
            XCTFail("Expected structured error")
        } catch {
            XCTAssertEqual(error as? RuntimeStructuredError, structured)
        }
    }

    func testDuplicatePendingRequestIDIsRejectedWithoutOverwritingCorrelation() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process)
        let controller = RuntimeController(
            configuration: makeConfiguration(),
            processFactory: FakeRuntimeProcessFactory([process]),
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_duplicate", "req_duplicate"])
        )
        _ = try await controller.connect()

        let first = Task { try await controller.send(command: .describeConfig) }
        try await eventually { process.writtenData.count == 2 }

        do {
            _ = try await controller.send(command: RuntimeCommand("another_read"))
            XCTFail("Expected duplicate ID rejection")
        } catch {
            XCTAssertEqual(error as? RuntimeControllerError, .duplicateRequestID("req_duplicate"))
        }

        let request = try requestFromLine(process.writtenData[1])
        process.emitStandardOutput(try encodedResponse(for: request))
        let firstResponse = try await first.value
        XCTAssertEqual(firstResponse.id, "req_duplicate")
        XCTAssertEqual(process.writtenData.count, 2)
    }

    func testMalformedProtocolOutputUsesBoundedAutomaticRestartRatherThanSuppression() async throws {
        let first = FakeRuntimeProcess()
        let second = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: first, epoch: "hep_repeat")
        configureSuccessfulHandshake(process: second, epoch: "hep_repeat")
        let clock = ManualRuntimeClock()
        var configuration = makeConfiguration()
        configuration.handshakeTimeout = 30
        configuration.requestTimeout = 30
        configuration.restartPolicy = RuntimeRestartPolicy(
            maximumAutomaticRestarts: 1,
            window: 60,
            delays: [0]
        )
        let factory = FakeRuntimeProcessFactory([first, second])
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: factory,
            clock: clock,
            idGenerator: SequenceRuntimeIDGenerator(["req_state_1", "req_state_2"])
        )
        _ = try await controller.connect()
        let firstToken = await controller.snapshot().launchIdentity?.launchToken

        first.emitStandardOutput(Data("{}\n".utf8))
        try await eventually { first.terminateCount == 1 }
        first.emitExit(status: 2)

        try await eventually { await controller.connectionState == .connected }
        XCTAssertEqual(factory.receivedConfigurations.count, 2)
        let secondSnapshot = await controller.snapshot()
        XCTAssertEqual(secondSnapshot.helperEpoch, "hep_repeat")
        XCTAssertNotEqual(firstToken, secondSnapshot.launchIdentity?.launchToken)
        first.emitStandardOutput(try stateEvent(epoch: "hep_repeat", revision: 999, generation: 999, service: .failed))
        try? await Task.sleep(nanoseconds: 10_000_000)
        let currentGenerationAfterStaleOutput = await controller.currentState?.payload.runtimeGeneration
        XCTAssertNotEqual(currentGenerationAfterStaleOutput, 999)
        clock.advance(by: 120)
    }

    func testConnectedFailureUsesCooperativeShutdownGraceBeforeForceTermination() async throws {
        let process = FakeRuntimeProcess()
        configureSuccessfulHandshake(process: process)
        let clock = ManualRuntimeClock()
        var configuration = makeConfiguration()
        configuration.shutdownGracePeriod = 11
        configuration.forceTerminationGracePeriod = 2
        configuration.restartPolicy = RuntimeRestartPolicy(maximumAutomaticRestarts: 0)
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([process]),
            clock: clock,
            idGenerator: SequenceRuntimeIDGenerator(["req_state"])
        )
        _ = try await controller.connect()

        process.emitStandardOutput(Data("{}\n".utf8))
        try await eventually { process.terminateCount == 1 }
        try await eventually {
            clock.recordedSleeps.filter { $0 == 11 }.count == 1
        }
        XCTAssertEqual(process.forceTerminateCount, 0)

        clock.advance(by: 11)
        try await eventually { process.forceTerminateCount == 1 }
        process.emitExit(status: 2)
        try await eventually {
            await controller.connectionState
                == .failed(.restartLimitExceeded(maximum: 0, window: 60))
        }
    }

    func testUnresponsiveFailedSessionRetiresActiveOperationAfterForceTerminationTimeout() async throws {
        let process = FakeRuntimeProcess()
        process.onRun { [weak process] in
            process?.emitStandardOutput(try! encodedHello())
        }
        process.onWrite { [weak process] data in
            guard let process, let request = try? requestFromLine(data) else { return }
            if request.command == .getState {
                let state = RuntimeStatePayload(
                    service: .stopped,
                    readiness: .unknown,
                    auth: .signedIn
                )
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "state_revision": .integer(1),
                            "state": try! JSONValue.encode(state),
                        ])
                    )
                )
            } else if request.command == .start {
                process.emitStandardOutput(
                    try! encodedResponse(
                        for: request,
                        result: .object([
                            "accepted": .bool(true),
                            "operation_id": .string("op_start"),
                        ])
                    )
                )
            }
        }

        let clock = ManualRuntimeClock()
        var configuration = makeConfiguration(
            restartPolicy: RuntimeRestartPolicy(maximumAutomaticRestarts: 0)
        )
        configuration.shutdownGracePeriod = 11
        configuration.forceTerminationGracePeriod = 2
        let controller = RuntimeController(
            configuration: configuration,
            processFactory: FakeRuntimeProcessFactory([process]),
            clock: clock,
            idGenerator: SequenceRuntimeIDGenerator(["req_state", "req_start"])
        )
        _ = try await controller.connect()
        let handle = try await controller.submitOperation(command: .start)
        let terminal = Task { try await controller.waitForOperation(id: handle.id) }

        process.emitStandardOutput(Data("{}\n".utf8))
        try await eventually { process.terminateCount == 1 }
        try await eventually { clock.recordedSleeps.contains(11) }
        clock.advance(by: 11)
        try await eventually { process.forceTerminateCount == 1 }
        try await eventually { clock.recordedSleeps.contains(2) }
        clock.advance(by: 2)

        let failed = try await terminal.value
        XCTAssertEqual(failed.status, .failed)
        XCTAssertEqual(failed.error?.code, "helper_exited")
        let snapshot = await controller.snapshot()
        XCTAssertNil(snapshot.activeOperationID)
        XCTAssertEqual(snapshot.operations[handle.id]?.status, .failed)
    }

    private func stateEvent(
        epoch: String,
        revision: UInt64,
        generation: UInt64,
        service: RuntimeServiceLifecycle
    ) throws -> Data {
        let state = RuntimeStatePayload(
            runtimeGeneration: generation,
            configRevision: "cfg_\(generation)",
            service: service,
            readiness: service == .running ? .ready : .unknown,
            auth: .signedIn
        )
        return try RuntimeFrameCodec().encodeLine(
            RuntimeEventEnvelope(
                version: 1,
                event: .state,
                helperEpoch: epoch,
                stateRevision: revision,
                payload: try JSONValue.encode(state)
            )
        )
    }

    private func operationEvent(
        revision: UInt64,
        operationID: String,
        phase: RuntimeOperationPhase,
        status: RuntimeOperationStatus,
        generation: UInt64? = nil
    ) throws -> Data {
        let payload = RuntimeOperationEventPayload(
            operationID: operationID,
            kind: RuntimeOperationKind("start"),
            phase: phase,
            status: status,
            runtimeGeneration: generation
        )
        return try RuntimeFrameCodec().encodeLine(
            RuntimeEventEnvelope(
                version: 1,
                event: .operation,
                helperEpoch: "hep_test",
                stateRevision: revision,
                payload: try JSONValue.encode(payload)
            )
        )
    }
}

extension RuntimeControllerTests {
    func testPackagedHelperIntegrationWhenConfigured() async throws {
        let environment = ProcessInfo.processInfo.environment
        guard let helperPath = environment["VEKIL_TEST_HELPER_PATH"],
              let expectedBuildID = environment["VEKIL_TEST_BUNDLE_BUILD_ID"] else {
            throw XCTSkip("Set VEKIL_TEST_HELPER_PATH and VEKIL_TEST_BUNDLE_BUILD_ID for packaged integration")
        }

        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let controller = RuntimeController(
            configuration: RuntimeControllerConfiguration(
                process: RuntimeProcessConfiguration(
                    executableURL: URL(fileURLWithPath: helperPath),
                    arguments: [
                        "--host", "127.0.0.1", "--port", "0",
                        "--parent-pid", String(ProcessInfo.processInfo.processIdentifier),
                    ],
                    environment: ProcessInfo.processInfo.environment.merging([
                        "HOME": root.appendingPathComponent("home").path,
                        "XDG_CONFIG_HOME": root.appendingPathComponent("home/.config").path,
                    ]) { _, replacement in replacement }
                ),
                expectedBundleBuildID: expectedBuildID,
                restartPolicy: RuntimeRestartPolicy(maximumAutomaticRestarts: 0)
            )
        )
        defer { Task { await controller.shutdown() } }

        do {
            let hello = try await controller.connect()
            XCTAssertEqual(hello.bundleBuildID, expectedBuildID)
            let snapshot = await controller.snapshot()
            XCTAssertEqual(snapshot.connectionState, .connected)
            XCTAssertEqual(snapshot.currentState?.payload.service, .stopped)
            XCTAssertNotNil(snapshot.lastStateRevision)
        } catch {
            let diagnostics = await controller.diagnosticsSnapshot()
            XCTFail("Packaged helper integration failed: \(error); diagnostics=\(diagnostics.text)")
        }
        await controller.shutdown()
    }
}

extension RuntimeControllerTests {
    func testFoundationProcessInheritsEnvironmentWhenConfigurationOmitsIt() async throws {
        let expectedHome = try XCTUnwrap(ProcessInfo.processInfo.environment["HOME"])
        let process = FoundationRuntimeProcess(
            configuration: RuntimeProcessConfiguration(executableURL: URL(fileURLWithPath: "/usr/bin/env"))
        )
        let outputTask = Task { () -> Data in
            var output = Data()
            for await chunk in process.standardOutput { output.append(chunk) }
            return output
        }
        try process.run()
        process.closeStandardInput()
        let output = String(decoding: await outputTask.value, as: UTF8.self)
        XCTAssertTrue(output.split(separator: "\n").contains("HOME=\(expectedHome)"))
    }
}
