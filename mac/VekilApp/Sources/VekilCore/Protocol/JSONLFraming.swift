import Foundation

public enum JSONLFrameError: Error, Sendable, Equatable, LocalizedError {
    case invalidMaximumFrameSize
    case frameTooLarge(limit: Int)
    case unterminatedFrame(byteCount: Int)
    case emptyFrame

    public var errorDescription: String? {
        switch self {
        case .invalidMaximumFrameSize:
            return "The maximum JSONL frame size must be positive."
        case let .frameTooLarge(limit):
            return "A runtime protocol frame exceeded the \(limit)-byte limit."
        case let .unterminatedFrame(byteCount):
            return "The runtime protocol ended with an unterminated \(byteCount)-byte frame."
        case .emptyFrame:
            return "The runtime protocol emitted an empty frame."
        }
    }
}

/// Incrementally separates newline-delimited frames while enforcing the limit
/// before buffering an oversized line. The newline itself is not counted.
public struct JSONLFrameDecoder: Sendable {
    public static let protocolMaximumFrameSize = 1_048_576

    public let maximumFrameSize: Int
    private var partialFrame = Data()

    public init(maximumFrameSize: Int = Self.protocolMaximumFrameSize) throws {
        guard maximumFrameSize > 0 else { throw JSONLFrameError.invalidMaximumFrameSize }
        self.maximumFrameSize = maximumFrameSize
    }

    public var bufferedByteCount: Int { partialFrame.count }

    public mutating func append(_ chunk: Data) throws -> [Data] {
        guard !chunk.isEmpty else { return [] }

        var frames: [Data] = []
        var cursor = chunk.startIndex

        while cursor < chunk.endIndex {
            let remainder = chunk[cursor...]
            if let newline = remainder.firstIndex(of: 0x0A) {
                let segment = chunk[cursor..<newline]
                try appendSegment(segment)

                if partialFrame.last == 0x0D {
                    partialFrame.removeLast()
                }
                guard !partialFrame.isEmpty else { throw JSONLFrameError.emptyFrame }

                frames.append(partialFrame)
                partialFrame.removeAll(keepingCapacity: true)
                cursor = chunk.index(after: newline)
            } else {
                try appendSegment(remainder)
                cursor = chunk.endIndex
            }
        }

        return frames
    }

    public mutating func finish() throws {
        guard partialFrame.isEmpty else {
            throw JSONLFrameError.unterminatedFrame(byteCount: partialFrame.count)
        }
    }

    public mutating func reset() {
        partialFrame.removeAll(keepingCapacity: false)
    }

    private mutating func appendSegment<S: DataProtocol>(_ segment: S) throws {
        guard partialFrame.count <= maximumFrameSize - segment.count else {
            partialFrame.removeAll(keepingCapacity: false)
            throw JSONLFrameError.frameTooLarge(limit: maximumFrameSize)
        }
        partialFrame.append(contentsOf: segment)
    }
}

public enum RuntimeJSONValidationError: Error, Sendable, Equatable, LocalizedError {
    case invalidUTF8
    case malformedJSON
    case duplicateObjectKey(String)
    case topLevelValueMustBeObject

    public var errorDescription: String? {
        switch self {
        case .invalidUTF8:
            return "The runtime protocol frame is not valid UTF-8."
        case .malformedJSON:
            return "The runtime protocol frame is malformed JSON."
        case let .duplicateObjectKey(key):
            return "The runtime protocol frame contains a duplicate object key: \(key)."
        case .topLevelValueMustBeObject:
            return "The runtime protocol frame must contain a JSON object."
        }
    }
}

/// Validates JSON syntax and rejects duplicate keys at every object depth.
/// `JSONDecoder` alone intentionally accepts duplicate keys, which is unsafe for
/// a command protocol because different implementations may choose different values.
enum RuntimeStrictJSONValidator {
    static func validateObject(_ data: Data) throws {
        guard String(data: data, encoding: .utf8) != nil else {
            throw RuntimeJSONValidationError.invalidUTF8
        }

        var parser = Parser(bytes: Array(data))
        parser.skipWhitespace()
        guard parser.peek == UInt8(ascii: "{") else {
            throw RuntimeJSONValidationError.topLevelValueMustBeObject
        }
        try parser.parseValue()
        parser.skipWhitespace()
        guard parser.isAtEnd else { throw RuntimeJSONValidationError.malformedJSON }
    }

    private struct Parser {
        let bytes: [UInt8]
        var index = 0

        var isAtEnd: Bool { index == bytes.count }
        var peek: UInt8? { isAtEnd ? nil : bytes[index] }

        mutating func skipWhitespace() {
            while let byte = peek, byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D {
                index += 1
            }
        }

        mutating func parseValue() throws {
            skipWhitespace()
            guard let byte = peek else { throw RuntimeJSONValidationError.malformedJSON }

            switch byte {
            case UInt8(ascii: "{"):
                try parseObject()
            case UInt8(ascii: "["):
                try parseArray()
            case UInt8(ascii: "\""):
                _ = try parseString()
            case UInt8(ascii: "t"):
                try consumeLiteral("true")
            case UInt8(ascii: "f"):
                try consumeLiteral("false")
            case UInt8(ascii: "n"):
                try consumeLiteral("null")
            case UInt8(ascii: "-"), UInt8(ascii: "0")...UInt8(ascii: "9"):
                try parseNumber()
            default:
                throw RuntimeJSONValidationError.malformedJSON
            }
        }

        mutating func parseObject() throws {
            try consume(UInt8(ascii: "{"))
            skipWhitespace()
            if peek == UInt8(ascii: "}") {
                index += 1
                return
            }

            var keys = Set<String>()
            while true {
                skipWhitespace()
                let key = try parseString()
                guard keys.insert(key).inserted else {
                    throw RuntimeJSONValidationError.duplicateObjectKey(key)
                }

                skipWhitespace()
                try consume(UInt8(ascii: ":"))
                try parseValue()
                skipWhitespace()

                if peek == UInt8(ascii: "}") {
                    index += 1
                    return
                }
                try consume(UInt8(ascii: ","))
            }
        }

        mutating func parseArray() throws {
            try consume(UInt8(ascii: "["))
            skipWhitespace()
            if peek == UInt8(ascii: "]") {
                index += 1
                return
            }

            while true {
                try parseValue()
                skipWhitespace()
                if peek == UInt8(ascii: "]") {
                    index += 1
                    return
                }
                try consume(UInt8(ascii: ","))
            }
        }

        mutating func parseString() throws -> String {
            let start = index
            try consume(UInt8(ascii: "\""))
            var escaped = false

            while let byte = peek {
                if escaped {
                    switch byte {
                    case UInt8(ascii: "\""), UInt8(ascii: "\\"), UInt8(ascii: "/"),
                         UInt8(ascii: "b"), UInt8(ascii: "f"), UInt8(ascii: "n"),
                         UInt8(ascii: "r"), UInt8(ascii: "t"):
                        index += 1
                    case UInt8(ascii: "u"):
                        index += 1
                        guard index + 4 <= bytes.count else {
                            throw RuntimeJSONValidationError.malformedJSON
                        }
                        for offset in 0..<4 where !Self.isHex(bytes[index + offset]) {
                            throw RuntimeJSONValidationError.malformedJSON
                        }
                        index += 4
                    default:
                        throw RuntimeJSONValidationError.malformedJSON
                    }
                    escaped = false
                    continue
                }

                if byte == UInt8(ascii: "\\") {
                    escaped = true
                    index += 1
                } else if byte == UInt8(ascii: "\"") {
                    index += 1
                    let token = Data(bytes[start..<index])
                    do {
                        return try JSONDecoder().decode(String.self, from: token)
                    } catch {
                        throw RuntimeJSONValidationError.malformedJSON
                    }
                } else if byte < 0x20 {
                    throw RuntimeJSONValidationError.malformedJSON
                } else {
                    index += 1
                }
            }

            throw RuntimeJSONValidationError.malformedJSON
        }

        mutating func parseNumber() throws {
            if peek == UInt8(ascii: "-") { index += 1 }
            guard let first = peek else { throw RuntimeJSONValidationError.malformedJSON }

            if first == UInt8(ascii: "0") {
                index += 1
                if let next = peek, Self.isDigit(next) {
                    throw RuntimeJSONValidationError.malformedJSON
                }
            } else {
                guard first >= UInt8(ascii: "1"), first <= UInt8(ascii: "9") else {
                    throw RuntimeJSONValidationError.malformedJSON
                }
                while let byte = peek, Self.isDigit(byte) { index += 1 }
            }

            if peek == UInt8(ascii: ".") {
                index += 1
                guard let digit = peek, Self.isDigit(digit) else {
                    throw RuntimeJSONValidationError.malformedJSON
                }
                while let byte = peek, Self.isDigit(byte) { index += 1 }
            }

            if peek == UInt8(ascii: "e") || peek == UInt8(ascii: "E") {
                index += 1
                if peek == UInt8(ascii: "+") || peek == UInt8(ascii: "-") { index += 1 }
                guard let digit = peek, Self.isDigit(digit) else {
                    throw RuntimeJSONValidationError.malformedJSON
                }
                while let byte = peek, Self.isDigit(byte) { index += 1 }
            }
        }

        mutating func consumeLiteral(_ literal: String) throws {
            let literalBytes = Array(literal.utf8)
            guard index + literalBytes.count <= bytes.count,
                  Array(bytes[index..<(index + literalBytes.count)]) == literalBytes else {
                throw RuntimeJSONValidationError.malformedJSON
            }
            index += literalBytes.count
        }

        mutating func consume(_ expected: UInt8) throws {
            guard peek == expected else { throw RuntimeJSONValidationError.malformedJSON }
            index += 1
        }

        static func isDigit(_ byte: UInt8) -> Bool {
            byte >= UInt8(ascii: "0") && byte <= UInt8(ascii: "9")
        }

        static func isHex(_ byte: UInt8) -> Bool {
            isDigit(byte)
                || (byte >= UInt8(ascii: "a") && byte <= UInt8(ascii: "f"))
                || (byte >= UInt8(ascii: "A") && byte <= UInt8(ascii: "F"))
        }
    }
}
