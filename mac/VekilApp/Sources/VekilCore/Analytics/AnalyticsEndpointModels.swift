import Foundation

/// Parsed body from `GET /readyz`. A non-2xx response can still be a valid
/// readiness result (normally `{ "status": "not_ready", "error": ... }`).
public struct ReadinessResponse: Codable, Equatable, Sendable {
    public var status: String
    public var error: String
    public var httpStatusCode: Int

    public init(status: String = "", error: String = "", httpStatusCode: Int = 0) {
        self.status = status
        self.error = error
        self.httpStatusCode = httpStatusCode
    }

    public var isReady: Bool {
        (200..<300).contains(httpStatusCode) && status.lowercased() == "ready"
    }

    private enum CodingKeys: String, CodingKey { case status, error }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        status = container.decodeDefault(String.self, forKey: .status, defaultValue: "")
        error = container.decodeDefault(String.self, forKey: .error, defaultValue: "")
        httpStatusCode = 0
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(status, forKey: .status)
        if !error.isEmpty {
            try container.encode(error, forKey: .error)
        }
    }
}

/// Tolerant OpenAI-compatible model catalog returned by `GET /v1/models`.
public struct RuntimeModelCatalog: Codable, Equatable, Sendable {
    public var object: String
    public var data: [RuntimeModel]

    public init(object: String = "list", data: [RuntimeModel] = []) {
        self.object = object
        self.data = data
    }

    private enum CodingKeys: String, CodingKey { case object, data }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        object = container.decodeDefault(String.self, forKey: .object, defaultValue: "list")
        data = container.decodeLossyArray(RuntimeModel.self, forKey: .data)
    }
}

public struct RuntimeModel: Codable, Equatable, Sendable {
    public var id: String
    public var object: String
    public var created: Int64
    public var ownedBy: String
    public var name: String
    public var supportedEndpoints: [String]
    public var capabilities: RuntimeModelCapabilities?
    public var modelPickerEnabled: Bool?
    public var modelPickerCategory: String
    public var contextWindow: Int64?
    public var maximumContextWindow: Int64?
    public var autoCompactTokenLimit: Int64?
    public var effectiveContextWindowPercent: Int64?

    public init(
        id: String = "",
        object: String = "model",
        created: Int64 = 0,
        ownedBy: String = "",
        name: String = "",
        supportedEndpoints: [String] = [],
        capabilities: RuntimeModelCapabilities? = nil,
        modelPickerEnabled: Bool? = nil,
        modelPickerCategory: String = "",
        contextWindow: Int64? = nil,
        maximumContextWindow: Int64? = nil,
        autoCompactTokenLimit: Int64? = nil,
        effectiveContextWindowPercent: Int64? = nil
    ) {
        self.id = id
        self.object = object
        self.created = created
        self.ownedBy = ownedBy
        self.name = name
        self.supportedEndpoints = supportedEndpoints
        self.capabilities = capabilities
        self.modelPickerEnabled = modelPickerEnabled
        self.modelPickerCategory = modelPickerCategory
        self.contextWindow = contextWindow
        self.maximumContextWindow = maximumContextWindow
        self.autoCompactTokenLimit = autoCompactTokenLimit
        self.effectiveContextWindowPercent = effectiveContextWindowPercent
    }

    private enum CodingKeys: String, CodingKey {
        case id, object, created
        case ownedBy = "owned_by"
        case name
        case supportedEndpoints = "supported_endpoints"
        case capabilities
        case modelPickerEnabled = "model_picker_enabled"
        case modelPickerCategory = "model_picker_category"
        case contextWindow = "context_window"
        case maximumContextWindow = "max_context_window"
        case autoCompactTokenLimit = "auto_compact_token_limit"
        case effectiveContextWindowPercent = "effective_context_window_percent"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = container.decodeDefault(String.self, forKey: .id, defaultValue: "")
        object = container.decodeDefault(String.self, forKey: .object, defaultValue: "model")
        created = container.decodeLossyInt64(forKey: .created)
        ownedBy = container.decodeDefault(String.self, forKey: .ownedBy, defaultValue: "")
        name = container.decodeDefault(String.self, forKey: .name, defaultValue: "")
        supportedEndpoints = container.decodeLossyArray(String.self, forKey: .supportedEndpoints)
        capabilities = try? container.decode(RuntimeModelCapabilities.self, forKey: .capabilities)
        modelPickerEnabled = container.decodeLossyOptionalBool(forKey: .modelPickerEnabled)
        modelPickerCategory = container.decodeDefault(String.self, forKey: .modelPickerCategory, defaultValue: "")
        contextWindow = container.decodeLossyOptionalInt64(forKey: .contextWindow)
        maximumContextWindow = container.decodeLossyOptionalInt64(forKey: .maximumContextWindow)
        autoCompactTokenLimit = container.decodeLossyOptionalInt64(forKey: .autoCompactTokenLimit)
        effectiveContextWindowPercent = container.decodeLossyOptionalInt64(forKey: .effectiveContextWindowPercent)
    }
}

public struct RuntimeModelCapabilities: Codable, Equatable, Sendable {
    public var limits: RuntimeModelLimits
    public var supports: RuntimeModelSupports

    public init(limits: RuntimeModelLimits = .init(), supports: RuntimeModelSupports = .init()) {
        self.limits = limits
        self.supports = supports
    }

    private enum CodingKeys: String, CodingKey { case limits, supports }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        limits = container.decodeDefault(RuntimeModelLimits.self, forKey: .limits, defaultValue: .init())
        supports = container.decodeDefault(RuntimeModelSupports.self, forKey: .supports, defaultValue: .init())
    }
}

public struct RuntimeModelLimits: Codable, Equatable, Sendable {
    public var maximumContextWindowTokens: Int64
    public var contextWindow: Int64
    public var contextWindowTokens: Int64
    public var maximumPromptTokens: Int64
    public var maximumPrompt: Int64
    public var maximumInputTokens: Int64

    public init(
        maximumContextWindowTokens: Int64 = 0,
        contextWindow: Int64 = 0,
        contextWindowTokens: Int64 = 0,
        maximumPromptTokens: Int64 = 0,
        maximumPrompt: Int64 = 0,
        maximumInputTokens: Int64 = 0
    ) {
        self.maximumContextWindowTokens = maximumContextWindowTokens
        self.contextWindow = contextWindow
        self.contextWindowTokens = contextWindowTokens
        self.maximumPromptTokens = maximumPromptTokens
        self.maximumPrompt = maximumPrompt
        self.maximumInputTokens = maximumInputTokens
    }

    private enum CodingKeys: String, CodingKey {
        case maximumContextWindowTokens = "max_context_window_tokens"
        case contextWindow = "context_window"
        case contextWindowTokens = "context_window_tokens"
        case maximumPromptTokens = "max_prompt_tokens"
        case maximumPrompt = "max_prompt"
        case maximumInputTokens = "max_input_tokens"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        maximumContextWindowTokens = container.decodeLossyInt64(forKey: .maximumContextWindowTokens)
        contextWindow = container.decodeLossyInt64(forKey: .contextWindow)
        contextWindowTokens = container.decodeLossyInt64(forKey: .contextWindowTokens)
        maximumPromptTokens = container.decodeLossyInt64(forKey: .maximumPromptTokens)
        maximumPrompt = container.decodeLossyInt64(forKey: .maximumPrompt)
        maximumInputTokens = container.decodeLossyInt64(forKey: .maximumInputTokens)
    }
}

public struct RuntimeModelSupports: Codable, Equatable, Sendable {
    public var parallelToolCalls: Bool
    public var reasoningEffort: [String]
    public var vision: Bool
    public var toolCalls: Bool

    public init(
        parallelToolCalls: Bool = false,
        reasoningEffort: [String] = [],
        vision: Bool = false,
        toolCalls: Bool = false
    ) {
        self.parallelToolCalls = parallelToolCalls
        self.reasoningEffort = reasoningEffort
        self.vision = vision
        self.toolCalls = toolCalls
    }

    private enum CodingKeys: String, CodingKey {
        case parallelToolCalls = "parallel_tool_calls"
        case reasoningEffort = "reasoning_effort"
        case vision
        case toolCalls = "tool_calls"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        parallelToolCalls = container.decodeLossyBool(forKey: .parallelToolCalls)
        reasoningEffort = container.decodeLossyArray(String.self, forKey: .reasoningEffort)
        vision = container.decodeLossyBool(forKey: .vision)
        toolCalls = container.decodeLossyBool(forKey: .toolCalls)
    }
}
