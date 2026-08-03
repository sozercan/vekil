import XCTest

@testable import VekilUI

final class StatusMenuTests: XCTestCase {
    func testStoppedMenuKeepsOnlyQuickControlsVisible() {
        let descriptors = makeCompactStatusMenuDescriptors(
            summaryTitle: "Proxy Stopped · Copilot default",
            hasBaseURL: false
        )

        XCTAssertEqual(
            visibleLayout(descriptors),
            [
                "Proxy Stopped · Copilot default",
                "<separator>",
                "Start Proxy",
                "Open Vekil…",
                "Settings…",
                "<separator>",
                "Quit Vekil",
            ]
        )

        let settingsItem = try! XCTUnwrap(descriptors.first { $0.title == "Settings…" })
        XCTAssertEqual(settingsItem.action, .settings)
        XCTAssertEqual(settingsItem.tag, 12)
        XCTAssertTrue(settingsItem.isEnabled)
        XCTAssertFalse(settingsItem.isHidden)

        let copyItem = try! XCTUnwrap(descriptors.first { $0.action == .copyBaseURL })
        XCTAssertTrue(copyItem.isHidden)
        XCTAssertFalse(copyItem.isEnabled)
    }

    func testRunningMenuRevealsContextualCopyActionAndWarning() {
        let descriptors = makeCompactStatusMenuDescriptors(
            summaryTitle: "Proxy Ready · Copilot default",
            warningTitle: "Configuration changed on disk.",
            primaryTitle: "Stop Proxy",
            primaryEnabled: true,
            hasBaseURL: true
        )

        XCTAssertEqual(
            visibleLayout(descriptors),
            [
                "Proxy Ready · Copilot default",
                "Configuration changed on disk.",
                "<separator>",
                "Stop Proxy",
                "Open Vekil…",
                "Settings…",
                "Copy Base URL",
                "<separator>",
                "Quit Vekil",
            ]
        )

        let primaryItem = try! XCTUnwrap(descriptors.first { $0.action == .primary })
        XCTAssertEqual(primaryItem.tag, 3)
        XCTAssertTrue(primaryItem.isEnabled)

        let copyItem = try! XCTUnwrap(descriptors.first { $0.action == .copyBaseURL })
        XCTAssertEqual(copyItem.tag, 4)
        XCTAssertFalse(copyItem.isHidden)
        XCTAssertTrue(copyItem.isEnabled)
    }

    private func visibleLayout(_ descriptors: [CompactStatusMenuItemDescriptor]) -> [String] {
        descriptors.compactMap { descriptor in
            guard !descriptor.isHidden else { return nil }
            return descriptor.isSeparator ? "<separator>" : descriptor.title
        }
    }
}
