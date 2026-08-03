import Foundation

public protocol AnalyticsDataSource: Sendable {
    func fetchStats() async throws -> StatsSnapshot
    func fetchReadiness() async throws -> ReadinessResponse
    func fetchModels() async throws -> RuntimeModelCatalog
}

public enum AnalyticsHTTPError: Error, Equatable, Sendable, CustomStringConvertible {
    case invalidResponse
    case httpStatus(endpoint: String, statusCode: Int)
    case decoding(endpoint: String)

    public var description: String {
        switch self {
        case .invalidResponse:
            return "The proxy returned an invalid HTTP response."
        case let .httpStatus(endpoint, statusCode):
            return "\(endpoint) returned HTTP \(statusCode)."
        case let .decoding(endpoint):
            return "The \(endpoint) response could not be decoded."
        }
    }
}

/// Foundation transport for the local proxy endpoints used by native analytics.
/// It has no AppKit, SwiftUI, or Sparkle dependency.
public final class URLSessionAnalyticsDataSource: AnalyticsDataSource, @unchecked Sendable {
    public let baseURL: URL
    private let session: URLSession
    private let requestTimeout: TimeInterval

    public init(
        baseURL: URL,
        session: URLSession = .shared,
        requestTimeout: TimeInterval = 15
    ) {
        self.baseURL = baseURL
        self.session = session
        self.requestTimeout = requestTimeout
    }

    public func fetchStats() async throws -> StatsSnapshot {
        let endpoint = "/stats.json"
        let (data, response) = try await request(endpoint)
        guard (200..<300).contains(response.statusCode) else {
            throw AnalyticsHTTPError.httpStatus(endpoint: endpoint, statusCode: response.statusCode)
        }
        do {
            return try JSONDecoder().decode(StatsSnapshot.self, from: data)
        } catch {
            throw AnalyticsHTTPError.decoding(endpoint: endpoint)
        }
    }

    public func fetchReadiness() async throws -> ReadinessResponse {
        let endpoint = "/readyz"
        let (data, response) = try await request(endpoint)
        var decoded = (try? JSONDecoder().decode(ReadinessResponse.self, from: data)) ?? ReadinessResponse()
        decoded.httpStatusCode = response.statusCode
        if decoded.status.isEmpty {
            decoded.status = (200..<300).contains(response.statusCode) ? "ready" : "not_ready"
        }
        return decoded
    }

    public func fetchModels() async throws -> RuntimeModelCatalog {
        let endpoint = "/v1/models"
        let (data, response) = try await request(endpoint)
        guard (200..<300).contains(response.statusCode) else {
            throw AnalyticsHTTPError.httpStatus(endpoint: endpoint, statusCode: response.statusCode)
        }
        do {
            return try JSONDecoder().decode(RuntimeModelCatalog.self, from: data)
        } catch {
            throw AnalyticsHTTPError.decoding(endpoint: endpoint)
        }
    }

    private func request(_ endpoint: String) async throws -> (Data, HTTPURLResponse) {
        let relativePath = endpoint.hasPrefix("/") ? String(endpoint.dropFirst()) : endpoint
        let url = baseURL.appendingPathComponent(relativePath)
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.cachePolicy = .reloadIgnoringLocalCacheData
        request.timeoutInterval = requestTimeout
        request.setValue("application/json", forHTTPHeaderField: "Accept")

        let (data, response) = try await session.data(for: request)
        guard let httpResponse = response as? HTTPURLResponse else {
            throw AnalyticsHTTPError.invalidResponse
        }
        return (data, httpResponse)
    }
}
