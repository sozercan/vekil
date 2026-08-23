import Foundation
import XCTest
@testable import VekilCore

final class RuntimeFrameCodecTests: XCTestCase {
    func testHelloDecodesAllRequiredIntegrationFields() throws {
        let data = Data(
            #"{"v":1,"event":"hello","payload":{"protocol_min":1,"protocol_max":2,"helper_build":"v1.2.3","bundle_build_id":"build_abc","pid":1234,"helper_epoch":"hep_456"}}"#.utf8
        )

        guard case let .event(envelope) = try RuntimeFrameCodec().decode(data),
              case let .hello(hello) = try envelope.decodePayload() else {
            return XCTFail("Expected hello event")
        }

        XCTAssertEqual(hello.protocolMin, 1)
        XCTAssertEqual(hello.protocolMax, 2)
        XCTAssertEqual(hello.helperBuild, "v1.2.3")
        XCTAssertEqual(hello.bundleBuildID, "build_abc")
        XCTAssertEqual(hello.pid, 1234)
        XCTAssertEqual(hello.helperEpoch, "hep_456")
    }

    func testHelloAcceptsEpochAtEnvelopeLevelForProtocolCompatibility() throws {
        let data = Data(
            #"{"v":1,"event":"hello","helper_epoch":"hep_top","payload":{"protocol_min":1,"protocol_max":1,"helper_build":"v1","bundle_build_id":"build_abc","pid":1234}}"#.utf8
        )
        guard case let .event(envelope) = try RuntimeFrameCodec().decode(data),
              case let .hello(hello) = try envelope.decodePayload() else {
            return XCTFail("Expected hello event")
        }
        XCTAssertEqual(hello.helperEpoch, "hep_top")
    }

    func testRequestResponseAndEventRoundTrip() throws {
        let codec = RuntimeFrameCodec()
        let request = RuntimeRequestEnvelope(
            version: 1,
            id: "req_123",
            command: .start,
            payload: .object(["expected_config_revision": .string("cfg_abc")])
        )
        XCTAssertEqual(try codec.decode(codec.encode(request)), .request(request))

        let response = RuntimeResponseEnvelope(
            version: 1,
            id: "req_123",
            helperEpoch: "hep_456",
            ok: true,
            result: .object([
                "accepted": .bool(true),
                "operation_id": .string("op_789"),
            ])
        )
        XCTAssertEqual(try codec.decode(codec.encode(response)), .response(response))

        let event = RuntimeEventEnvelope(
            version: 1,
            event: .state,
            helperEpoch: "hep_456",
            stateRevision: 12,
            payload: .object([
                "service": .string("running"),
                "readiness": .string("ready"),
                "auth": .string("signed_in"),
            ])
        )
        XCTAssertEqual(try codec.decode(codec.encode(event)), .event(event))
    }


    func testStateDecodesAuthenticationSourceAndDeviceCodeRevision() throws {
        let stateData = Data(
            #"{"v":1,"event":"state","helper_epoch":"hep","state_revision":4,"payload":{"service":"stopped","readiness":"unknown","auth":"signed_in","auth_source":"environment","configuration":{"mode":"legacy","available":true,"drifted":false,"managed_ownership_present":false}}}"#.utf8
        )
        guard case let .event(stateEnvelope) = try RuntimeFrameCodec().decode(stateData),
              case let .state(state) = try stateEnvelope.decodePayload() else {
            return XCTFail("Expected state event")
        }
        XCTAssertEqual(state.payload.authSource, .environment)

        let deviceData = Data(
            #"{"v":1,"event":"device_code","helper_epoch":"hep","state_revision":5,"payload":{"operation_id":"op_device","verification_url":"https://github.com/login/device","user_code":"ABCD-EFGH","expires_in":900}}"#.utf8
        )
        guard case let .event(deviceEnvelope) = try RuntimeFrameCodec().decode(deviceData),
              case let .deviceCode(device) = try deviceEnvelope.decodePayload() else {
            return XCTFail("Expected device-code event")
        }
        XCTAssertEqual(device.stateRevision, 5)
        XCTAssertEqual(device.payload.expiresInSeconds, 900)
    }

    func testStructuredErrorUsesAllowlistedShape() throws {
        let data = Data(
            #"{"code":"invalid_config","user_message":"Invalid.","retryable":false,"recovery_action":"open_providers","field_errors":[{"path":"providers[0].base_url","code":"invalid_url","message":"Enter a valid URL."}]}"#.utf8
        )
        let error = try JSONDecoder().decode(RuntimeStructuredError.self, from: data)

        XCTAssertEqual(error.code, "invalid_config")
        XCTAssertEqual(error.userMessage, "Invalid.")
        XCTAssertFalse(error.retryable)
        XCTAssertEqual(error.recoveryAction, "open_providers")
        XCTAssertEqual(
            error.fieldErrors,
            [RuntimeFieldError(path: "providers[0].base_url", code: "invalid_url", message: "Enter a valid URL.")]
        )
    }

    func testDuplicateKeysAreRejectedAtAnyDepthIncludingEscapedEquivalents() {
        let codec = RuntimeFrameCodec()
        let topLevel = Data(#"{"v":1,"v":1,"event":"hello","payload":{}}"#.utf8)
        XCTAssertThrowsError(try codec.decode(topLevel)) { error in
            XCTAssertEqual(error as? RuntimeJSONValidationError, .duplicateObjectKey("v"))
        }

        let nested = Data(#"{"v":1,"event":"hello","payload":{"a":1,"\u0061":2}}"#.utf8)
        XCTAssertThrowsError(try codec.decode(nested)) { error in
            XCTAssertEqual(error as? RuntimeJSONValidationError, .duplicateObjectKey("a"))
        }
    }

    func testInvalidUTF8TopLevelScalarAndAmbiguousEnvelopeAreRejected() {
        let codec = RuntimeFrameCodec()
        XCTAssertThrowsError(try codec.decode(Data([0x7B, 0xFF, 0x7D]))) { error in
            XCTAssertEqual(error as? RuntimeJSONValidationError, .invalidUTF8)
        }
        XCTAssertThrowsError(try codec.decode(Data("[]".utf8))) { error in
            XCTAssertEqual(error as? RuntimeJSONValidationError, .topLevelValueMustBeObject)
        }
        XCTAssertThrowsError(
            try codec.decode(Data(#"{"v":1,"id":"req","command":"start","ok":true}"#.utf8))
        ) { error in
            XCTAssertEqual(error as? RuntimeFrameCodecError, .ambiguousEnvelope)
        }
    }

    func testFailedResponseRequiresErrorAndCannotContainResult() {
        let codec = RuntimeFrameCodec()
        let missingError = Data(
            #"{"v":1,"id":"req","helper_epoch":"hep","ok":false}"#.utf8
        )
        XCTAssertThrowsError(try codec.decode(missingError))

        let both = Data(
            #"{"v":1,"id":"req","helper_epoch":"hep","ok":false,"result":{},"error":{"code":"x","user_message":"x","retryable":false}}"#.utf8
        )
        XCTAssertThrowsError(try codec.decode(both))
    }

    func testUnknownStringTokensRoundTripWithoutDecodeFailure() throws {
        let state = try JSONDecoder().decode(
            RuntimeStatePayload.self,
            from: Data(#"{"service":"future_service","readiness":"future_readiness","auth":"future_auth","operation":"idle"}"#.utf8)
        )
        XCTAssertEqual(state.service.rawValue, "future_service")
        XCTAssertEqual(state.readiness.rawValue, "future_readiness")
        XCTAssertEqual(state.auth.rawValue, "future_auth")
    }

    func testJSONValuePreservesFullWidthUnsignedIntegers() throws {
        let original = JSONValue.unsignedInteger(UInt64.max)
        let data = try JSONEncoder().encode(original)
        XCTAssertEqual(try JSONDecoder().decode(JSONValue.self, from: data), original)
    }

    func testConfigurationStateAcceptsDescriptiveAliases() throws {
        let configuration = try JSONDecoder().decode(
            RuntimeConfigurationState.self,
            from: Data(
                #"{"selected_mode":"external","selected_revision":"disk","active_revision":"live","drift_status":"drifted"}"#.utf8
            )
        )
        XCTAssertEqual(configuration.mode, .external)
        XCTAssertEqual(configuration.drift, .drifted)
    }

    func testConfigurationErrorsMapToBlockingDriftStates() throws {
        let cases: [(String, RuntimeConfigurationDrift)] = [
            ("missing_config", .missing),
            ("unsafe_symlink", .unsafe),
            ("unsafe_file_type", .unsafe),
            ("invalid_config", .invalid),
            ("config_too_large", .invalid),
            ("config_unavailable", .invalid),
        ]

        for (code, expected) in cases {
            let configuration = try JSONDecoder().decode(
                RuntimeConfigurationState.self,
                from: Data(
                    #"{"selected_mode":"external","drifted":false,"error_code":"\#(code)"}"#.utf8
                )
            )
            XCTAssertEqual(configuration.drift, expected, "error_code=\(code)")
            XCTAssertEqual(configuration.lastError?.code, code)
        }
    }

    func testExpectedStartRevisionPrefersTheStoppedSelection() {
        let state = RuntimeStatePayload(
            configRevision: "cfg_previous_runtime",
            service: .stopped,
            readiness: .unknown,
            auth: .signedIn,
            configuration: RuntimeConfigurationState(
                mode: .external,
                selectedRevision: "cfg_new_selection",
                activeRevision: "cfg_previous_runtime"
            )
        )

        XCTAssertEqual(state.expectedStartConfigRevision, "cfg_new_selection")
    }
}
