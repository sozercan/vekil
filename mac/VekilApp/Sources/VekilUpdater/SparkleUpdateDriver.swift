import Sparkle
import VekilCore

@MainActor
public final class SparkleUpdateDriver: UpdaterService {
    private let controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
    public init() {}
    public var isAvailable: Bool { controller.updater.canCheckForUpdates }
    public func checkForUpdates() async throws { controller.checkForUpdates(nil) }
}
