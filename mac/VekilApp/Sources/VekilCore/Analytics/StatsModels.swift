import Foundation

/// The Go-owned payload returned by `GET /stats.json`.
///
/// Every field currently emitted by `proxy/stats.go` is represented. Decoding is
/// intentionally tolerant of omitted/null fields and additive server changes so
/// an older native shell can continue presenting the compatible subset.
public struct StatsSnapshot: Codable, Equatable, Sendable {
    public var uptimeSeconds: Int64
    public var inflight: Int64
    public var totals: StatsTotals
    public var status: [String: Int64]
    public var statusCodes: [StatsCountRow]
    public var errors: [StatsCountRow]
    public var series: [StatsSeriesPoint]
    public var byModel: [StatsBreakdown]
    public var byProvider: [StatsBreakdown]
    public var byAgent: [StatsBreakdown]
    public var byRoute: [StatsBreakdown]
    public var byTarget: [StatsTargetBreakdown]
    public var physicalUsage: StatsTokenUsage
    public var wastedUsage: StatsTokenUsage
    public var upstreamAttempts: Int64
    public var targetSwitches: Int64
    public var requestsWithFailover: Int64
    public var successfulFailovers: Int64
    public var routeExhaustions: Int64
    public var stateBindingHits: Int64
    public var stateBindingMisses: Int64
    public var stateBindingEvictions: Int64
    public var retries: Int64
    public var retriesByCode: [StatsCountRow]
    public var recent: [StatsRecentRequest]
    public var recentAttempts: [StatsRecentAttempt]
    public var insightsEnabled: Bool
    public var policyRouting: PolicyStatsSnapshot

    public init(
        uptimeSeconds: Int64 = 0,
        inflight: Int64 = 0,
        totals: StatsTotals = .init(),
        status: [String: Int64] = [:],
        statusCodes: [StatsCountRow] = [],
        errors: [StatsCountRow] = [],
        series: [StatsSeriesPoint] = [],
        byModel: [StatsBreakdown] = [],
        byProvider: [StatsBreakdown] = [],
        byAgent: [StatsBreakdown] = [],
        byRoute: [StatsBreakdown] = [],
        byTarget: [StatsTargetBreakdown] = [],
        physicalUsage: StatsTokenUsage = .init(),
        wastedUsage: StatsTokenUsage = .init(),
        upstreamAttempts: Int64 = 0,
        targetSwitches: Int64 = 0,
        requestsWithFailover: Int64 = 0,
        successfulFailovers: Int64 = 0,
        routeExhaustions: Int64 = 0,
        stateBindingHits: Int64 = 0,
        stateBindingMisses: Int64 = 0,
        stateBindingEvictions: Int64 = 0,
        retries: Int64 = 0,
        retriesByCode: [StatsCountRow] = [],
        recent: [StatsRecentRequest] = [],
        recentAttempts: [StatsRecentAttempt] = [],
        insightsEnabled: Bool = false,
        policyRouting: PolicyStatsSnapshot = .init()
    ) {
        self.uptimeSeconds = uptimeSeconds
        self.inflight = inflight
        self.totals = totals
        self.status = status
        self.statusCodes = statusCodes
        self.errors = errors
        self.series = series
        self.byModel = byModel
        self.byProvider = byProvider
        self.byAgent = byAgent
        self.byRoute = byRoute
        self.byTarget = byTarget
        self.physicalUsage = physicalUsage
        self.wastedUsage = wastedUsage
        self.upstreamAttempts = upstreamAttempts
        self.targetSwitches = targetSwitches
        self.requestsWithFailover = requestsWithFailover
        self.successfulFailovers = successfulFailovers
        self.routeExhaustions = routeExhaustions
        self.stateBindingHits = stateBindingHits
        self.stateBindingMisses = stateBindingMisses
        self.stateBindingEvictions = stateBindingEvictions
        self.retries = retries
        self.retriesByCode = retriesByCode
        self.recent = recent
        self.recentAttempts = recentAttempts
        self.insightsEnabled = insightsEnabled
        self.policyRouting = policyRouting
    }

    private enum CodingKeys: String, CodingKey {
        case uptimeSeconds = "uptime_seconds"
        case inflight
        case totals
        case status
        case statusCodes = "status_codes"
        case errors
        case series
        case byModel = "by_model"
        case byProvider = "by_provider"
        case byAgent = "by_agent"
        case byRoute = "by_route"
        case byTarget = "by_target"
        case physicalUsage = "physical_usage"
        case wastedUsage = "wasted_usage"
        case upstreamAttempts = "upstream_attempts"
        case targetSwitches = "target_switches"
        case requestsWithFailover = "requests_with_failover"
        case successfulFailovers = "successful_failovers"
        case routeExhaustions = "route_exhaustions"
        case stateBindingHits = "state_binding_hits"
        case stateBindingMisses = "state_binding_misses"
        case stateBindingEvictions = "state_binding_evictions"
        case retries
        case retriesByCode = "retries_by_code"
        case recent
        case recentAttempts = "recent_attempts"
        case insightsEnabled = "insights_enabled"
        case policyRouting = "policy_routing"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        uptimeSeconds = container.decodeLossyInt64(forKey: .uptimeSeconds)
        inflight = container.decodeLossyInt64(forKey: .inflight)
        totals = container.decodeDefault(StatsTotals.self, forKey: .totals, defaultValue: .init())
        status = container.decodeLossyInt64Dictionary(forKey: .status)
        statusCodes = container.decodeLossyArray(StatsCountRow.self, forKey: .statusCodes)
        errors = container.decodeLossyArray(StatsCountRow.self, forKey: .errors)
        series = container.decodeLossyArray(StatsSeriesPoint.self, forKey: .series)
        byModel = container.decodeLossyArray(StatsBreakdown.self, forKey: .byModel)
        byProvider = container.decodeLossyArray(StatsBreakdown.self, forKey: .byProvider)
        byAgent = container.decodeLossyArray(StatsBreakdown.self, forKey: .byAgent)
        byRoute = container.decodeLossyArray(StatsBreakdown.self, forKey: .byRoute)
        byTarget = container.decodeLossyArray(StatsTargetBreakdown.self, forKey: .byTarget)
        physicalUsage = container.decodeDefault(StatsTokenUsage.self, forKey: .physicalUsage, defaultValue: .init())
        wastedUsage = container.decodeDefault(StatsTokenUsage.self, forKey: .wastedUsage, defaultValue: .init())
        upstreamAttempts = container.decodeLossyInt64(forKey: .upstreamAttempts)
        targetSwitches = container.decodeLossyInt64(forKey: .targetSwitches)
        requestsWithFailover = container.decodeLossyInt64(forKey: .requestsWithFailover)
        successfulFailovers = container.decodeLossyInt64(forKey: .successfulFailovers)
        routeExhaustions = container.decodeLossyInt64(forKey: .routeExhaustions)
        stateBindingHits = container.decodeLossyInt64(forKey: .stateBindingHits)
        stateBindingMisses = container.decodeLossyInt64(forKey: .stateBindingMisses)
        stateBindingEvictions = container.decodeLossyInt64(forKey: .stateBindingEvictions)
        retries = container.decodeLossyInt64(forKey: .retries)
        retriesByCode = container.decodeLossyArray(StatsCountRow.self, forKey: .retriesByCode)
        recent = container.decodeLossyArray(StatsRecentRequest.self, forKey: .recent)
        recentAttempts = container.decodeLossyArray(StatsRecentAttempt.self, forKey: .recentAttempts)
        insightsEnabled = container.decodeLossyBool(forKey: .insightsEnabled)
        policyRouting = container.decodeDefault(PolicyStatsSnapshot.self, forKey: .policyRouting, defaultValue: .init())
    }
}

public struct StatsTotals: Codable, Equatable, Sendable {
    public var requests: Int64
    public var errors: Int64
    public var promptTokens: Int64
    public var completionTokens: Int64
    public var totalTokens: Int64
    public var cachedTokens: Int64
    public var reasoningTokens: Int64
    public var latencyP50Milliseconds: Int64
    public var latencyP95Milliseconds: Int64
    public var latencyP99Milliseconds: Int64

    public init(
        requests: Int64 = 0,
        errors: Int64 = 0,
        promptTokens: Int64 = 0,
        completionTokens: Int64 = 0,
        totalTokens: Int64 = 0,
        cachedTokens: Int64 = 0,
        reasoningTokens: Int64 = 0,
        latencyP50Milliseconds: Int64 = 0,
        latencyP95Milliseconds: Int64 = 0,
        latencyP99Milliseconds: Int64 = 0
    ) {
        self.requests = requests
        self.errors = errors
        self.promptTokens = promptTokens
        self.completionTokens = completionTokens
        self.totalTokens = totalTokens
        self.cachedTokens = cachedTokens
        self.reasoningTokens = reasoningTokens
        self.latencyP50Milliseconds = latencyP50Milliseconds
        self.latencyP95Milliseconds = latencyP95Milliseconds
        self.latencyP99Milliseconds = latencyP99Milliseconds
    }

    private enum CodingKeys: String, CodingKey {
        case requests, errors
        case promptTokens = "prompt_tokens"
        case completionTokens = "completion_tokens"
        case totalTokens = "total_tokens"
        case cachedTokens = "cached_tokens"
        case reasoningTokens = "reasoning_tokens"
        case latencyP50Milliseconds = "latency_p50_ms"
        case latencyP95Milliseconds = "latency_p95_ms"
        case latencyP99Milliseconds = "latency_p99_ms"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        requests = container.decodeLossyInt64(forKey: .requests)
        errors = container.decodeLossyInt64(forKey: .errors)
        promptTokens = container.decodeLossyInt64(forKey: .promptTokens)
        completionTokens = container.decodeLossyInt64(forKey: .completionTokens)
        totalTokens = container.decodeLossyInt64(forKey: .totalTokens)
        cachedTokens = container.decodeLossyInt64(forKey: .cachedTokens)
        reasoningTokens = container.decodeLossyInt64(forKey: .reasoningTokens)
        latencyP50Milliseconds = container.decodeLossyInt64(forKey: .latencyP50Milliseconds)
        latencyP95Milliseconds = container.decodeLossyInt64(forKey: .latencyP95Milliseconds)
        latencyP99Milliseconds = container.decodeLossyInt64(forKey: .latencyP99Milliseconds)
    }
}

public struct StatsSeriesPoint: Codable, Equatable, Sendable {
    public var timestamp: Int64
    public var requests: Int64
    public var errors: Int64
    public var promptTokens: Int64
    public var completionTokens: Int64

    public init(
        timestamp: Int64 = 0,
        requests: Int64 = 0,
        errors: Int64 = 0,
        promptTokens: Int64 = 0,
        completionTokens: Int64 = 0
    ) {
        self.timestamp = timestamp
        self.requests = requests
        self.errors = errors
        self.promptTokens = promptTokens
        self.completionTokens = completionTokens
    }

    public var date: Date { Date(timeIntervalSince1970: TimeInterval(timestamp)) }

    private enum CodingKeys: String, CodingKey {
        case timestamp = "t"
        case requests = "req"
        case errors = "err"
        case promptTokens = "prompt"
        case completionTokens = "completion"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        timestamp = container.decodeLossyInt64(forKey: .timestamp)
        requests = container.decodeLossyInt64(forKey: .requests)
        errors = container.decodeLossyInt64(forKey: .errors)
        promptTokens = container.decodeLossyInt64(forKey: .promptTokens)
        completionTokens = container.decodeLossyInt64(forKey: .completionTokens)
    }
}

public struct StatsBreakdown: Codable, Equatable, Sendable {
    public var model: String
    public var provider: String
    public var kind: String
    public var agent: String
    public var route: String
    public var requests: Int64
    public var tokens: Int64
    public var errors: Int64
    public var averageMilliseconds: Int64
    public var targetSwitches: Int64
    public var requestsWithFailover: Int64
    public var successfulFailovers: Int64
    public var routeExhaustions: Int64

    public init(
        model: String = "",
        provider: String = "",
        kind: String = "",
        agent: String = "",
        route: String = "",
        requests: Int64 = 0,
        tokens: Int64 = 0,
        errors: Int64 = 0,
        averageMilliseconds: Int64 = 0,
        targetSwitches: Int64 = 0,
        requestsWithFailover: Int64 = 0,
        successfulFailovers: Int64 = 0,
        routeExhaustions: Int64 = 0
    ) {
        self.model = model
        self.provider = provider
        self.kind = kind
        self.agent = agent
        self.route = route
        self.requests = requests
        self.tokens = tokens
        self.errors = errors
        self.averageMilliseconds = averageMilliseconds
        self.targetSwitches = targetSwitches
        self.requestsWithFailover = requestsWithFailover
        self.successfulFailovers = successfulFailovers
        self.routeExhaustions = routeExhaustions
    }

    public var label: String {
        if !model.isEmpty { return model }
        if !provider.isEmpty { return provider }
        if !agent.isEmpty { return agent }
        return route
    }

    private enum CodingKeys: String, CodingKey {
        case model, provider, kind, agent, route, requests, tokens, errors
        case averageMilliseconds = "avg_ms"
        case targetSwitches = "target_switches"
        case requestsWithFailover = "requests_with_failover"
        case successfulFailovers = "successful_failovers"
        case routeExhaustions = "route_exhaustions"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        model = container.decodeDefault(String.self, forKey: .model, defaultValue: "")
        provider = container.decodeDefault(String.self, forKey: .provider, defaultValue: "")
        kind = container.decodeDefault(String.self, forKey: .kind, defaultValue: "")
        agent = container.decodeDefault(String.self, forKey: .agent, defaultValue: "")
        route = container.decodeDefault(String.self, forKey: .route, defaultValue: "")
        requests = container.decodeLossyInt64(forKey: .requests)
        tokens = container.decodeLossyInt64(forKey: .tokens)
        errors = container.decodeLossyInt64(forKey: .errors)
        averageMilliseconds = container.decodeLossyInt64(forKey: .averageMilliseconds)
        targetSwitches = container.decodeLossyInt64(forKey: .targetSwitches)
        requestsWithFailover = container.decodeLossyInt64(forKey: .requestsWithFailover)
        successfulFailovers = container.decodeLossyInt64(forKey: .successfulFailovers)
        routeExhaustions = container.decodeLossyInt64(forKey: .routeExhaustions)
    }
}

public struct StatsTokenUsage: Codable, Equatable, Sendable {
    public var promptTokens: Int64
    public var completionTokens: Int64
    public var totalTokens: Int64
    public var cachedTokens: Int64
    public var reasoningTokens: Int64

    public init(
        promptTokens: Int64 = 0,
        completionTokens: Int64 = 0,
        totalTokens: Int64 = 0,
        cachedTokens: Int64 = 0,
        reasoningTokens: Int64 = 0
    ) {
        self.promptTokens = promptTokens
        self.completionTokens = completionTokens
        self.totalTokens = totalTokens
        self.cachedTokens = cachedTokens
        self.reasoningTokens = reasoningTokens
    }

    private enum CodingKeys: String, CodingKey {
        case promptTokens = "prompt_tokens"
        case completionTokens = "completion_tokens"
        case totalTokens = "total_tokens"
        case cachedTokens = "cached_tokens"
        case reasoningTokens = "reasoning_tokens"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        promptTokens = container.decodeLossyInt64(forKey: .promptTokens)
        completionTokens = container.decodeLossyInt64(forKey: .completionTokens)
        totalTokens = container.decodeLossyInt64(forKey: .totalTokens)
        cachedTokens = container.decodeLossyInt64(forKey: .cachedTokens)
        reasoningTokens = container.decodeLossyInt64(forKey: .reasoningTokens)
    }
}

public struct StatsTargetBreakdown: Codable, Equatable, Sendable {
    public var route: String
    public var target: String
    public var provider: String
    public var kind: String
    public var attempts: Int64
    public var physicalUsage: StatsTokenUsage
    public var wastedUsage: StatsTokenUsage

    public init(
        route: String = "",
        target: String = "",
        provider: String = "",
        kind: String = "",
        attempts: Int64 = 0,
        physicalUsage: StatsTokenUsage = .init(),
        wastedUsage: StatsTokenUsage = .init()
    ) {
        self.route = route
        self.target = target
        self.provider = provider
        self.kind = kind
        self.attempts = attempts
        self.physicalUsage = physicalUsage
        self.wastedUsage = wastedUsage
    }

    private enum CodingKeys: String, CodingKey {
        case route, target, provider, kind, attempts
        case physicalUsage = "physical_usage"
        case wastedUsage = "wasted_usage"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        route = container.decodeDefault(String.self, forKey: .route, defaultValue: "")
        target = container.decodeDefault(String.self, forKey: .target, defaultValue: "")
        provider = container.decodeDefault(String.self, forKey: .provider, defaultValue: "")
        kind = container.decodeDefault(String.self, forKey: .kind, defaultValue: "")
        attempts = container.decodeLossyInt64(forKey: .attempts)
        physicalUsage = container.decodeDefault(StatsTokenUsage.self, forKey: .physicalUsage, defaultValue: .init())
        wastedUsage = container.decodeDefault(StatsTokenUsage.self, forKey: .wastedUsage, defaultValue: .init())
    }
}

public struct StatsCountRow: Codable, Equatable, Sendable {
    public var label: String
    public var count: Int64

    public init(label: String = "", count: Int64 = 0) {
        self.label = label
        self.count = count
    }

    private enum CodingKeys: String, CodingKey { case label, count }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        label = container.decodeDefault(String.self, forKey: .label, defaultValue: "")
        count = container.decodeLossyInt64(forKey: .count)
    }
}

public struct StatsRecentRequest: Codable, Equatable, Sendable {
    public var timestamp: Int64
    public var endpoint: String
    public var model: String
    public var provider: String
    public var operationID: String
    public var routeID: String
    public var finalTarget: String
    public var upstreamSends: Int64
    public var targetSwitches: Int64
    public var agent: String
    public var status: Int
    public var durationMilliseconds: Int64
    public var totalTokens: Int64
    public var upstreamRequestID: String

    public init(
        timestamp: Int64 = 0,
        endpoint: String = "",
        model: String = "",
        provider: String = "",
        operationID: String = "",
        routeID: String = "",
        finalTarget: String = "",
        upstreamSends: Int64 = 0,
        targetSwitches: Int64 = 0,
        agent: String = "",
        status: Int = 0,
        durationMilliseconds: Int64 = 0,
        totalTokens: Int64 = 0,
        upstreamRequestID: String = ""
    ) {
        self.timestamp = timestamp
        self.endpoint = endpoint
        self.model = model
        self.provider = provider
        self.operationID = operationID
        self.routeID = routeID
        self.finalTarget = finalTarget
        self.upstreamSends = upstreamSends
        self.targetSwitches = targetSwitches
        self.agent = agent
        self.status = status
        self.durationMilliseconds = durationMilliseconds
        self.totalTokens = totalTokens
        self.upstreamRequestID = upstreamRequestID
    }

    public var date: Date { Date(timeIntervalSince1970: TimeInterval(timestamp)) }
    public var isError: Bool { status >= 400 }
    public var isFailover: Bool { targetSwitches > 0 }

    private enum CodingKeys: String, CodingKey {
        case timestamp = "t"
        case endpoint, model, provider
        case operationID = "operation_id"
        case routeID = "route_id"
        case finalTarget = "final_target"
        case upstreamSends = "upstream_sends"
        case targetSwitches = "target_switches"
        case agent, status
        case durationMilliseconds = "dur_ms"
        case totalTokens = "total_tokens"
        case upstreamRequestID = "upstream_request_id"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        timestamp = container.decodeLossyInt64(forKey: .timestamp)
        endpoint = container.decodeDefault(String.self, forKey: .endpoint, defaultValue: "")
        model = container.decodeDefault(String.self, forKey: .model, defaultValue: "")
        provider = container.decodeDefault(String.self, forKey: .provider, defaultValue: "")
        operationID = container.decodeDefault(String.self, forKey: .operationID, defaultValue: "")
        routeID = container.decodeDefault(String.self, forKey: .routeID, defaultValue: "")
        finalTarget = container.decodeDefault(String.self, forKey: .finalTarget, defaultValue: "")
        upstreamSends = container.decodeLossyInt64(forKey: .upstreamSends)
        targetSwitches = container.decodeLossyInt64(forKey: .targetSwitches)
        agent = container.decodeDefault(String.self, forKey: .agent, defaultValue: "")
        status = container.decodeLossyInt(forKey: .status)
        durationMilliseconds = container.decodeLossyInt64(forKey: .durationMilliseconds)
        totalTokens = container.decodeLossyInt64(forKey: .totalTokens)
        upstreamRequestID = container.decodeDefault(String.self, forKey: .upstreamRequestID, defaultValue: "")
    }
}

public struct StatsRecentAttempt: Codable, Equatable, Sendable {
    public var timestamp: Int64
    public var operationID: String
    public var routeID: String
    public var targetID: String
    public var providerID: String
    public var providerKind: String
    public var sequence: Int
    public var attemptKind: String
    public var status: String
    public var statusCode: Int
    public var outcome: String
    public var delivery: String
    public var semanticProgress: String
    public var downstreamCommitment: String
    public var retryDecision: String
    public var timeToFirstTokenMilliseconds: Int64?
    public var retryAfterSeconds: Int64?
    public var upstreamRequestID: String
    public var cleanupComplete: Bool
    public var reportedUsage: StatsTokenUsage?

    public init(
        timestamp: Int64 = 0,
        operationID: String = "",
        routeID: String = "",
        targetID: String = "",
        providerID: String = "",
        providerKind: String = "",
        sequence: Int = 0,
        attemptKind: String = "",
        status: String = "",
        statusCode: Int = 0,
        outcome: String = "",
        delivery: String = "",
        semanticProgress: String = "",
        downstreamCommitment: String = "",
        retryDecision: String = "",
        timeToFirstTokenMilliseconds: Int64? = nil,
        retryAfterSeconds: Int64? = nil,
        upstreamRequestID: String = "",
        cleanupComplete: Bool = false,
        reportedUsage: StatsTokenUsage? = nil
    ) {
        self.timestamp = timestamp
        self.operationID = operationID
        self.routeID = routeID
        self.targetID = targetID
        self.providerID = providerID
        self.providerKind = providerKind
        self.sequence = sequence
        self.attemptKind = attemptKind
        self.status = status
        self.statusCode = statusCode
        self.outcome = outcome
        self.delivery = delivery
        self.semanticProgress = semanticProgress
        self.downstreamCommitment = downstreamCommitment
        self.retryDecision = retryDecision
        self.timeToFirstTokenMilliseconds = timeToFirstTokenMilliseconds
        self.retryAfterSeconds = retryAfterSeconds
        self.upstreamRequestID = upstreamRequestID
        self.cleanupComplete = cleanupComplete
        self.reportedUsage = reportedUsage
    }

    public var date: Date { Date(timeIntervalSince1970: TimeInterval(timestamp)) }

    /// Policy-routed attempts intentionally expose only the public policy ID for
    /// route/target and remove provider/request identifiers.
    public var isTopologyRedacted: Bool {
        !routeID.isEmpty && routeID == targetID &&
            providerID.isEmpty && providerKind.isEmpty && upstreamRequestID.isEmpty
    }

    private enum CodingKeys: String, CodingKey {
        case timestamp = "t"
        case operationID = "operation_id"
        case routeID = "route_id"
        case targetID = "target_id"
        case providerID = "provider_id"
        case providerKind = "provider_kind"
        case sequence
        case attemptKind = "attempt_kind"
        case status
        case statusCode = "status_code"
        case outcome, delivery
        case semanticProgress = "semantic_progress"
        case downstreamCommitment = "downstream_commitment"
        case retryDecision = "retry_decision"
        case timeToFirstTokenMilliseconds = "ttft_ms"
        case retryAfterSeconds = "retry_after_seconds"
        case upstreamRequestID = "upstream_request_id"
        case cleanupComplete = "cleanup_complete"
        case reportedUsage = "reported_usage"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        timestamp = container.decodeLossyInt64(forKey: .timestamp)
        operationID = container.decodeDefault(String.self, forKey: .operationID, defaultValue: "")
        routeID = container.decodeDefault(String.self, forKey: .routeID, defaultValue: "")
        targetID = container.decodeDefault(String.self, forKey: .targetID, defaultValue: "")
        providerID = container.decodeDefault(String.self, forKey: .providerID, defaultValue: "")
        providerKind = container.decodeDefault(String.self, forKey: .providerKind, defaultValue: "")
        sequence = container.decodeLossyInt(forKey: .sequence)
        attemptKind = container.decodeDefault(String.self, forKey: .attemptKind, defaultValue: "")
        status = container.decodeDefault(String.self, forKey: .status, defaultValue: "")
        statusCode = container.decodeLossyInt(forKey: .statusCode)
        outcome = container.decodeDefault(String.self, forKey: .outcome, defaultValue: "")
        delivery = container.decodeDefault(String.self, forKey: .delivery, defaultValue: "")
        semanticProgress = container.decodeDefault(String.self, forKey: .semanticProgress, defaultValue: "")
        downstreamCommitment = container.decodeDefault(String.self, forKey: .downstreamCommitment, defaultValue: "")
        retryDecision = container.decodeDefault(String.self, forKey: .retryDecision, defaultValue: "")
        timeToFirstTokenMilliseconds = container.decodeLossyOptionalInt64(forKey: .timeToFirstTokenMilliseconds)
        retryAfterSeconds = container.decodeLossyOptionalInt64(forKey: .retryAfterSeconds)
        upstreamRequestID = container.decodeDefault(String.self, forKey: .upstreamRequestID, defaultValue: "")
        cleanupComplete = container.decodeLossyBool(forKey: .cleanupComplete)
        reportedUsage = try? container.decode(StatsTokenUsage.self, forKey: .reportedUsage)
    }
}
