import Foundation

public struct RuntimeDiagnosticsSnapshot: Sendable, Equatable {
    public var text: String
    public var retainedByteCount: Int
    public var droppedByteCount: Int
    public var totalByteCount: Int

    public init(
        text: String,
        retainedByteCount: Int,
        droppedByteCount: Int,
        totalByteCount: Int
    ) {
        self.text = text
        self.retainedByteCount = retainedByteCount
        self.droppedByteCount = droppedByteCount
        self.totalByteCount = totalByteCount
    }
}

struct BoundedRuntimeDiagnostics: Sendable {
    let capacity: Int
    private(set) var retained = Data()
    private(set) var droppedByteCount = 0
    private(set) var totalByteCount = 0

    init(capacity: Int) {
        self.capacity = max(0, capacity)
    }

    mutating func append(_ data: Data) {
        guard !data.isEmpty else { return }
        totalByteCount += data.count

        guard capacity > 0 else {
            droppedByteCount += data.count
            return
        }

        if data.count >= capacity {
            droppedByteCount += retained.count + data.count - capacity
            retained = data.suffix(capacity)
            return
        }

        let overflow = retained.count + data.count - capacity
        if overflow > 0 {
            retained.removeFirst(overflow)
            droppedByteCount += overflow
        }
        retained.append(data)
    }

    mutating func reset() {
        retained.removeAll(keepingCapacity: false)
        droppedByteCount = 0
        totalByteCount = 0
    }

    func snapshot() -> RuntimeDiagnosticsSnapshot {
        RuntimeDiagnosticsSnapshot(
            text: String(decoding: retained, as: UTF8.self),
            retainedByteCount: retained.count,
            droppedByteCount: droppedByteCount,
            totalByteCount: totalByteCount
        )
    }
}
