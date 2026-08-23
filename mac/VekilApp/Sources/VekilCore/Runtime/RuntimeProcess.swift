import Foundation

public struct RuntimeProcessConfiguration: Sendable, Equatable {
    public var executableURL: URL
    public var arguments: [String]
    public var environment: [String: String]?
    public var currentDirectoryURL: URL?

    public init(
        executableURL: URL,
        arguments: [String] = [],
        environment: [String: String]? = nil,
        currentDirectoryURL: URL? = nil
    ) {
        self.executableURL = executableURL
        self.arguments = arguments
        self.environment = environment
        self.currentDirectoryURL = currentDirectoryURL
    }
}

public enum RuntimeProcessTerminationReason: Sendable, Equatable {
    case exit
    case uncaughtSignal
}

public struct RuntimeProcessTermination: Sendable, Equatable {
    public var status: Int32
    public var reason: RuntimeProcessTerminationReason

    public init(status: Int32, reason: RuntimeProcessTerminationReason) {
        self.status = status
        self.reason = reason
    }
}

/// Injectable process/transport seam used by `RuntimeController`.
/// Implementations must make all three streams available before `run()` and
/// make `writeStandardInput` asynchronous so pipe backpressure cannot block the controller actor.
public protocol RuntimeProcess: AnyObject, Sendable {
    var standardOutput: AsyncStream<Data> { get }
    var standardError: AsyncStream<Data> { get }
    var termination: AsyncStream<RuntimeProcessTermination> { get }
    var processIdentifier: Int32? { get }

    func run() throws
    func writeStandardInput(_ data: Data) async throws
    func closeStandardInput()
    func terminate()
    func forceTerminate()
}

extension RuntimeProcess {
    public var processIdentifier: Int32? { nil }
}

public protocol RuntimeProcessFactory: Sendable {
    func makeProcess(configuration: RuntimeProcessConfiguration) throws -> any RuntimeProcess
}

public enum RuntimeHelperLocator {
    public static let relativePath = "Contents/Helpers/vekil-runtime"

    public static func bundledHelperURL(in bundle: Bundle = .main) -> URL {
        bundle.bundleURL.appendingPathComponent(relativePath, isDirectory: false)
    }
}
