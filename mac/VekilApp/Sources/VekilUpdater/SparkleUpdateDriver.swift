import AppKit
import Sparkle
import VekilCore

@MainActor
public final class SparkleUpdateDriver: UpdaterService {
    private let controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
    public init() {}
    public var isAvailable: Bool { controller.updater.canCheckForUpdates }
    public func checkForUpdates() async throws {
        // Vekil launches as an accessory app, so Sparkle's update windows would
        // otherwise open behind the frontmost application.
        NSApp.activate(ignoringOtherApps: true)
        controller.checkForUpdates(nil)
    }
}
