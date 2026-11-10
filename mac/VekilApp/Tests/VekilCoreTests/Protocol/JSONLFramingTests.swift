import Foundation
import XCTest
@testable import VekilCore

final class JSONLFramingTests: XCTestCase {
    func testFragmentedAndCoalescedFramesAreDecodedInOrder() throws {
        var decoder = try JSONLFrameDecoder(maximumFrameSize: 64)

        XCTAssertEqual(try decoder.append(Data("{\"a\":".utf8)), [])
        XCTAssertEqual(
            try decoder.append(Data("1}\n{\"b\":2}\r\n{\"c\"".utf8)),
            [Data("{\"a\":1}".utf8), Data("{\"b\":2}".utf8)]
        )
        XCTAssertEqual(
            try decoder.append(Data(":3}\n".utf8)),
            [Data("{\"c\":3}".utf8)]
        )
        XCTAssertNoThrow(try decoder.finish())
    }

    func testMaximumSizeIsEnforcedBeforeNewline() throws {
        var decoder = try JSONLFrameDecoder(maximumFrameSize: 8)
        XCTAssertEqual(
            try decoder.append(Data("12345678\n".utf8)),
            [Data("12345678".utf8)]
        )

        XCTAssertThrowsError(try decoder.append(Data("123456789".utf8))) { error in
            XCTAssertEqual(error as? JSONLFrameError, .frameTooLarge(limit: 8))
        }
        XCTAssertEqual(decoder.bufferedByteCount, 0)
    }

    func testExactMaximumSizeAcceptsCRLFTogetherAndSplit() throws {
        let payload = Data("12345678".utf8)

        var together = try JSONLFrameDecoder(maximumFrameSize: payload.count)
        XCTAssertEqual(
            try together.append(Data("12345678\r\n".utf8)),
            [payload]
        )

        var split = try JSONLFrameDecoder(maximumFrameSize: payload.count)
        XCTAssertEqual(try split.append(Data("12345678\r".utf8)), [])
        XCTAssertEqual(split.bufferedByteCount, payload.count + 1)
        XCTAssertEqual(try split.append(Data("\n".utf8)), [payload])
        XCTAssertEqual(split.bufferedByteCount, 0)
    }

    func testProvisionalCRDoesNotPermitOversizePayload() throws {
        var decoder = try JSONLFrameDecoder(maximumFrameSize: 8)
        XCTAssertEqual(try decoder.append(Data("12345678\r".utf8)), [])

        XCTAssertThrowsError(try decoder.append(Data("x\n".utf8))) { error in
            XCTAssertEqual(error as? JSONLFrameError, .frameTooLarge(limit: 8))
        }
        XCTAssertEqual(decoder.bufferedByteCount, 0)
    }

    func testSplitOversizeFrameIsRejectedIncrementally() throws {
        var decoder = try JSONLFrameDecoder(maximumFrameSize: 5)
        XCTAssertEqual(try decoder.append(Data("123".utf8)), [])
        XCTAssertThrowsError(try decoder.append(Data("456".utf8))) { error in
            XCTAssertEqual(error as? JSONLFrameError, .frameTooLarge(limit: 5))
        }
    }

    func testEmptyAndUnterminatedFramesAreRejected() throws {
        var empty = try JSONLFrameDecoder()
        XCTAssertThrowsError(try empty.append(Data("\n".utf8))) { error in
            XCTAssertEqual(error as? JSONLFrameError, .emptyFrame)
        }

        var unterminated = try JSONLFrameDecoder()
        _ = try unterminated.append(Data("{}".utf8))
        XCTAssertThrowsError(try unterminated.finish()) { error in
            XCTAssertEqual(error as? JSONLFrameError, .unterminatedFrame(byteCount: 2))
        }
    }

    func testProtocolLimitIsExactlyOneMiB() {
        XCTAssertEqual(JSONLFrameDecoder.protocolMaximumFrameSize, 1_048_576)
    }
}
