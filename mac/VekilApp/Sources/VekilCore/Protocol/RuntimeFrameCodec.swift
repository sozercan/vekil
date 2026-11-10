import Foundation

public enum RuntimeFrameCodecError: Error, Sendable, Equatable, LocalizedError {
    case ambiguousEnvelope
    case unrecognizedEnvelope
    case invalidEnvelope(String)
    case encodedFrameTooLarge(limit: Int)

    public var errorDescription: String? {
        switch self {
        case .ambiguousEnvelope:
            return "The runtime frame matches more than one envelope type."
        case .unrecognizedEnvelope:
            return "The runtime frame is not a request, response, or event envelope."
        case let .invalidEnvelope(message):
            return "The runtime frame contains an invalid envelope: \(message)"
        case let .encodedFrameTooLarge(limit):
            return "The encoded runtime frame exceeded the \(limit)-byte limit."
        }
    }
}

/// Strict encoder/decoder for one JSON object, without the trailing JSONL newline.
public struct RuntimeFrameCodec: Sendable {
    public let maximumFrameSize: Int

    public init(maximumFrameSize: Int = JSONLFrameDecoder.protocolMaximumFrameSize) {
        self.maximumFrameSize = maximumFrameSize
    }

    public func decode(_ frame: Data) throws -> RuntimeWireFrame {
        guard !frame.isEmpty else { throw JSONLFrameError.emptyFrame }
        guard frame.count <= maximumFrameSize else {
            throw JSONLFrameError.frameTooLarge(limit: maximumFrameSize)
        }

        try RuntimeStrictJSONValidator.validateObject(frame)

        let discriminator: EnvelopeDiscriminator
        do {
            discriminator = try JSONDecoder().decode(EnvelopeDiscriminator.self, from: frame)
        } catch {
            throw RuntimeJSONValidationError.malformedJSON
        }

        let isRequest = discriminator.command != nil
        let isResponse = discriminator.ok != nil
        let isEvent = discriminator.event != nil
        let matchCount = [isRequest, isResponse, isEvent].filter { $0 }.count

        guard matchCount == 1 else {
            throw matchCount == 0 ? RuntimeFrameCodecError.unrecognizedEnvelope : .ambiguousEnvelope
        }

        do {
            if isRequest {
                guard discriminator.id != nil else {
                    throw RuntimeFrameCodecError.invalidEnvelope("request is missing id")
                }
                return .request(try JSONDecoder().decode(RuntimeRequestEnvelope.self, from: frame))
            }

            if isResponse {
                guard discriminator.id != nil else {
                    throw RuntimeFrameCodecError.invalidEnvelope("response is missing id")
                }
                let response = try JSONDecoder().decode(RuntimeResponseEnvelope.self, from: frame)
                try response.validate()
                return .response(response)
            }

            return .event(try JSONDecoder().decode(RuntimeEventEnvelope.self, from: frame))
        } catch let error as RuntimeFrameCodecError {
            throw error
        } catch let error as RuntimeEnvelopeValidationError {
            throw RuntimeFrameCodecError.invalidEnvelope(String(describing: error))
        } catch {
            throw RuntimeFrameCodecError.invalidEnvelope(String(describing: error))
        }
    }

    public func encode(_ envelope: RuntimeRequestEnvelope) throws -> Data {
        try encodeValue(envelope)
    }

    public func encode(_ envelope: RuntimeResponseEnvelope) throws -> Data {
        try envelope.validate()
        return try encodeValue(envelope)
    }

    public func encode(_ envelope: RuntimeEventEnvelope) throws -> Data {
        try encodeValue(envelope)
    }

    public func encodeLine(_ envelope: RuntimeRequestEnvelope) throws -> Data {
        var data = try encode(envelope)
        data.append(0x0A)
        return data
    }

    public func encodeLine(_ envelope: RuntimeResponseEnvelope) throws -> Data {
        var data = try encode(envelope)
        data.append(0x0A)
        return data
    }

    public func encodeLine(_ envelope: RuntimeEventEnvelope) throws -> Data {
        var data = try encode(envelope)
        data.append(0x0A)
        return data
    }

    private func encodeValue<T: Encodable>(_ value: T) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(value)
        guard data.count <= maximumFrameSize else {
            throw RuntimeFrameCodecError.encodedFrameTooLarge(limit: maximumFrameSize)
        }
        return data
    }

    private struct EnvelopeDiscriminator: Decodable {
        var id: String?
        var command: String?
        var event: String?
        var ok: Bool?
    }
}
