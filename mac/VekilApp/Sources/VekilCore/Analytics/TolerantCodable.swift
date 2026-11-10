import Foundation

/// Internal decoding helpers for compatibility payloads owned by the Go proxy.
/// Unknown keys are ignored by Codable, while missing, null, or type-mismatched
/// optional fields fall back to safe defaults instead of invalidating an entire
/// analytics snapshot.
internal struct LossyArray<Element: Codable>: Codable {
    let elements: [Element]

    init(_ elements: [Element]) {
        self.elements = elements
    }

    init(from decoder: Decoder) throws {
        var container = try decoder.unkeyedContainer()
        var decoded: [Element] = []
        decoded.reserveCapacity(container.count ?? 0)

        while !container.isAtEnd {
            do {
                decoded.append(try container.decode(Element.self))
            } catch {
                // A malformed row should not hide otherwise valid bounded rows.
                // Consume the value recursively so decoding can continue.
                guard (try? container.decode(DiscardedJSONValue.self)) != nil else {
                    break
                }
            }
        }
        elements = decoded
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.unkeyedContainer()
        for element in elements {
            try container.encode(element)
        }
    }
}

private struct DiscardedJSONValue: Decodable {
    init(from decoder: Decoder) throws {
        if var container = try? decoder.unkeyedContainer() {
            while !container.isAtEnd {
                _ = try container.decode(DiscardedJSONValue.self)
            }
            return
        }

        if let container = try? decoder.container(keyedBy: AnyCodingKey.self) {
            for key in container.allKeys {
                _ = try container.decode(DiscardedJSONValue.self, forKey: key)
            }
            return
        }

        let container = try decoder.singleValueContainer()
        if container.decodeNil() { return }
        if (try? container.decode(Bool.self)) != nil { return }
        if (try? container.decode(Int64.self)) != nil { return }
        if (try? container.decode(Double.self)) != nil { return }
        if (try? container.decode(String.self)) != nil { return }
        throw DecodingError.dataCorruptedError(in: container, debugDescription: "Unsupported JSON value")
    }
}

private struct AnyCodingKey: CodingKey {
    let stringValue: String
    let intValue: Int?

    init?(stringValue: String) {
        self.stringValue = stringValue
        intValue = nil
    }

    init?(intValue: Int) {
        stringValue = String(intValue)
        self.intValue = intValue
    }
}

private struct LossyInt64: Decodable {
    let value: Int64

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if let value = try? container.decode(Int64.self) {
            self.value = value
            return
        }
        if let value = try? container.decode(Int.self) {
            self.value = Int64(value)
            return
        }
        if let value = try? container.decode(Double.self),
           value.isFinite,
           value.rounded(.towardZero) == value,
           value >= Double(Int64.min),
           value <= Double(Int64.max) {
            self.value = Int64(value)
            return
        }
        if let value = try? container.decode(String.self),
           let parsed = Int64(value.trimmingCharacters(in: .whitespacesAndNewlines)) {
            self.value = parsed
            return
        }
        throw DecodingError.dataCorruptedError(in: container, debugDescription: "Expected an integer")
    }
}

private struct LossyBool: Decodable {
    let value: Bool

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if let value = try? container.decode(Bool.self) {
            self.value = value
            return
        }
        if let value = try? container.decode(Int.self) {
            switch value {
            case 0: self.value = false
            case 1: self.value = true
            default:
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "Expected a boolean")
            }
            return
        }
        if let value = try? container.decode(String.self) {
            switch value.trimmingCharacters(in: .whitespacesAndNewlines).lowercased() {
            case "true", "1": self.value = true
            case "false", "0": self.value = false
            default:
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "Expected a boolean")
            }
            return
        }
        throw DecodingError.dataCorruptedError(in: container, debugDescription: "Expected a boolean")
    }
}

private struct LossyInt64Dictionary: Decodable {
    let values: [String: Int64]

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: AnyCodingKey.self)
        var decoded: [String: Int64] = [:]
        decoded.reserveCapacity(container.allKeys.count)
        for key in container.allKeys {
            if let value = try? container.decode(LossyInt64.self, forKey: key).value {
                decoded[key.stringValue] = value
            }
        }
        values = decoded
    }
}

internal extension KeyedDecodingContainer {
    func decodeDefault<T: Decodable>(
        _ type: T.Type,
        forKey key: Key,
        defaultValue: @autoclosure () -> T
    ) -> T {
        guard let value = try? decode(type, forKey: key) else { return defaultValue() }
        return value
    }

    func decodeLossyArray<T: Codable>(_ type: T.Type, forKey key: Key) -> [T] {
        guard let value = try? decode(LossyArray<T>.self, forKey: key) else { return [] }
        return value.elements
    }

    func decodeLossyInt64(forKey key: Key, defaultValue: Int64 = 0) -> Int64 {
        guard let value = try? decode(LossyInt64.self, forKey: key) else { return defaultValue }
        return value.value
    }

    func decodeLossyInt(forKey key: Key, defaultValue: Int = 0) -> Int {
        let decoded = decodeLossyInt64(forKey: key, defaultValue: Int64(defaultValue))
        guard decoded >= Int64(Int.min), decoded <= Int64(Int.max) else { return defaultValue }
        return Int(decoded)
    }

    func decodeLossyOptionalInt64(forKey key: Key) -> Int64? {
        guard let value = try? decode(LossyInt64.self, forKey: key) else { return nil }
        return value.value
    }

    func decodeLossyOptionalInt(forKey key: Key) -> Int? {
        guard let value = decodeLossyOptionalInt64(forKey: key),
              value >= Int64(Int.min), value <= Int64(Int.max) else {
            return nil
        }
        return Int(value)
    }

    func decodeLossyBool(forKey key: Key, defaultValue: Bool = false) -> Bool {
        guard let value = try? decode(LossyBool.self, forKey: key) else { return defaultValue }
        return value.value
    }

    func decodeLossyOptionalBool(forKey key: Key) -> Bool? {
        guard let value = try? decode(LossyBool.self, forKey: key) else { return nil }
        return value.value
    }

    func decodeLossyInt64Dictionary(forKey key: Key) -> [String: Int64] {
        guard let value = try? decode(LossyInt64Dictionary.self, forKey: key) else { return [:] }
        return value.values
    }
}
