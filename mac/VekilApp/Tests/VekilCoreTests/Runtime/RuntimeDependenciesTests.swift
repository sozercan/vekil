import Foundation
import XCTest
@testable import VekilCore

final class RuntimeDependenciesTests: XCTestCase {
    func testNanosecondConversionClampsWithoutOverflow() {
        XCTAssertEqual(SystemRuntimeClock.nanoseconds(for: -1), 0)
        XCTAssertEqual(SystemRuntimeClock.nanoseconds(for: .nan), 0)
        XCTAssertEqual(SystemRuntimeClock.nanoseconds(for: 1.5), 1_500_000_000)
        XCTAssertEqual(SystemRuntimeClock.nanoseconds(for: .infinity), UInt64.max)
        XCTAssertEqual(SystemRuntimeClock.nanoseconds(for: .greatestFiniteMagnitude), UInt64.max)

        let maximumWholeSeconds = TimeInterval(UInt64.max / 1_000_000_000)
        XCTAssertEqual(SystemRuntimeClock.nanoseconds(for: maximumWholeSeconds), UInt64.max)
        XCTAssertLessThan(SystemRuntimeClock.nanoseconds(for: maximumWholeSeconds.nextDown), UInt64.max)
    }

    func testInfiniteSleepIsCancellable() async {
        let task = Task {
            try await SystemRuntimeClock().sleep(for: .infinity)
        }
        task.cancel()

        do {
            try await task.value
            XCTFail("infinite sleep completed without cancellation")
        } catch is CancellationError {
        } catch {
            XCTFail("infinite sleep failed with unexpected error: \(error)")
        }
    }
}
