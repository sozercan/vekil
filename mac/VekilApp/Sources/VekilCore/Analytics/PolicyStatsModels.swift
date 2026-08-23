import Foundation

/// Additive policy-routing telemetry embedded in `/stats.json`.
public struct PolicyStatsSnapshot: Codable, Equatable, Sendable {
    public var totals: PolicyStatsMetrics
    public var profiles: [PolicyStatsProfile]

    public init(totals: PolicyStatsMetrics = .init(), profiles: [PolicyStatsProfile] = []) {
        self.totals = totals
        self.profiles = profiles
    }

    private enum CodingKeys: String, CodingKey { case totals, profiles }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        totals = container.decodeDefault(PolicyStatsMetrics.self, forKey: .totals, defaultValue: .init())
        profiles = container.decodeLossyArray(PolicyStatsProfile.self, forKey: .profiles)
    }
}

public struct PolicyStatsProfile: Codable, Equatable, Sendable {
    public var profile: String
    public var effectiveMode: String
    public var preflightState: String
    public var breakerState: String
    public var generationHashes: PolicyStatsGenerationHashes
    public var totals: PolicyStatsMetrics
    public var trafficBuckets: [PolicyStatsTrafficBucket]

    public init(
        profile: String = "",
        effectiveMode: String = "",
        preflightState: String = "",
        breakerState: String = "",
        generationHashes: PolicyStatsGenerationHashes = .init(),
        totals: PolicyStatsMetrics = .init(),
        trafficBuckets: [PolicyStatsTrafficBucket] = []
    ) {
        self.profile = profile
        self.effectiveMode = effectiveMode
        self.preflightState = preflightState
        self.breakerState = breakerState
        self.generationHashes = generationHashes
        self.totals = totals
        self.trafficBuckets = trafficBuckets
    }

    private enum CodingKeys: String, CodingKey {
        case profile
        case effectiveMode = "effective_mode"
        case preflightState = "preflight_state"
        case breakerState = "breaker_state"
        case generationHashes = "generation_hashes"
        case totals
        case trafficBuckets = "traffic_buckets"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        profile = container.decodeDefault(String.self, forKey: .profile, defaultValue: "")
        effectiveMode = container.decodeDefault(String.self, forKey: .effectiveMode, defaultValue: "")
        preflightState = container.decodeDefault(String.self, forKey: .preflightState, defaultValue: "")
        breakerState = container.decodeDefault(String.self, forKey: .breakerState, defaultValue: "")
        generationHashes = container.decodeDefault(
            PolicyStatsGenerationHashes.self,
            forKey: .generationHashes,
            defaultValue: .init()
        )
        totals = container.decodeDefault(PolicyStatsMetrics.self, forKey: .totals, defaultValue: .init())
        trafficBuckets = container.decodeLossyArray(PolicyStatsTrafficBucket.self, forKey: .trafficBuckets)
    }
}

public struct PolicyStatsGenerationHashes: Codable, Equatable, Sendable {
    public var config: String
    public var profile: String
    public var classifier: String
    public var binary: String

    public init(config: String = "", profile: String = "", classifier: String = "", binary: String = "") {
        self.config = config
        self.profile = profile
        self.classifier = classifier
        self.binary = binary
    }

    private enum CodingKeys: String, CodingKey {
        case config = "config_generation"
        case profile = "profile_generation"
        case classifier = "classifier_generation"
        case binary = "binary_generation"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        config = container.decodeDefault(String.self, forKey: .config, defaultValue: "")
        profile = container.decodeDefault(String.self, forKey: .profile, defaultValue: "")
        classifier = container.decodeDefault(String.self, forKey: .classifier, defaultValue: "")
        binary = container.decodeDefault(String.self, forKey: .binary, defaultValue: "")
    }
}

public struct PolicyStatsTrafficBucket: Codable, Equatable, Sendable {
    public var trafficBucket: String
    public var metrics: PolicyStatsMetrics

    public init(trafficBucket: String = "", metrics: PolicyStatsMetrics = .init()) {
        self.trafficBucket = trafficBucket
        self.metrics = metrics
    }

    private enum CodingKeys: String, CodingKey {
        case trafficBucket = "traffic_bucket"
        case metrics
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        trafficBucket = container.decodeDefault(String.self, forKey: .trafficBucket, defaultValue: "")
        metrics = container.decodeDefault(PolicyStatsMetrics.self, forKey: .metrics, defaultValue: .init())
    }
}

public struct PolicyStatsMetrics: Codable, Equatable, Sendable {
    public var eligible: Int64
    public var sampled: Int64
    public var admitted: Int64
    public var dropReasons: [StatsCountRow]
    public var classifier: PolicyStatsClassifier
    public var actualTiers: PolicyStatsTierCounts
    public var shadowTiers: PolicyStatsTierCounts
    public var classifierLatency: PolicyStatsLatency
    public var classifierUsage: PolicyStatsTokenUsage
    public var physicalClassifierSends: Int64

    public init(
        eligible: Int64 = 0,
        sampled: Int64 = 0,
        admitted: Int64 = 0,
        dropReasons: [StatsCountRow] = [],
        classifier: PolicyStatsClassifier = .init(),
        actualTiers: PolicyStatsTierCounts = .init(),
        shadowTiers: PolicyStatsTierCounts = .init(),
        classifierLatency: PolicyStatsLatency = .init(),
        classifierUsage: PolicyStatsTokenUsage = .init(),
        physicalClassifierSends: Int64 = 0
    ) {
        self.eligible = eligible
        self.sampled = sampled
        self.admitted = admitted
        self.dropReasons = dropReasons
        self.classifier = classifier
        self.actualTiers = actualTiers
        self.shadowTiers = shadowTiers
        self.classifierLatency = classifierLatency
        self.classifierUsage = classifierUsage
        self.physicalClassifierSends = physicalClassifierSends
    }

    private enum CodingKeys: String, CodingKey {
        case eligible, sampled, admitted
        case dropReasons = "drop_reasons"
        case classifier
        case actualTiers = "actual_tiers"
        case shadowTiers = "shadow_tiers"
        case classifierLatency = "classifier_latency"
        case classifierUsage = "classifier_usage"
        case physicalClassifierSends = "physical_classifier_sends"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        eligible = container.decodeLossyInt64(forKey: .eligible)
        sampled = container.decodeLossyInt64(forKey: .sampled)
        admitted = container.decodeLossyInt64(forKey: .admitted)
        dropReasons = container.decodeLossyArray(StatsCountRow.self, forKey: .dropReasons)
        classifier = container.decodeDefault(PolicyStatsClassifier.self, forKey: .classifier, defaultValue: .init())
        actualTiers = container.decodeDefault(PolicyStatsTierCounts.self, forKey: .actualTiers, defaultValue: .init())
        shadowTiers = container.decodeDefault(PolicyStatsTierCounts.self, forKey: .shadowTiers, defaultValue: .init())
        classifierLatency = container.decodeDefault(
            PolicyStatsLatency.self,
            forKey: .classifierLatency,
            defaultValue: .init()
        )
        classifierUsage = container.decodeDefault(
            PolicyStatsTokenUsage.self,
            forKey: .classifierUsage,
            defaultValue: .init()
        )
        physicalClassifierSends = container.decodeLossyInt64(forKey: .physicalClassifierSends)
    }
}

public struct PolicyStatsClassifier: Codable, Equatable, Sendable {
    public var completion: Int64
    public var unavailable: Int64
    public var uncertain: Int64
    public var abstain: Int64

    public init(completion: Int64 = 0, unavailable: Int64 = 0, uncertain: Int64 = 0, abstain: Int64 = 0) {
        self.completion = completion
        self.unavailable = unavailable
        self.uncertain = uncertain
        self.abstain = abstain
    }

    private enum CodingKeys: String, CodingKey { case completion, unavailable, uncertain, abstain }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        completion = container.decodeLossyInt64(forKey: .completion)
        unavailable = container.decodeLossyInt64(forKey: .unavailable)
        uncertain = container.decodeLossyInt64(forKey: .uncertain)
        abstain = container.decodeLossyInt64(forKey: .abstain)
    }
}

public struct PolicyStatsTierCounts: Codable, Equatable, Sendable {
    public var lightweight: Int64
    public var powerful: Int64
    public var unknown: Int64

    public init(lightweight: Int64 = 0, powerful: Int64 = 0, unknown: Int64 = 0) {
        self.lightweight = lightweight
        self.powerful = powerful
        self.unknown = unknown
    }

    private enum CodingKeys: String, CodingKey { case lightweight, powerful, unknown }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        lightweight = container.decodeLossyInt64(forKey: .lightweight)
        powerful = container.decodeLossyInt64(forKey: .powerful)
        unknown = container.decodeLossyInt64(forKey: .unknown)
    }
}

public struct PolicyStatsLatency: Codable, Equatable, Sendable {
    public var count: Int64
    public var recentSamples: Int
    public var averageMilliseconds: Int64
    public var minimumMilliseconds: Int64
    public var maximumMilliseconds: Int64
    public var p50Milliseconds: Int64
    public var p95Milliseconds: Int64
    public var p99Milliseconds: Int64

    public init(
        count: Int64 = 0,
        recentSamples: Int = 0,
        averageMilliseconds: Int64 = 0,
        minimumMilliseconds: Int64 = 0,
        maximumMilliseconds: Int64 = 0,
        p50Milliseconds: Int64 = 0,
        p95Milliseconds: Int64 = 0,
        p99Milliseconds: Int64 = 0
    ) {
        self.count = count
        self.recentSamples = recentSamples
        self.averageMilliseconds = averageMilliseconds
        self.minimumMilliseconds = minimumMilliseconds
        self.maximumMilliseconds = maximumMilliseconds
        self.p50Milliseconds = p50Milliseconds
        self.p95Milliseconds = p95Milliseconds
        self.p99Milliseconds = p99Milliseconds
    }

    private enum CodingKeys: String, CodingKey {
        case count
        case recentSamples = "recent_samples"
        case averageMilliseconds = "avg_ms"
        case minimumMilliseconds = "min_ms"
        case maximumMilliseconds = "max_ms"
        case p50Milliseconds = "p50_ms"
        case p95Milliseconds = "p95_ms"
        case p99Milliseconds = "p99_ms"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        count = container.decodeLossyInt64(forKey: .count)
        recentSamples = container.decodeLossyInt(forKey: .recentSamples)
        averageMilliseconds = container.decodeLossyInt64(forKey: .averageMilliseconds)
        minimumMilliseconds = container.decodeLossyInt64(forKey: .minimumMilliseconds)
        maximumMilliseconds = container.decodeLossyInt64(forKey: .maximumMilliseconds)
        p50Milliseconds = container.decodeLossyInt64(forKey: .p50Milliseconds)
        p95Milliseconds = container.decodeLossyInt64(forKey: .p95Milliseconds)
        p99Milliseconds = container.decodeLossyInt64(forKey: .p99Milliseconds)
    }
}

public struct PolicyStatsTokenUsage: Codable, Equatable, Sendable {
    public var inputTokens: Int64
    public var outputTokens: Int64
    public var totalTokens: Int64
    public var cachedInputTokens: Int64
    public var reasoningTokens: Int64

    public init(
        inputTokens: Int64 = 0,
        outputTokens: Int64 = 0,
        totalTokens: Int64 = 0,
        cachedInputTokens: Int64 = 0,
        reasoningTokens: Int64 = 0
    ) {
        self.inputTokens = inputTokens
        self.outputTokens = outputTokens
        self.totalTokens = totalTokens
        self.cachedInputTokens = cachedInputTokens
        self.reasoningTokens = reasoningTokens
    }

    private enum CodingKeys: String, CodingKey {
        case inputTokens = "input_tokens"
        case outputTokens = "output_tokens"
        case totalTokens = "total_tokens"
        case cachedInputTokens = "cached_input_tokens"
        case reasoningTokens = "reasoning_tokens"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        inputTokens = container.decodeLossyInt64(forKey: .inputTokens)
        outputTokens = container.decodeLossyInt64(forKey: .outputTokens)
        totalTokens = container.decodeLossyInt64(forKey: .totalTokens)
        cachedInputTokens = container.decodeLossyInt64(forKey: .cachedInputTokens)
        reasoningTokens = container.decodeLossyInt64(forKey: .reasoningTokens)
    }
}
