import Foundation
@testable import VekilCore

func fixtureData(_ name: String) throws -> Data {
    let testDirectory = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
    return try Data(contentsOf: testDirectory.appendingPathComponent("Fixtures").appendingPathComponent(name))
}

func decodeStatsFixture(_ name: String) throws -> StatsSnapshot {
    try JSONDecoder().decode(StatsSnapshot.self, from: fixtureData(name))
}

func makeCapture(
    fixture name: String,
    generation: UInt64,
    runOrdinal: UInt64 = 1,
    sequence: UInt64,
    capturedAt: Date = Date(timeIntervalSince1970: 1_718_900_500)
) throws -> StatsCapture {
    StatsCapture(
        scope: StatsSnapshotScope(runtimeGeneration: generation, runOrdinal: runOrdinal, snapshotSequence: sequence),
        snapshot: try decodeStatsFixture(name),
        capturedAt: capturedAt
    )
}
