import Darwin
import Foundation

public struct FoundationRuntimeProcessFactory: RuntimeProcessFactory {
    public init() {}

    public func makeProcess(configuration: RuntimeProcessConfiguration) throws -> any RuntimeProcess {
        FoundationRuntimeProcess(configuration: configuration)
    }
}

/// `Process` adapter that installs stdout/stderr readability handlers and the
/// termination handler during initialization, before the child is launched.
public final class FoundationRuntimeProcess: RuntimeProcess, @unchecked Sendable {
    public let standardOutput: AsyncStream<Data>
    public let standardError: AsyncStream<Data>
    public let termination: AsyncStream<RuntimeProcessTermination>

    private let process: Process
    private let inputPipe: Pipe
    private let outputPipe: Pipe
    private let errorPipe: Pipe
    private let inputQueue = DispatchQueue(label: "com.vekil.runtime.stdin")
    private let outputContinuation: AsyncStream<Data>.Continuation
    private let errorContinuation: AsyncStream<Data>.Continuation
    private let terminationContinuation: AsyncStream<RuntimeProcessTermination>.Continuation
    private let lock = NSLock()
    private var didRun = false
    private var inputClosed = false

    public var processIdentifier: Int32? {
        lock.lock()
        defer { lock.unlock() }
        let identifier = process.processIdentifier
        return didRun && identifier > 0 ? identifier : nil
    }

    public init(configuration: RuntimeProcessConfiguration) {
        process = Process()
        inputPipe = Pipe()
        outputPipe = Pipe()
        errorPipe = Pipe()

        var outputContinuation: AsyncStream<Data>.Continuation!
        standardOutput = AsyncStream<Data> { outputContinuation = $0 }
        let outputStreamContinuation = outputContinuation!
        self.outputContinuation = outputStreamContinuation

        var errorContinuation: AsyncStream<Data>.Continuation!
        standardError = AsyncStream<Data>(bufferingPolicy: .bufferingNewest(64)) {
            errorContinuation = $0
        }
        let errorStreamContinuation = errorContinuation!
        self.errorContinuation = errorStreamContinuation

        var terminationContinuation: AsyncStream<RuntimeProcessTermination>.Continuation!
        termination = AsyncStream<RuntimeProcessTermination>(bufferingPolicy: .bufferingNewest(1)) {
            terminationContinuation = $0
        }
        let processTerminationContinuation = terminationContinuation!
        self.terminationContinuation = processTerminationContinuation

        process.executableURL = configuration.executableURL
        process.arguments = configuration.arguments
        if let environment = configuration.environment {
            process.environment = environment
        }
        process.currentDirectoryURL = configuration.currentDirectoryURL
        process.standardInput = inputPipe
        process.standardOutput = outputPipe
        process.standardError = errorPipe

        let outputHandle = outputPipe.fileHandleForReading
        outputHandle.readabilityHandler = { handle in
            let data = handle.availableData
            if data.isEmpty {
                handle.readabilityHandler = nil
                outputStreamContinuation.finish()
            } else {
                outputStreamContinuation.yield(data)
            }
        }

        let errorHandle = errorPipe.fileHandleForReading
        errorHandle.readabilityHandler = { handle in
            let data = handle.availableData
            if data.isEmpty {
                handle.readabilityHandler = nil
                errorStreamContinuation.finish()
            } else {
                errorStreamContinuation.yield(data)
            }
        }

        process.terminationHandler = { process in
            let reason: RuntimeProcessTerminationReason = process.terminationReason == .exit
                ? .exit
                : .uncaughtSignal
            processTerminationContinuation.yield(
                RuntimeProcessTermination(status: process.terminationStatus, reason: reason)
            )
            processTerminationContinuation.finish()
        }
    }

    public func run() throws {
        lock.lock()
        guard !didRun else {
            lock.unlock()
            throw FoundationRuntimeProcessError.alreadyRun
        }
        didRun = true
        lock.unlock()

        do {
            try process.run()
        } catch {
            finishStreamsAfterLaunchFailure()
            throw error
        }
    }

    public func writeStandardInput(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            inputQueue.async { [weak self] in
                guard let self else {
                    continuation.resume(throwing: FoundationRuntimeProcessError.notRunning)
                    return
                }
                self.lock.lock()
                guard self.didRun else {
                    self.lock.unlock()
                    continuation.resume(throwing: FoundationRuntimeProcessError.notRunning)
                    return
                }
                guard !self.inputClosed else {
                    self.lock.unlock()
                    continuation.resume(throwing: FoundationRuntimeProcessError.standardInputClosed)
                    return
                }
                let handle = self.inputPipe.fileHandleForWriting
                self.lock.unlock()
                do {
                    try handle.write(contentsOf: data)
                    continuation.resume(returning: ())
                } catch {
                    continuation.resume(throwing: error)
                }
            }
        }
    }

    public func closeStandardInput() {
        lock.lock()
        guard !inputClosed else {
            lock.unlock()
            return
        }
        inputClosed = true
        lock.unlock()
        try? inputPipe.fileHandleForWriting.close()
    }

    public func terminate() {
        guard process.isRunning else { return }
        let pid = process.processIdentifier
        if kill(-pid, SIGTERM) != 0 { process.terminate() }
    }

    public func forceTerminate() {
        guard process.isRunning else { return }
        let pid = process.processIdentifier
        if kill(-pid, SIGKILL) != 0 { kill(pid, SIGKILL) }
    }

    deinit {
        outputPipe.fileHandleForReading.readabilityHandler = nil
        errorPipe.fileHandleForReading.readabilityHandler = nil
        closeStandardInput()
        outputContinuation.finish()
        errorContinuation.finish()
        terminationContinuation.finish()
    }

    private func finishStreamsAfterLaunchFailure() {
        outputPipe.fileHandleForReading.readabilityHandler = nil
        errorPipe.fileHandleForReading.readabilityHandler = nil
        outputContinuation.finish()
        errorContinuation.finish()
        terminationContinuation.finish()
        closeStandardInput()
    }
}

public enum FoundationRuntimeProcessError: Error, Sendable, Equatable {
    case alreadyRun
    case notRunning
    case standardInputClosed
}
