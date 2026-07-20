"use strict";

(function initializeVekilConfigEditor(globalScope) {
  var API_ROOT = "/dashboard/api/v1/config";
  var APPLY_POLL_INITIAL_MS = 650;
  var APPLY_POLL_MAX_MS = 2500;

  var PROVIDER_TYPES = [
    "copilot",
    "azure-openai",
    "openai-codex",
    "openai-compatible",
    "anthropic-compatible"
  ];

  var PROVIDER_LABELS = {
    "copilot": "GitHub Copilot",
    "azure-openai": "Azure OpenAI",
    "openai-codex": "OpenAI Codex",
    "openai-compatible": "OpenAI-compatible",
    "anthropic-compatible": "Anthropic-compatible"
  };

  var COMMON_PROVIDER_FIELDS = [
    "id",
    "type",
    "default",
    "include_models",
    "exclude_models",
    "trust_domain",
    "classifier_no_store_supported"
  ];

  var PROVIDER_FIELDS_BY_TYPE = {
    "copilot": ["headers"],
    "azure-openai": [
      "base_url",
      "auth_mode",
      "api_key",
      "api_key_env",
      "api_version",
      "token_scope",
      "models"
    ],
    "openai-codex": ["base_url"],
    "openai-compatible": [
      "base_url",
      "auth_type",
      "auth_header",
      "auth_prefix",
      "api_key",
      "api_key_env",
      "extra_headers",
      "chat_completions_path",
      "responses_path",
      "models_path",
      "model_discovery",
      "models"
    ],
    "anthropic-compatible": [
      "base_url",
      "auth_type",
      "auth_header",
      "auth_prefix",
      "api_key",
      "api_key_env",
      "extra_headers",
      "messages_path",
      "models_path",
      "model_discovery",
      "models"
    ]
  };

  var KNOWN_PROVIDER_FIELDS = COMMON_PROVIDER_FIELDS.concat([
    "base_url",
    "auth_mode",
    "api_key",
    "api_key_env",
    "api_version",
    "token_scope",
    "auth_type",
    "auth_header",
    "auth_prefix",
    "extra_headers",
    "chat_completions_path",
    "responses_path",
    "messages_path",
    "models_path",
    "model_discovery",
    "headers",
    "models"
  ]);

  var COPILOT_HEADER_PROFILES = [
    ["default", "Default"],
    ["chat_completions", "Chat completions"],
    ["responses", "Responses"]
  ];

  var COPILOT_HEADER_FIELDS = [
    ["editor_version", "Editor version"],
    ["editor_plugin_version", "Editor plugin version"],
    ["user_agent", "User agent"],
    ["copilot_integration_id", "Copilot integration ID"],
    ["github_api_version", "GitHub API version"],
    ["openai_intent", "OpenAI intent"]
  ];

  var POLICY_STEPS = [
    "Identity and mode",
    "Routes and fallbacks",
    "Classifier limits",
    "Data acknowledgements"
  ];

  var TERMINAL_APPLY_STATES = [
    "succeeded",
    "failed_decode",
    "failed_revision",
    "failed_validation",
    "failed_discovery",
    "failed_preflight",
    "failed_encoding",
    "failed_persistence",
    "timed_out",
    "canceled",
    "canceled_shutdown"
  ];

  function isPlainObject(value) {
    return value !== null && typeof value === "object" && !Array.isArray(value);
  }

  function hasOwn(object, key) {
    return Object.prototype.hasOwnProperty.call(object || {}, key);
  }

  function deepClone(value) {
    if (value === undefined) return undefined;
    return JSON.parse(JSON.stringify(value));
  }

  function jsonPointerEscape(value) {
    return String(value).replace(/~/g, "~0").replace(/\//g, "~1");
  }

  function jsonPointerUnescape(value) {
    return String(value).replace(/~1/g, "/").replace(/~0/g, "~");
  }

  function normalizeConfigPath(path) {
    if (path === undefined || path === null) return "/";
    var text = String(path).trim();
    if (!text || text === "$" || text === "config") return "/";
    if (text.charAt(0) === "/") return text;
    text = text.replace(/^\$\.?/, "").replace(/^config\.?/, "");
    text = text.replace(/\[(\d+)\]/g, ".$1");
    text = text.replace(/\[['"]([^'"]+)['"]\]/g, ".$1");
    var segments = text.split(".").filter(function (segment) { return segment !== ""; });
    return "/" + segments.map(jsonPointerEscape).join("/");
  }

  function pointerSegments(path) {
    var normalized = normalizeConfigPath(path);
    if (normalized === "/") return [];
    return normalized.slice(1).split("/").map(jsonPointerUnescape);
  }

  function getByPointer(object, path) {
    var current = object;
    var segments = pointerSegments(path);
    for (var i = 0; i < segments.length; i++) {
      if (current === null || current === undefined || !hasOwn(current, segments[i])) {
        return { found: false, value: undefined };
      }
      current = current[segments[i]];
    }
    return { found: true, value: current };
  }

  function setByPointer(object, path, value) {
    var segments = pointerSegments(path);
    if (!segments.length) return deepClone(value);
    var current = object;
    for (var i = 0; i < segments.length - 1; i++) {
      var segment = segments[i];
      var next = segments[i + 1];
      if (current[segment] === null || typeof current[segment] !== "object") {
        current[segment] = /^\d+$/.test(next) ? [] : {};
      }
      current = current[segment];
    }
    current[segments[segments.length - 1]] = deepClone(value);
    return object;
  }

  function deleteByPointer(object, path) {
    var segments = pointerSegments(path);
    if (!segments.length) return object;
    var current = object;
    for (var i = 0; i < segments.length - 1; i++) {
      if (current === null || current === undefined || !hasOwn(current, segments[i])) return object;
      current = current[segments[i]];
    }
    if (current !== null && current !== undefined) {
      if (Array.isArray(current) && /^\d+$/.test(segments[segments.length - 1])) {
        current.splice(Number(segments[segments.length - 1]), 1);
      } else {
        delete current[segments[segments.length - 1]];
      }
    }
    return object;
  }

  function restorePreservedPaths(candidate, base, preservedPaths) {
    var restored = deepClone(candidate || {});
    var paths = Array.isArray(preservedPaths) ? preservedPaths : [];
    paths.forEach(function (rawPath) {
      var path = normalizeConfigPath(rawPath);
      if (path.indexOf("*") !== -1) return;
      var baseValue = getByPointer(base || {}, path);
      if (baseValue.found) {
        restored = setByPointer(restored, path, baseValue.value);
      } else {
        deleteByPointer(restored, path);
      }
    });
    return restored;
  }

  function stripSecretValues(config) {
    var clean = deepClone(config || {});
    var stripped = 0;
    var providers = Array.isArray(clean.providers) ? clean.providers : [];
    providers.forEach(function (provider) {
      if (!isPlainObject(provider)) return;
      if (hasOwn(provider, "api_key")) {
        if (provider.api_key !== "" && provider.api_key !== null && provider.api_key !== undefined) stripped++;
        delete provider.api_key;
      }
      if (isPlainObject(provider.extra_headers)) {
        Object.keys(provider.extra_headers).forEach(function (name) {
          if (provider.extra_headers[name] !== "" && provider.extra_headers[name] !== null && provider.extra_headers[name] !== undefined) stripped++;
          provider.extra_headers[name] = "";
        });
      }
    });
    return { config: clean, stripped: stripped };
  }

  function hasSchemaV2Features(config) {
    if (!isPlainObject(config)) return false;
    if (Array.isArray(config.policy_profiles) && config.policy_profiles.length > 0) return true;
    var providers = Array.isArray(config.providers) ? config.providers : [];
    for (var i = 0; i < providers.length; i++) {
      var provider = providers[i];
      if (isPlainObject(provider) && (hasOwn(provider, "trust_domain") || hasOwn(provider, "classifier_no_store_supported"))) return true;
    }
    var routes = Array.isArray(config.model_routes) ? config.model_routes : [];
    for (var j = 0; j < routes.length; j++) {
      var route = routes[j];
      if (isPlainObject(route) && (hasOwn(route, "exposure") || hasOwn(route, "internal_purpose"))) return true;
    }
    return false;
  }

  function enforceSchemaVersion(config, baseSchemaVersion, forceV2) {
    var next = deepClone(config || {});
    var baseVersion = Number(baseSchemaVersion) || 0;
    var currentVersion = Number(next.schema_version) || 0;
    var shouldBeV2 = baseVersion >= 2 || Boolean(forceV2) || hasSchemaV2Features(next);
    var promoted = false;
    if (shouldBeV2 && currentVersion < 2) {
      next.schema_version = 2;
      promoted = true;
    }
    return { config: next, promoted: promoted };
  }

  function serializeConfigDraft(config, baseConfig, preservedPaths, baseSchemaVersion, forceV2) {
    var secrets = stripSecretValues(config || {});
    var restored = restorePreservedPaths(secrets.config, baseConfig || {}, preservedPaths || []);
    var versioned = enforceSchemaVersion(restored, baseSchemaVersion, forceV2);
    return {
      config: versioned.config,
      promoted: versioned.promoted,
      strippedSecrets: secrets.stripped
    };
  }

  function moveArrayItem(items, fromIndex, toIndex) {
    var copy = Array.isArray(items) ? items.slice() : [];
    if (fromIndex < 0 || fromIndex >= copy.length || toIndex < 0 || toIndex >= copy.length || fromIndex === toIndex) return copy;
    var item = copy.splice(fromIndex, 1)[0];
    copy.splice(toIndex, 0, item);
    return copy;
  }

  function parseList(value) {
    return String(value || "")
      .split(/[\n,]/)
      .map(function (item) { return item.trim(); })
      .filter(function (item) { return item !== ""; });
  }

  function providerSecretPath(providerID) {
    return "/providers/" + jsonPointerEscape(String(providerID || "")) + "/api_key";
  }

  function extraHeaderSecretPath(providerID, headerName) {
    return "/providers/" + jsonPointerEscape(String(providerID || "")) + "/extra_headers/" + jsonPointerEscape(String(headerName || ""));
  }

  function providerSecretFingerprint(provider) {
    var source = isPlainObject(provider) ? provider : {};
    function trimmed(field) {
      return source[field] === undefined || source[field] === null ? "" : String(source[field]).trim();
    }
    return JSON.stringify({
      id: trimmed("id"),
      type: trimmed("type"),
      base_url: trimmed("base_url").replace(/\/+$/, ""),
      auth_mode: trimmed("auth_mode"),
      api_key_env: trimmed("api_key_env"),
      auth_type: trimmed("auth_type"),
      auth_header: trimmed("auth_header"),
      auth_prefix: trimmed("auth_prefix"),
      api_version: trimmed("api_version"),
      token_scope: trimmed("token_scope")
    });
  }

  function buildSecretOperations(entries) {
    var errors = [];
    var operations = new Map();
    (Array.isArray(entries) ? entries : []).forEach(function (entry) {
      if (!entry || !entry.path) return;
      var operation = String(entry.operation || "").trim();
      var normalizedPath = normalizeConfigPath(entry.path);
      if (["keep", "set", "clear"].indexOf(operation) === -1) {
        errors.push({ path: entry.fieldPath || normalizedPath, message: "Choose keep, set, or clear for this secret." });
        return;
      }
      if (operation === "keep" && entry.canKeep === false) {
        errors.push({ path: entry.fieldPath || normalizedPath, message: "This secret cannot be kept after the provider identity or destination changed. Set a new value or explicitly clear it." });
        return;
      }
      var serialized = { path: normalizedPath, operation: operation };
      if (operation === "set") {
        if (typeof entry.value !== "string" || entry.value.length === 0 || entry.value === "***") {
          errors.push({ path: entry.fieldPath || normalizedPath, message: "Enter a non-placeholder secret value before choosing set." });
          return;
        }
        serialized.value = entry.value;
      }
      operations.set(normalizedPath, serialized);
    });
    return {
      operations: Array.from(operations.values()).sort(function (a, b) { return a.path.localeCompare(b.path); }),
      errors: errors
    };
  }

  function deriveWriteCapability(capability) {
    if (capability === true) return true;
    if (capability === false || capability === null || capability === undefined) return false;
    if (typeof capability === "string") {
      return ["enabled", "available", "writable", "read-write", "read_write", "write"].indexOf(capability.toLowerCase()) !== -1;
    }
    if (!isPlainObject(capability)) return false;
    var direct = ["writable", "can_write", "write_enabled", "available"];
    for (var i = 0; i < direct.length; i++) {
      if (typeof capability[direct[i]] === "boolean") return capability[direct[i]];
    }
    var mode = capability.mode || capability.access || capability.level || capability.status;
    return deriveWriteCapability(mode);
  }

  function capabilityReason(capability) {
    if (!isPlainObject(capability)) return "";
    return capability.reason || capability.disabled_reason || capability.message || capability.detail || "";
  }

  function isTerminalApplyStatus(status) {
    var normalized = String(status || "").toLowerCase();
    return TERMINAL_APPLY_STATES.indexOf(normalized) !== -1 || normalized.indexOf("failed_") === 0 || normalized.indexOf("canceled") === 0;
  }

  function isSuccessfulApplyStatus(status) {
    return String(status || "").toLowerCase() === "succeeded";
  }

  function readFirst(object, keys, fallback) {
    if (!object) return fallback;
    for (var i = 0; i < keys.length; i++) {
      if (object[keys[i]] !== undefined && object[keys[i]] !== null && object[keys[i]] !== "") return object[keys[i]];
    }
    return fallback;
  }

  function formatValue(value) {
    if (value === undefined || value === null || value === "") return "—";
    if (typeof value === "boolean") return value ? "Yes" : "No";
    if (typeof value === "string" || typeof value === "number") return String(value);
    try {
      return JSON.stringify(value);
    } catch (_error) {
      return String(value);
    }
  }

  function safeIdentifier(value, fallback) {
    var normalized = String(value || "").trim().replace(/[^A-Za-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "");
    return normalized || fallback;
  }

  var exportedHelpers = Object.freeze({
    deepClone: deepClone,
    jsonPointerEscape: jsonPointerEscape,
    jsonPointerUnescape: jsonPointerUnescape,
    normalizeConfigPath: normalizeConfigPath,
    getByPointer: getByPointer,
    setByPointer: setByPointer,
    deleteByPointer: deleteByPointer,
    restorePreservedPaths: restorePreservedPaths,
    stripSecretValues: stripSecretValues,
    hasSchemaV2Features: hasSchemaV2Features,
    enforceSchemaVersion: enforceSchemaVersion,
    serializeConfigDraft: serializeConfigDraft,
    moveArrayItem: moveArrayItem,
    parseList: parseList,
    providerSecretPath: providerSecretPath,
    extraHeaderSecretPath: extraHeaderSecretPath,
    providerSecretFingerprint: providerSecretFingerprint,
    buildSecretOperations: buildSecretOperations,
    deriveWriteCapability: deriveWriteCapability,
    isTerminalApplyStatus: isTerminalApplyStatus,
    isSuccessfulApplyStatus: isSuccessfulApplyStatus
  });

  if (typeof module !== "undefined" && module.exports) {
    module.exports = Object.freeze(Object.assign({}, exportedHelpers, { ConfigEditor: ConfigEditor }));
  }
  globalScope.VekilConfigHelpers = exportedHelpers;

  function ConfigEditor(doc) {
    this.document = doc;
    this.state = {
      response: null,
      configAvailable: false,
      baseConfig: null,
      draft: null,
      revision: "",
      etag: "",
      csrfToken: "",
      source: null,
      capability: null,
      policy: null,
      secretStates: [],
      preservedPaths: [],
      providerCapabilities: null,
      canWrite: false,
      canValidate: false,
      dirty: false,
      rawDirty: false,
      forceV2: false,
      baseSchemaVersion: 0,
      stale: false,
      pendingApply: false,
      activeApply: null,
      currentTab: "providers",
      pollToken: 0
    };
    this.providerMeta = new WeakMap();
    this.policyMeta = new WeakMap();
    this.secretStateByPath = new Map();
    this.rawSyncHandle = null;
    this.fieldSequence = 0;
  }

  ConfigEditor.prototype.byId = function (id) {
    return this.document.getElementById(id);
  };

  ConfigEditor.prototype.init = function () {
    this.bindStaticEvents();
    this.selectTab("providers", false);
    this.loadConfig();
  };

  ConfigEditor.prototype.bindStaticEvents = function () {
    var self = this;
    var tabs = Array.from(this.document.querySelectorAll('[role="tab"][data-tab]'));
    tabs.forEach(function (tab) {
      tab.addEventListener("click", function () { self.selectTab(tab.dataset.tab, true); });
    });
    var tabList = this.document.querySelector('[role="tablist"]');
    if (tabList) {
      tabList.addEventListener("keydown", function (event) { self.handleTabKeydown(event, tabs); });
    }

    this.byId("configForm").addEventListener("submit", function (event) { event.preventDefault(); });
    this.byId("addProviderButton").addEventListener("click", function () { self.addProvider(); });
    this.byId("addRouteButton").addEventListener("click", function () { self.addRoute(); });
    this.byId("addPolicyButton").addEventListener("click", function () { self.addPolicy(); });
    this.byId("reloadButton").addEventListener("click", function () { self.reloadRequested(); });
    this.byId("validateButton").addEventListener("click", function () { self.validateDraft(); });
    this.byId("applyButton").addEventListener("click", function () { self.applyDraft(); });
    this.byId("resetButton").addEventListener("click", function () { self.openResetDialog(); });
    this.byId("formatRawButton").addEventListener("click", function () { self.formatRawEditor(); });
    this.byId("updateFromRawButton").addEventListener("click", function () { self.commitRawEditor(true); });
    this.byId("copyRawButton").addEventListener("click", function () { self.copyRawEditor(); });
    this.byId("rawConfig").addEventListener("input", function () {
      self.state.rawDirty = true;
      self.state.dirty = true;
      self.clearErrors();
      self.updateActionState();
    });
    this.byId("rawFileInput").addEventListener("change", function (event) { self.importRawFile(event); });

    var resetDialog = this.byId("resetDialog");
    resetDialog.addEventListener("close", function () {
      if (resetDialog.returnValue === "confirm") self.resetManaged();
    });

    globalScope.addEventListener("beforeunload", function (event) {
      if (!self.state.dirty && !self.state.pendingApply) return;
      event.preventDefault();
      event.returnValue = "";
    });
  };

  ConfigEditor.prototype.handleTabKeydown = function (event, tabs) {
    var current = tabs.indexOf(event.target);
    if (current === -1) return;
    var next = current;
    if (event.key === "ArrowRight") next = (current + 1) % tabs.length;
    else if (event.key === "ArrowLeft") next = (current - 1 + tabs.length) % tabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = tabs.length - 1;
    else return;
    event.preventDefault();
    if (this.selectTab(tabs[next].dataset.tab, true)) tabs[next].focus();
  };

  ConfigEditor.prototype.selectTab = function (name, commitRaw) {
    if (commitRaw && this.state.currentTab === "raw" && name !== "raw" && this.state.rawDirty) {
      if (!this.commitRawEditor(true)) return false;
    }
    var tabs = Array.from(this.document.querySelectorAll('[role="tab"][data-tab]'));
    var panels = Array.from(this.document.querySelectorAll('[role="tabpanel"][data-panel]'));
    tabs.forEach(function (tab) {
      var selected = tab.dataset.tab === name;
      tab.setAttribute("aria-selected", selected ? "true" : "false");
      tab.tabIndex = selected ? 0 : -1;
    });
    panels.forEach(function (panel) { panel.hidden = panel.dataset.panel !== name; });
    this.state.currentTab = name;
    if (name === "raw" && !this.state.rawDirty) this.syncRawEditor();
    return true;
  };

  ConfigEditor.prototype.loadConfig = async function () {
    this.state.pollToken++;
    this.announce("Loading active configuration…");
    this.clearErrors();
    try {
      var response = await fetch(API_ROOT, {
        method: "GET",
        headers: { "Accept": "application/json" },
        cache: "no-store",
        credentials: "same-origin"
      });
      var payload = await this.readJSONResponse(response);
      if (!response.ok && !payload) throw new Error("Configuration request failed with HTTP " + response.status + ".");
      this.applyLoadedResponse(payload || {}, response.headers.get("ETag") || "");
      if (response.ok) {
        this.announce(this.state.configAvailable ? "Active configuration loaded." : "Configuration capability loaded; the provider document is unavailable in this server mode.");
      } else {
        this.showErrors(this.extractErrors(payload, response.status));
        this.announce("Configuration editing is unavailable.");
      }
    } catch (error) {
      this.state.configAvailable = false;
      this.state.canWrite = false;
      this.state.canValidate = false;
      this.renderAll();
      this.showErrors([{ path: "/", message: error.message || "Unable to load the active configuration." }]);
      this.announce("Unable to load the active configuration.");
    }
  };

  ConfigEditor.prototype.applyLoadedResponse = function (payload, etag) {
    this.state.response = payload;
    this.state.capability = payload.capability;
    this.state.source = payload.source || null;
    this.state.policy = payload.policy || {};
    this.state.revision = String(payload.revision || "");
    this.state.etag = etag || this.state.revision;
    this.state.csrfToken = String(payload.csrf_token || "");
    this.state.secretStates = Array.isArray(payload.secret_states) ? payload.secret_states.slice() : [];
    this.state.preservedPaths = Array.isArray(payload.preserved_paths) ? payload.preserved_paths.slice() : [];
    this.state.providerCapabilities = payload.provider_capabilities || null;
    this.state.policyEligibility = payload.policy_eligibility || null;
    this.state.configAvailable = isPlainObject(payload.config);

    var sanitized = stripSecretValues(this.state.configAvailable ? payload.config : { providers: [] });
    this.state.baseConfig = sanitized.config;
    this.state.draft = deepClone(sanitized.config);
    this.state.baseSchemaVersion = Number(payload.schema_version || this.state.baseConfig.schema_version) || 0;
    this.state.forceV2 = this.state.baseSchemaVersion >= 2;
    this.state.dirty = false;
    this.state.rawDirty = false;
    this.state.stale = false;
    this.state.pendingApply = false;
    this.state.activeApply = null;

    var capabilityAllowsWrite = deriveWriteCapability(this.state.capability);
    this.state.canWrite = this.state.configAvailable && capabilityAllowsWrite && Boolean(this.state.csrfToken);
    var explicitValidate = isPlainObject(this.state.capability) && typeof this.state.capability.can_validate === "boolean"
      ? this.state.capability.can_validate
      : this.state.configAvailable;
    this.state.canValidate = Boolean(explicitValidate) && capabilityAllowsWrite && this.state.configAvailable && Boolean(this.state.csrfToken);

    this.initializeMetadata();
    this.renderAll();
  };

  ConfigEditor.prototype.initializeMetadata = function () {
    var self = this;
    this.providerMeta = new WeakMap();
    this.policyMeta = new WeakMap();
    this.secretStateByPath = new Map();
    this.state.secretStates.forEach(function (secretState) {
      if (secretState && secretState.path) self.secretStateByPath.set(normalizeConfigPath(secretState.path), secretState);
    });

    var baseProviders = this.state.baseConfig && Array.isArray(this.state.baseConfig.providers) ? this.state.baseConfig.providers : [];
    var providers = this.state.draft && Array.isArray(this.state.draft.providers) ? this.state.draft.providers : [];
    providers.forEach(function (provider, index) {
      if (!isPlainObject(provider)) return;
      var origin = baseProviders.find(function (candidate) {
        return isPlainObject(candidate) && String(candidate.id || "") === String(provider.id || "");
      }) || null;
      var apiState = self.findSecretState(provider, index, "api_key");
      var headerNames = new Set();
      if (isPlainObject(provider.extra_headers)) Object.keys(provider.extra_headers).forEach(function (name) { headerNames.add(name); });
      self.state.secretStates.forEach(function (secretState) {
        var parsed = self.parseExtraHeaderSecretState(secretState, provider, index);
        if (parsed) headerNames.add(parsed.name);
      });
      var headerRows = Array.from(headerNames).sort().map(function (name) {
        var secretState = self.findExtraHeaderSecretState(provider, index, name);
        return {
          name: name,
          originName: secretState ? name : null,
          originalPath: secretState ? normalizeConfigPath(secretState.path) : "",
          operation: secretState ? "keep" : "clear",
          value: ""
        };
      });
      if (isPlainObject(provider.extra_headers)) {
        Object.keys(provider.extra_headers).forEach(function (name) { provider.extra_headers[name] = ""; });
      }
      self.providerMeta.set(provider, {
        origin: origin ? deepClone(origin) : null,
        apiKey: {
          statePath: apiState ? normalizeConfigPath(apiState.path) : "",
          originalPath: apiState && apiState.state === "configured" && apiState.source === "inline" ? normalizeConfigPath(apiState.path) : "",
          operation: apiState && apiState.state === "configured" && apiState.source === "inline" ? "keep" : "clear",
          value: ""
        },
        headerRows: headerRows,
        removedHeaderPaths: []
      });
    });

    var policies = this.state.draft && Array.isArray(this.state.draft.policy_profiles) ? this.state.draft.policy_profiles : [];
    policies.forEach(function (policy) {
      if (isPlainObject(policy)) self.policyMeta.set(policy, { step: 0 });
    });
  };

  ConfigEditor.prototype.findSecretState = function (provider, providerIndex, field) {
    var providerID = String(provider && provider.id || "");
    for (var i = 0; i < this.state.secretStates.length; i++) {
      var secretState = this.state.secretStates[i];
      if (!secretState || !secretState.path) continue;
      var segments = pointerSegments(secretState.path);
      if (segments.length < 3 || segments[0] !== "providers" || segments[2] !== field) continue;
      if (segments[1] === providerID || segments[1] === String(providerIndex)) return secretState;
    }
    return null;
  };

  ConfigEditor.prototype.parseExtraHeaderSecretState = function (secretState, provider, providerIndex) {
    if (!secretState || !secretState.path) return null;
    var segments = pointerSegments(secretState.path);
    var providerID = String(provider && provider.id || "");
    if (segments.length < 4 || segments[0] !== "providers" || segments[2] !== "extra_headers") return null;
    if (segments[1] !== providerID && segments[1] !== String(providerIndex)) return null;
    return { name: segments.slice(3).join("/"), state: secretState };
  };

  ConfigEditor.prototype.findExtraHeaderSecretState = function (provider, providerIndex, name) {
    for (var i = 0; i < this.state.secretStates.length; i++) {
      var parsed = this.parseExtraHeaderSecretState(this.state.secretStates[i], provider, providerIndex);
      if (parsed && parsed.name === name) return parsed.state;
    }
    return null;
  };

  ConfigEditor.prototype.reloadRequested = function () {
    if (this.state.dirty && !globalScope.confirm("Reloading will replace the current browser draft with the active configuration. Continue?")) return;
    this.loadConfig();
  };

  ConfigEditor.prototype.renderAll = function () {
    this.renderOverview();
    this.renderProviders();
    this.renderRoutes();
    this.renderPolicies();
    this.syncRawEditor();
    this.renderApplyStatus();
    this.updateActionState();
  };

  ConfigEditor.prototype.renderOverview = function () {
    var capabilityText = this.describeCapability();
    this.byId("capabilityValue").textContent = capabilityText.label;
    this.byId("capabilityValue").className = capabilityText.className;
    this.byId("sourceValue").textContent = this.describeSource();
    this.byId("revisionValue").textContent = this.state.revision || "—";
    var schema = this.state.draft && this.state.draft.schema_version;
    this.byId("schemaValue").textContent = schema ? "Version " + schema : "Implicit version 1";

    var policy = this.state.policy || {};
    var policyProfiles = Array.isArray(policy.profiles) ? policy.profiles : [];
    function profileModeSummary(field, fallbackKeys) {
      if (policyProfiles.length) {
        return policyProfiles.map(function (profile) {
          return String(profile.id || profile.public_id || "profile") + ": " + String(profile[field] || "off");
        }).join(", ");
      }
      return formatValue(readFirst(policy, fallbackKeys, "off"));
    }
    this.byId("policyConfiguredValue").textContent = profileModeSummary("configured_mode", ["configured_mode", "configured", "mode"]);
    this.byId("policyEffectiveValue").textContent = profileModeSummary("effective_mode", ["effective_mode", "effective"]);
    this.byId("policyCeilingValue").textContent = formatValue(readFirst(policy, ["process_ceiling", "ceiling", "maximum_mode"], "off"));

    var sourceDetails = this.byId("sourceDetails");
    sourceDetails.replaceChildren();
    if (isPlainObject(this.state.source)) {
      Object.keys(this.state.source).sort().forEach(function (key) {
        var term = document.createElement("dt");
        term.textContent = key.replace(/_/g, " ");
        var definition = document.createElement("dd");
        definition.textContent = formatValue(this.state.source[key]);
        sourceDetails.append(term, definition);
      }, this);
    } else {
      var term = document.createElement("dt");
      term.textContent = "source";
      var definition = document.createElement("dd");
      definition.textContent = formatValue(this.state.source);
      sourceDetails.append(term, definition);
    }

    var preserved = this.byId("preservedPathsList");
    preserved.replaceChildren();
    var paths = this.state.preservedPaths;
    if (!paths.length) {
      var none = document.createElement("li");
      none.textContent = "No server-preserved paths were reported.";
      preserved.appendChild(none);
    } else {
      paths.forEach(function (path) {
        var item = document.createElement("li");
        var code = document.createElement("code");
        code.textContent = path;
        item.appendChild(code);
        preserved.appendChild(item);
      });
    }
    this.byId("preservedSummary").textContent = "Server-preserved paths (" + paths.length + ")";

    var reason = capabilityReason(this.state.capability);
    var availability = this.byId("availabilityBanner");
    availability.hidden = this.state.configAvailable && this.state.canWrite;
    if (!availability.hidden) {
      this.byId("availabilityReason").textContent = reason || (this.state.configAvailable
        ? "The active configuration can be viewed, but this server did not grant write capability."
        : "This server mode does not expose the provider document to the dashboard.");
    }
    this.byId("staleBanner").hidden = !this.state.stale;
  };

  ConfigEditor.prototype.describeCapability = function () {
    if (this.state.canWrite) return { label: "Read and write", className: "status-good" };
    if (this.state.configAvailable) {
      if (deriveWriteCapability(this.state.capability) && !this.state.csrfToken) {
        return { label: "Read-only (CSRF token unavailable)", className: "status-warning" };
      }
      return { label: "Read-only", className: "status-warning" };
    }
    return { label: "Unavailable", className: "status-bad" };
  };

  ConfigEditor.prototype.describeSource = function () {
    if (typeof this.state.source === "string") return this.state.source;
    if (!isPlainObject(this.state.source)) return "—";
    return formatValue(readFirst(this.state.source, ["active", "kind", "id", "bootstrap_source", "bootstrap"], "—"));
  };

  ConfigEditor.prototype.announce = function (message) {
    this.byId("liveStatus").textContent = message;
  };

  ConfigEditor.prototype.markDirty = function (announcement) {
    this.state.dirty = true;
    this.state.rawDirty = false;
    this.clearErrors();
    this.scheduleRawSync();
    this.updateActionState();
    if (announcement) this.announce(announcement);
  };

  ConfigEditor.prototype.touchSchemaV2 = function () {
    this.state.forceV2 = true;
    if (this.state.draft && Number(this.state.draft.schema_version) < 2) {
      this.state.draft.schema_version = 2;
      this.byId("schemaValue").textContent = "Version 2";
      this.announce("Schema promoted to version 2. This editor never downgrades a version-2 configuration.");
    }
  };

  ConfigEditor.prototype.scheduleRawSync = function () {
    var self = this;
    if (this.rawSyncHandle !== null) return;
    var schedule = globalScope.requestAnimationFrame || function (callback) { return globalScope.setTimeout(callback, 0); };
    this.rawSyncHandle = schedule(function () {
      self.rawSyncHandle = null;
      self.syncRawEditor();
    });
  };

  ConfigEditor.prototype.syncRawEditor = function () {
    if (!this.state.draft || this.state.rawDirty) return;
    var serialized = serializeConfigDraft(
      this.state.draft,
      this.state.baseConfig,
      this.state.preservedPaths,
      this.state.baseSchemaVersion,
      this.state.forceV2
    );
    this.byId("rawConfig").value = JSON.stringify(serialized.config, null, 2) + "\n";
  };

  ConfigEditor.prototype.commitRawEditor = function (focusOnError) {
    var raw = this.byId("rawConfig").value;
    var parsed;
    try {
      parsed = JSON.parse(raw);
      if (!isPlainObject(parsed)) throw new Error("The top-level JSON value must be an object.");
    } catch (error) {
      this.showErrors([{ path: "/", message: "Structured JSON is invalid: " + error.message }]);
      this.announce("Structured JSON could not be applied to the forms.");
      return false;
    }

    var sanitized = stripSecretValues(parsed);
    var restored = restorePreservedPaths(sanitized.config, this.state.baseConfig || {}, this.state.preservedPaths);
    var versioned = enforceSchemaVersion(restored, this.state.baseSchemaVersion, this.state.forceV2);
    this.state.draft = versioned.config;
    if (!Array.isArray(this.state.draft.providers)) this.state.draft.providers = [];
    this.state.forceV2 = this.state.forceV2 || Number(this.state.draft.schema_version) >= 2 || hasSchemaV2Features(this.state.draft);
    this.state.rawDirty = false;
    this.state.dirty = true;
    this.initializeMetadata();
    this.renderProviders();
    this.renderRoutes();
    this.renderPolicies();
    this.renderOverview();
    this.syncRawEditor();
    this.clearErrors();
    this.updateActionState();
    var notes = [];
    if (sanitized.stripped) notes.push(sanitized.stripped + " secret value" + (sanitized.stripped === 1 ? " was" : "s were") + " removed; use explicit secret controls");
    if (versioned.promoted) notes.push("schema_version was kept at version 2");
    this.announce(notes.length ? "JSON updated. " + notes.join("; ") + "." : "Forms updated from structured JSON.");
    return true;
  };

  ConfigEditor.prototype.formatRawEditor = function () {
    var raw = this.byId("rawConfig").value;
    try {
      var parsed = JSON.parse(raw);
      this.byId("rawConfig").value = JSON.stringify(parsed, null, 2) + "\n";
      this.state.rawDirty = true;
      this.state.dirty = true;
      this.clearErrors();
      this.updateActionState();
      this.announce("Structured JSON formatted. Update the forms or validate to use it.");
    } catch (error) {
      this.showErrors([{ path: "/", message: "Structured JSON is invalid: " + error.message }]);
    }
  };

  ConfigEditor.prototype.copyRawEditor = async function () {
    if (!this.state.rawDirty) this.syncRawEditor();
    var text = this.byId("rawConfig").value;
    try {
      if (!navigator.clipboard || !navigator.clipboard.writeText) throw new Error("Clipboard access is unavailable.");
      await navigator.clipboard.writeText(text);
      this.announce("Configuration JSON copied to the clipboard.");
    } catch (error) {
      this.showErrors([{ path: "/", message: error.message || "Unable to copy the JSON." }]);
    }
  };

  ConfigEditor.prototype.importRawFile = function (event) {
    var self = this;
    var file = event.target.files && event.target.files[0];
    if (!file) return;
    var reader = new FileReader();
    reader.addEventListener("load", function () {
      self.byId("rawConfig").value = String(reader.result || "");
      self.state.rawDirty = true;
      self.state.dirty = true;
      self.selectTab("raw", false);
      self.updateActionState();
      self.announce("Imported " + file.name + ". Review it, then update the forms or validate.");
    });
    reader.addEventListener("error", function () {
      self.showErrors([{ path: "/", message: "Unable to read " + file.name + "." }]);
    });
    reader.readAsText(file);
    event.target.value = "";
  };

  ConfigEditor.prototype.nextFieldID = function (prefix) {
    this.fieldSequence++;
    return safeIdentifier(prefix, "field") + "-" + this.fieldSequence;
  };

  ConfigEditor.prototype.isPreservedPath = function (path) {
    var normalized = normalizeConfigPath(path);
    return this.state.preservedPaths.some(function (candidate) {
      var preserved = normalizeConfigPath(candidate);
      if (preserved.indexOf("*") !== -1) return false;
      return normalized === preserved || normalized.indexOf(preserved + "/") === 0;
    });
  };

  ConfigEditor.prototype.controlDisabled = function (path, explicitlyReadOnly) {
    return Boolean(explicitlyReadOnly) || !this.state.canWrite || this.state.pendingApply || this.isPreservedPath(path);
  };

  ConfigEditor.prototype.makeElement = function (tagName, className, text) {
    var element = this.document.createElement(tagName);
    if (className) element.className = className;
    if (text !== undefined && text !== null) element.textContent = text;
    return element;
  };

  ConfigEditor.prototype.makeField = function (labelText, control, options) {
    options = options || {};
    var wrapper = this.makeElement("div", "field" + (options.wide ? " field-wide" : ""));
    var label = this.makeElement("label", options.required ? "required-label" : "", labelText + (options.required ? " (required)" : ""));
    label.htmlFor = control.id;
    if (options.required && "required" in control) control.required = true;
    wrapper.append(label, control);
    var describedBy = [];
    if (options.hint) {
      var hint = this.makeElement("p", "field-hint", options.hint);
      hint.id = control.id + "-hint";
      wrapper.appendChild(hint);
      describedBy.push(hint.id);
    }
    if (options.path) control.dataset.configPath = normalizeConfigPath(options.path);
    if (describedBy.length) control.setAttribute("aria-describedby", describedBy.join(" "));
    return wrapper;
  };

  ConfigEditor.prototype.makeTextField = function (object, key, label, path, options) {
    var self = this;
    options = options || {};
    var input = this.makeElement("input");
    input.type = options.type || "text";
    input.id = this.nextFieldID(path || key);
    input.value = object && object[key] !== undefined && object[key] !== null ? String(object[key]) : "";
    if (options.placeholder) input.placeholder = options.placeholder;
    if (options.autocomplete) input.autocomplete = options.autocomplete;
    if (options.spellcheck === false) input.spellcheck = false;
    input.disabled = this.controlDisabled(path, options.readOnly);
    input.addEventListener("input", function () {
      var value = input.value;
      if (value === "" && !options.keepEmpty) delete object[key];
      else object[key] = value;
      if (options.schemaV2) self.touchSchemaV2();
      self.markDirty();
      if (options.onInput) options.onInput(value);
    });
    if (options.onChange) {
      input.addEventListener("change", function () { options.onChange(input.value); });
    }
    return this.makeField(label, input, {
      path: path,
      hint: options.hint,
      required: options.required,
      wide: options.wide
    });
  };

  ConfigEditor.prototype.makeNumberField = function (object, key, label, path, options) {
    var self = this;
    options = options || {};
    var input = this.makeElement("input");
    input.type = "number";
    input.id = this.nextFieldID(path || key);
    if (hasOwn(object, key) && object[key] !== null) input.value = String(object[key]);
    if (options.min !== undefined) input.min = String(options.min);
    if (options.max !== undefined) input.max = String(options.max);
    if (options.step !== undefined) input.step = String(options.step);
    input.disabled = this.controlDisabled(path, options.readOnly);
    input.addEventListener("input", function () {
      if (input.value === "") delete object[key];
      else object[key] = Number(input.value);
      if (options.schemaV2) self.touchSchemaV2();
      self.markDirty();
      if (options.onInput) options.onInput(input.value === "" ? undefined : Number(input.value));
    });
    return this.makeField(label, input, {
      path: path,
      hint: options.hint,
      required: options.required,
      wide: options.wide
    });
  };

  ConfigEditor.prototype.makeSelectField = function (object, key, label, path, choices, options) {
    var self = this;
    options = options || {};
    var select = this.makeElement("select");
    select.id = this.nextFieldID(path || key);
    var current = object && object[key] !== undefined && object[key] !== null ? String(object[key]) : "";
    var known = new Set();
    (choices || []).forEach(function (choice) {
      var value = Array.isArray(choice) ? String(choice[0]) : String(choice);
      var text = Array.isArray(choice) ? String(choice[1]) : String(choice);
      known.add(value);
      var option = self.makeElement("option", "", text);
      option.value = value;
      select.appendChild(option);
    });
    if (current && !known.has(current)) {
      var retained = this.makeElement("option", "", current + " (current value)");
      retained.value = current;
      select.appendChild(retained);
    }
    select.value = current;
    select.disabled = this.controlDisabled(path, options.readOnly);
    select.addEventListener("change", function () {
      if (select.value === "" && options.optional !== false) delete object[key];
      else object[key] = select.value;
      if (options.schemaV2) self.touchSchemaV2();
      self.markDirty();
      if (options.onChange) options.onChange(select.value);
    });
    return this.makeField(label, select, {
      path: path,
      hint: options.hint,
      required: options.required,
      wide: options.wide
    });
  };

  ConfigEditor.prototype.makeTriStateField = function (object, key, label, path, options) {
    options = options || {};
    var choices = [
      ["", options.unsetLabel || "Use provider default"],
      ["true", "Yes"],
      ["false", "No"]
    ];
    var proxy = { value: hasOwn(object, key) ? String(Boolean(object[key])) : "" };
    var field = this.makeSelectField(proxy, "value", label, path, choices, {
      optional: false,
      hint: options.hint,
      schemaV2: options.schemaV2,
      readOnly: options.readOnly,
      onChange: function (value) {
        if (value === "") delete object[key];
        else object[key] = value === "true";
        if (options.onChange) options.onChange(value);
      }
    });
    return field;
  };

  ConfigEditor.prototype.makeCheckboxField = function (object, key, label, path, options) {
    var self = this;
    options = options || {};
    var wrapper = this.makeElement("div", "checkbox-field" + (options.wide ? " field-wide" : ""));
    var input = this.makeElement("input");
    input.type = "checkbox";
    input.id = this.nextFieldID(path || key);
    input.checked = Boolean(object && object[key]);
    input.disabled = this.controlDisabled(path, options.readOnly);
    input.dataset.configPath = normalizeConfigPath(path);
    var textWrap = this.makeElement("div");
    var labelElement = this.makeElement("label", "", label);
    labelElement.htmlFor = input.id;
    textWrap.appendChild(labelElement);
    if (options.hint) textWrap.appendChild(this.makeElement("p", "field-hint", options.hint));
    wrapper.append(input, textWrap);
    input.addEventListener("change", function () {
      object[key] = input.checked;
      if (options.schemaV2) self.touchSchemaV2();
      self.markDirty();
      if (options.onChange) options.onChange(input.checked);
    });
    return wrapper;
  };

  ConfigEditor.prototype.makeListField = function (object, key, label, path, options) {
    var self = this;
    options = options || {};
    var textarea = this.makeElement("textarea");
    textarea.id = this.nextFieldID(path || key);
    textarea.rows = options.rows || 2;
    textarea.value = Array.isArray(object && object[key]) ? object[key].join("\n") : "";
    textarea.placeholder = options.placeholder || "One value per line";
    textarea.spellcheck = false;
    textarea.disabled = this.controlDisabled(path, options.readOnly);
    textarea.addEventListener("input", function () {
      object[key] = parseList(textarea.value);
      if (options.schemaV2) self.touchSchemaV2();
      self.markDirty();
    });
    return this.makeField(label, textarea, {
      path: path,
      hint: options.hint,
      required: options.required,
      wide: options.wide
    });
  };

  ConfigEditor.prototype.setNestedOptionalText = function (root, keys, value) {
    if (value !== "") {
      var current = root;
      for (var i = 0; i < keys.length - 1; i++) {
        if (!isPlainObject(current[keys[i]])) current[keys[i]] = {};
        current = current[keys[i]];
      }
      current[keys[keys.length - 1]] = value;
      return;
    }
    var chain = [root];
    var cursor = root;
    for (var j = 0; j < keys.length - 1; j++) {
      if (!isPlainObject(cursor[keys[j]])) return;
      cursor = cursor[keys[j]];
      chain.push(cursor);
    }
    delete cursor[keys[keys.length - 1]];
    for (var k = keys.length - 2; k >= 0; k--) {
      if (isPlainObject(chain[k][keys[k]]) && Object.keys(chain[k][keys[k]]).length === 0) delete chain[k][keys[k]];
      else break;
    }
  };

  ConfigEditor.prototype.makeNestedTextField = function (root, keys, label, path, options) {
    var self = this;
    options = options || {};
    var value = root;
    for (var i = 0; i < keys.length; i++) {
      value = isPlainObject(value) ? value[keys[i]] : undefined;
    }
    var proxy = { value: value === undefined || value === null ? "" : value };
    return this.makeTextField(proxy, "value", label, path, {
      hint: options.hint,
      readOnly: options.readOnly,
      onInput: function (nextValue) { self.setNestedOptionalText(root, keys, nextValue); }
    });
  };

  ConfigEditor.prototype.resolveProviderCapability = function (providerType) {
    var capabilities = this.state.providerCapabilities;
    if (!capabilities) return null;
    if (Array.isArray(capabilities)) {
      return capabilities.find(function (item) {
        return item && (item.type === providerType || item.kind === providerType || item.id === providerType);
      }) || null;
    }
    if (!isPlainObject(capabilities)) return null;
    if (capabilities[providerType]) return capabilities[providerType];
    var buckets = [capabilities.providers, capabilities.types, capabilities.kinds];
    for (var i = 0; i < buckets.length; i++) {
      if (isPlainObject(buckets[i]) && buckets[i][providerType]) return buckets[i][providerType];
      if (Array.isArray(buckets[i])) {
        var found = buckets[i].find(function (item) { return item && (item.type === providerType || item.kind === providerType); });
        if (found) return found;
      }
    }
    return null;
  };

  ConfigEditor.prototype.providerFieldDescriptor = function (providerType, field) {
    var capability = this.resolveProviderCapability(providerType);
    if (!capability) return null;
    var capabilityField = field === "headers" ? "copilot_headers" : field;
    var secretFields = capability.secret_fields || capability.secretFields;
    if (Array.isArray(secretFields) && secretFields.indexOf(capabilityField) !== -1) return { supported: true, secret: true };
    var fields = capability.fields || capability.provider_fields || capability.editable_fields || capability.supported_fields;
    if (Array.isArray(fields)) return fields.indexOf(capabilityField) !== -1 ? { supported: true } : { supported: false };
    if (isPlainObject(fields) && hasOwn(fields, capabilityField)) {
      if (typeof fields[capabilityField] === "boolean") return { supported: fields[capabilityField] };
      if (isPlainObject(fields[capabilityField])) return fields[capabilityField];
      return { supported: Boolean(fields[capabilityField]) };
    }
    return null;
  };

  ConfigEditor.prototype.providerFieldSupported = function (providerType, field) {
    var descriptor = this.providerFieldDescriptor(providerType, field);
    if (descriptor && descriptor.supported === false) return false;
    if (descriptor) return true;
    return COMMON_PROVIDER_FIELDS.indexOf(field) !== -1 || (PROVIDER_FIELDS_BY_TYPE[providerType] || []).indexOf(field) !== -1;
  };

  ConfigEditor.prototype.providerFieldReadOnly = function (providerType, field) {
    var descriptor = this.providerFieldDescriptor(providerType, field);
    return Boolean(descriptor && (descriptor.editable === false || descriptor.read_only === true));
  };

  ConfigEditor.prototype.providerSupportsDiscovery = function (providerType) {
    var capability = this.resolveProviderCapability(providerType);
    if (capability && typeof capability.supports_discovery === "boolean") return capability.supports_discovery;
    if (capability && typeof capability.supportsDiscovery === "boolean") return capability.supportsDiscovery;
    return this.providerFieldSupported(providerType, "model_discovery");
  };

  ConfigEditor.prototype.providerTypes = function () {
    var types = PROVIDER_TYPES.slice();
    var capabilities = this.state.providerCapabilities;
    var candidates = [];
    if (Array.isArray(capabilities)) candidates = capabilities;
    else if (isPlainObject(capabilities)) {
      if (Array.isArray(capabilities.providers)) candidates = capabilities.providers;
      else if (Array.isArray(capabilities.types)) candidates = capabilities.types;
      else {
        Object.keys(capabilities).forEach(function (key) {
          if (key.indexOf("-") !== -1) candidates.push({ type: key });
        });
      }
    }
    candidates.forEach(function (item) {
      var type = item && (item.type || item.kind || item.id);
      if (type && types.indexOf(type) === -1) types.push(type);
    });
    return types;
  };

  ConfigEditor.prototype.renderProviders = function (focusProviderIndex) {
    var self = this;
    var container = this.byId("providersList");
    container.replaceChildren();
    var providers = this.state.draft && Array.isArray(this.state.draft.providers) ? this.state.draft.providers : [];
    if (!providers.length) {
      container.appendChild(this.makeElement("div", "empty-state", "No explicit providers are configured. Applying an empty provider list returns the runtime to implicit GitHub Copilot behavior."));
      return;
    }
    providers.forEach(function (provider, index) {
      if (!isPlainObject(provider)) {
        var invalid = self.makeElement("div", "notice notice-error", "Provider " + (index + 1) + " is not a JSON object. Use Structured JSON to correct or remove it.");
        container.appendChild(invalid);
        return;
      }
      container.appendChild(self.renderProviderCard(provider, index));
    });
    if (focusProviderIndex !== undefined) {
      var target = this.byId("provider-card-" + focusProviderIndex);
      if (target) target.focus();
    }
  };

  ConfigEditor.prototype.renderProviderCard = function (provider, index) {
    var self = this;
    var path = "/providers/" + index;
    var providerType = String(provider.type || "openai-compatible");
    var card = this.makeElement("article", "editor-card");
    card.id = "provider-card-" + index;
    card.tabIndex = -1;

    var header = this.makeElement("div", "editor-card-header");
    var titleWrap = this.makeElement("div");
    var title = this.makeElement("h3", "", "Provider " + (index + 1) + " · " + (provider.id || "Untitled"));
    var subtitle = this.makeElement("p", "card-subtitle", PROVIDER_LABELS[providerType] || providerType || "Provider type not selected");
    titleWrap.append(title, subtitle);
    var remove = this.makeElement("button", "button button-danger-secondary button-small", "Remove provider");
    remove.type = "button";
    remove.disabled = this.controlDisabled(path);
    remove.setAttribute("aria-label", "Remove provider " + (provider.id || String(index + 1)));
    remove.addEventListener("click", function () {
      self.state.draft.providers.splice(index, 1);
      self.markDirty("Provider removed from the draft.");
      self.renderProviders(Math.max(0, index - 1));
      self.renderRoutes();
      self.renderPolicies();
    });
    header.append(titleWrap, remove);

    var body = this.makeElement("div", "card-body");
    var common = this.makeElement("section", "subsection");
    common.appendChild(this.makeElement("h4", "fieldset-title", "Identity and ownership"));
    var commonGrid = this.makeElement("div", "form-grid form-grid-3");
    commonGrid.append(
      this.makeTextField(provider, "id", "Provider ID", path + "/id", {
        required: true,
        keepEmpty: true,
        autocomplete: "off",
        spellcheck: false,
        hint: "Stable operational ID used by routes and secret paths.",
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
          self.renderRoutes();
          self.renderPolicies();
        }
      }),
      this.makeSelectField(provider, "type", "Provider type", path + "/type", this.providerTypes().map(function (type) {
        return [type, PROVIDER_LABELS[type] || type];
      }), {
        required: true,
        optional: false,
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
        }
      }),
      this.makeCheckboxField(provider, "default", "Default provider", path + "/default", {
        hint: "Used for legacy provider-owned routes when more than one provider is configured."
      })
    );
    commonGrid.append(
      this.makeListField(provider, "include_models", "Include model IDs", path + "/include_models", {
        hint: "Optional allowlist; one model ID per line."
      }),
      this.makeListField(provider, "exclude_models", "Exclude model IDs", path + "/exclude_models", {
        hint: "Optional denylist; one model ID per line."
      })
    );
    if (this.providerFieldSupported(providerType, "trust_domain")) {
      commonGrid.appendChild(this.makeTextField(provider, "trust_domain", "Trust domain", path + "/trust_domain", {
        schemaV2: true,
        hint: "Operator-defined data-governance label used by policy routing.",
        readOnly: this.providerFieldReadOnly(providerType, "trust_domain")
      }));
    }
    if (this.providerFieldSupported(providerType, "classifier_no_store_supported")) {
      commonGrid.appendChild(this.makeTriStateField(provider, "classifier_no_store_supported", "Classifier no-store supported", path + "/classifier_no_store_supported", {
        schemaV2: true,
        hint: "Declares that the classifier adapter can send this provider's supported non-storage option.",
        readOnly: this.providerFieldReadOnly(providerType, "classifier_no_store_supported")
      }));
    }
    common.appendChild(commonGrid);
    body.appendChild(common);

    this.renderProviderTypeSections(body, provider, index, providerType);
    this.renderRetainedProviderFields(body, provider, providerType);
    card.append(header, body);
    return card;
  };

  ConfigEditor.prototype.renderProviderTypeSections = function (body, provider, index, providerType) {
    var self = this;
    var path = "/providers/" + index;
    var connection = this.makeElement("section", "subsection");
    var connectionFields = this.makeElement("div", "form-grid form-grid-3");
    var connectionCount = 0;

    if (this.providerFieldSupported(providerType, "base_url")) {
      connectionFields.appendChild(this.makeTextField(provider, "base_url", "Base URL", path + "/base_url", {
        required: providerType !== "openai-codex",
        keepEmpty: providerType !== "openai-codex",
        type: "url",
        placeholder: providerType === "openai-codex" ? "Use the Codex default" : "https://provider.example/v1",
        hint: "Absolute upstream URL. URL userinfo is rejected.",
        readOnly: this.providerFieldReadOnly(providerType, "base_url"),
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
        }
      }));
      connectionCount++;
    }

    if (providerType === "azure-openai" && this.providerFieldSupported(providerType, "auth_mode")) {
      connectionFields.appendChild(this.makeSelectField(provider, "auth_mode", "Authentication mode", path + "/auth_mode", [
        ["", "API key (default)"],
        ["api_key", "API key"],
        ["azure_identity", "Azure identity"]
      ], {
        hint: "Azure identity uses the server's DefaultAzureCredential chain.",
        readOnly: this.providerFieldReadOnly(providerType, "auth_mode"),
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
        }
      }));
      connectionCount++;
      connectionFields.appendChild(this.makeTextField(provider, "api_version", "Azure API version", path + "/api_version", {
        hint: "Required unless the base URL path ends in /openai/v1.",
        readOnly: this.providerFieldReadOnly(providerType, "api_version"),
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
        }
      }));
      connectionCount++;
      if (String(provider.auth_mode || "api_key") === "azure_identity") {
        connectionFields.appendChild(this.makeTextField(provider, "token_scope", "Token scope", path + "/token_scope", {
          placeholder: "https://cognitiveservices.azure.com/.default",
          hint: "Leave empty to use the Azure OpenAI default scope.",
          readOnly: this.providerFieldReadOnly(providerType, "token_scope"),
          onChange: function () {
            self.reconcileProviderSecretCompatibility(provider);
            self.renderProviders(index);
          }
        }));
        connectionCount++;
      }
    }

    if ((providerType === "openai-compatible" || providerType === "anthropic-compatible") && this.providerFieldSupported(providerType, "auth_type")) {
      connectionFields.appendChild(this.makeSelectField(provider, "auth_type", "Authentication type", path + "/auth_type", [
        ["", "Bearer (default when a key is configured)"],
        ["bearer", "Bearer token"],
        ["api-key-header", "API key header"],
        ["none", "No authentication"]
      ], {
        hint: "Use no authentication only for a trusted local or private upstream.",
        readOnly: this.providerFieldReadOnly(providerType, "auth_type"),
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
        }
      }));
      connectionCount++;
      if (String(provider.auth_type || "") === "api-key-header") {
        connectionFields.appendChild(this.makeTextField(provider, "auth_header", "Authentication header", path + "/auth_header", {
          required: true,
          keepEmpty: true,
          placeholder: "x-api-key",
          readOnly: this.providerFieldReadOnly(providerType, "auth_header"),
          onChange: function () {
            self.reconcileProviderSecretCompatibility(provider);
            self.renderProviders(index);
          }
        }));
        connectionCount++;
      }
      if (String(provider.auth_type || "") === "bearer" || String(provider.auth_type || "") === "api-key-header") {
        connectionFields.appendChild(this.makeTextField(provider, "auth_prefix", "Authentication prefix", path + "/auth_prefix", {
          placeholder: provider.auth_type === "bearer" ? "Bearer" : "Optional prefix",
          hint: "Applied before the secret value when configured.",
          readOnly: this.providerFieldReadOnly(providerType, "auth_prefix"),
          onChange: function () {
            self.reconcileProviderSecretCompatibility(provider);
            self.renderProviders(index);
          }
        }));
        connectionCount++;
      }
    }

    if (this.providerFieldSupported(providerType, "model_discovery") && this.providerSupportsDiscovery(providerType)) {
      var discoveryChoices = [
        ["", "Static (default)"],
        ["static", "Static"],
        ["openai", "OpenAI /models"],
        ["openrouter-tools", "OpenRouter tools-capable models"]
      ];
      if (providerType !== "anthropic-compatible") discoveryChoices.push(["ollama", "Ollama /api/tags"]);
      connectionFields.appendChild(this.makeSelectField(provider, "model_discovery", "Model discovery", path + "/model_discovery", discoveryChoices, {
        hint: "Dynamic discovery runs privately while an apply candidate is built.",
        readOnly: this.providerFieldReadOnly(providerType, "model_discovery"),
        onChange: function () { self.renderProviders(index); }
      }));
      connectionCount++;
    }

    if (connectionCount) {
      connection.prepend(this.makeElement("h4", "fieldset-title", "Connection and provider behavior"));
      connection.appendChild(connectionFields);
      body.appendChild(connection);
    }

    if (this.shouldRenderCredentialFields(provider, providerType)) {
      body.appendChild(this.renderProviderCredentialSection(provider, index, providerType));
    }

    if (providerType === "openai-compatible" || providerType === "anthropic-compatible") {
      var pathsSection = this.renderProviderPathSection(provider, index, providerType);
      if (pathsSection) body.appendChild(pathsSection);
    }

    if (this.providerFieldSupported(providerType, "extra_headers")) {
      body.appendChild(this.renderExtraHeadersSection(provider, index));
    }

    if (providerType === "copilot" && this.providerFieldSupported(providerType, "headers")) {
      body.appendChild(this.renderCopilotHeadersSection(provider, index));
    }

    if (this.providerFieldSupported(providerType, "models")) {
      body.appendChild(this.renderProviderModelsSection(provider, index, providerType));
    }
  };

  ConfigEditor.prototype.shouldRenderCredentialFields = function (provider, providerType) {
    var meta = this.providerMeta.get(provider);
    if (meta && meta.apiKey && meta.apiKey.originalPath) return true;
    if (!this.providerFieldSupported(providerType, "api_key") && !this.providerFieldSupported(providerType, "api_key_env")) return false;
    if (providerType === "azure-openai") return String(provider.auth_mode || "api_key") !== "azure_identity";
    if (providerType === "openai-compatible" || providerType === "anthropic-compatible") return String(provider.auth_type || "") !== "none";
    return false;
  };

  ConfigEditor.prototype.renderProviderCredentialSection = function (provider, index, providerType) {
    var self = this;
    var section = this.makeElement("section", "subsection");
    var heading = this.makeElement("div", "subsection-heading");
    var headingCopy = this.makeElement("div");
    headingCopy.append(
      this.makeElement("h4", "", "Credential sources"),
      this.makeElement("p", "", "Use an environment-variable name or manage the inline API key with an explicit operation. Secret values never enter structured JSON.")
    );
    heading.appendChild(headingCopy);
    section.appendChild(heading);
    var grid = this.makeElement("div", "form-grid");
    if (this.providerFieldSupported(providerType, "api_key_env")) {
      grid.appendChild(this.makeTextField(provider, "api_key_env", "API key environment variable", "/providers/" + index + "/api_key_env", {
        placeholder: "PROVIDER_API_KEY",
        autocomplete: "off",
        spellcheck: false,
        hint: "The editor stores the variable name, not its value.",
        readOnly: this.providerFieldReadOnly(providerType, "api_key_env"),
        onChange: function () {
          self.reconcileProviderSecretCompatibility(provider);
          self.renderProviders(index);
        }
      }));
    }
    var credentialMeta = this.providerMeta.get(provider);
    var apiKeySupported = this.providerFieldSupported(providerType, "api_key");
    if (apiKeySupported || (credentialMeta && credentialMeta.apiKey.originalPath)) {
      var secret = this.renderAPIKeySecretControl(provider, index, apiKeySupported);
      secret.classList.add("field-wide");
      grid.appendChild(secret);
    }
    section.appendChild(grid);
    return section;
  };

  ConfigEditor.prototype.reconcileProviderSecretCompatibility = function (provider) {
    var meta = this.providerMeta.get(provider);
    if (!meta) return;
    var compatible = this.providerSecretCompatible(provider, meta);
    if (!compatible) {
      if (meta.apiKey.operation === "keep" || meta.apiKey.operation === "set") {
        meta.apiKey.operation = "";
        meta.apiKey.value = "";
        meta.apiKey.touched = false;
      }
      meta.headerRows.forEach(function (row) {
        if (row.operation === "keep" || row.operation === "set") {
          row.operation = "";
          row.value = "";
          row.touched = false;
        }
      });
    }
  };

  ConfigEditor.prototype.providerSecretCompatible = function (provider, meta) {
    return Boolean(meta && meta.origin && providerSecretFingerprint(provider) === providerSecretFingerprint(meta.origin));
  };

  ConfigEditor.prototype.renderAPIKeySecretControl = function (provider, index, allowSet) {
    var meta = this.providerMeta.get(provider);
    if (!meta) {
      meta = { origin: null, apiKey: { statePath: "", originalPath: "", operation: "clear", value: "" }, headerRows: [], removedHeaderPaths: [] };
      this.providerMeta.set(provider, meta);
    }
    var currentPath = providerSecretPath(provider.id);
    return this.renderSecretControl({
      label: "Inline API key",
      path: currentPath,
      fieldPath: "/providers/" + index + "/api_key",
      entry: meta.apiKey,
      canKeep: Boolean(meta.apiKey.originalPath) && this.providerSecretCompatible(provider, meta),
      secretState: meta.apiKey.statePath ? this.secretStateByPath.get(meta.apiKey.statePath) : null,
      allowSet: allowSet !== false,
      readOnly: this.providerFieldReadOnly(String(provider.type || ""), "api_key")
    });
  };

  ConfigEditor.prototype.renderSecretControl = function (options) {
    var self = this;
    var wrapper = this.makeElement("div", "secret-control");
    var operationField = this.makeElement("div", "field");
    var select = this.makeElement("select");
    select.id = this.nextFieldID(options.fieldPath + "-operation");
    select.dataset.configPath = normalizeConfigPath(options.fieldPath);
    var choices = [];
    if (!options.entry.operation || !options.canKeep) choices.push(["", "Choose a secret operation"]);
    choices.push(["keep", "Keep existing secret"]);
    if (options.allowSet !== false) choices.push(["set", "Set a new secret"]);
    choices.push(["clear", "Clear stored secret"]);
    choices.forEach(function (choice) {
      var option = self.makeElement("option", "", choice[1]);
      option.value = choice[0];
      if (choice[0] === "keep" && !options.canKeep) option.disabled = true;
      select.appendChild(option);
    });
    if (options.entry.operation === "keep" && !options.canKeep) options.entry.operation = "";
    if (options.entry.operation === "set" && options.allowSet === false) {
      options.entry.operation = "";
      options.entry.value = "";
    }
    select.value = options.entry.operation || "";
    select.disabled = this.controlDisabled(options.fieldPath, options.readOnly);
    var operationLabel = this.makeElement("label", "", options.label + " operation");
    operationLabel.htmlFor = select.id;
    operationField.append(operationLabel, select);
    var stateText = "No stored secret is reported at this path.";
    if (options.secretState) {
      stateText = "Existing secret: " + formatValue(options.secretState.state || "configured");
      if (options.secretState.source) stateText += " · source: " + options.secretState.source;
    } else if (!options.canKeep && options.entry.originalPath) {
      stateText = "The prior secret cannot be kept because the provider identity or destination changed.";
    }
    operationField.appendChild(this.makeElement("p", "secret-state", stateText));

    var valueField = this.makeElement("div", "field secret-value-field");
    var input = this.makeElement("input");
    input.type = "password";
    input.id = this.nextFieldID(options.fieldPath + "-value");
    input.value = options.entry.value || "";
    input.autocomplete = "new-password";
    input.spellcheck = false;
    input.placeholder = "Enter a new secret value";
    input.dataset.configPath = normalizeConfigPath(options.fieldPath);
    input.disabled = this.controlDisabled(options.fieldPath, options.readOnly);
    var valueLabel = this.makeElement("label", "", "New secret value");
    valueLabel.htmlFor = input.id;
    valueField.append(valueLabel, input, this.makeElement("p", "field-hint", "This value is held only in this page until validation or apply."));
    valueField.hidden = select.value !== "set";

    select.addEventListener("change", function () {
      options.entry.operation = select.value;
      options.entry.touched = true;
      if (select.value !== "set") options.entry.value = "";
      valueField.hidden = select.value !== "set";
      input.value = options.entry.value;
      self.markDirty();
      if (select.value === "set") input.focus();
    });
    input.addEventListener("input", function () {
      options.entry.value = input.value;
      options.entry.touched = true;
      self.markDirty();
    });
    wrapper.append(operationField, valueField);
    return wrapper;
  };

  ConfigEditor.prototype.renderProviderPathSection = function (provider, index, providerType) {
    var fields = [];
    var path = "/providers/" + index;
    if (providerType === "openai-compatible" && this.providerFieldSupported(providerType, "chat_completions_path")) {
      fields.push(this.makeTextField(provider, "chat_completions_path", "Chat completions path", path + "/chat_completions_path", {
        placeholder: "/chat/completions",
        readOnly: this.providerFieldReadOnly(providerType, "chat_completions_path")
      }));
    }
    if (providerType === "openai-compatible" && this.providerFieldSupported(providerType, "responses_path")) {
      fields.push(this.makeTextField(provider, "responses_path", "Responses path", path + "/responses_path", {
        placeholder: "/responses",
        readOnly: this.providerFieldReadOnly(providerType, "responses_path")
      }));
    }
    if (providerType === "anthropic-compatible" && this.providerFieldSupported(providerType, "messages_path")) {
      fields.push(this.makeTextField(provider, "messages_path", "Messages path", path + "/messages_path", {
        placeholder: "/v1/messages",
        readOnly: this.providerFieldReadOnly(providerType, "messages_path")
      }));
    }
    if (this.providerFieldSupported(providerType, "models_path")) {
      fields.push(this.makeTextField(provider, "models_path", "Models discovery path", path + "/models_path", {
        placeholder: provider.model_discovery === "ollama" ? "/api/tags" : "/models",
        readOnly: this.providerFieldReadOnly(providerType, "models_path")
      }));
    }
    if (!fields.length) return null;
    var section = this.makeElement("section", "subsection");
    section.appendChild(this.makeElement("h4", "fieldset-title", "Endpoint paths"));
    var grid = this.makeElement("div", "form-grid form-grid-3");
    fields.forEach(function (field) { grid.appendChild(field); });
    section.appendChild(grid);
    return section;
  };

  ConfigEditor.prototype.renderExtraHeadersSection = function (provider, index) {
    var self = this;
    var meta = this.providerMeta.get(provider);
    var section = this.makeElement("section", "subsection");
    var heading = this.makeElement("div", "subsection-heading");
    var copy = this.makeElement("div");
    copy.append(
      this.makeElement("h4", "", "Extra secret headers"),
      this.makeElement("p", "", "Header names are non-secret. Every value uses an explicit keep, set, or clear operation.")
    );
    var add = this.makeElement("button", "button button-secondary button-small", "Add header");
    add.type = "button";
    add.disabled = this.controlDisabled("/providers/" + index + "/extra_headers");
    add.addEventListener("click", function () {
      meta.headerRows.push({ name: "", originName: null, originalPath: "", operation: "set", value: "" });
      self.syncProviderExtraHeaders(provider, meta);
      self.markDirty("Extra header added. Enter a header name and secret value.");
      self.renderProviders(index);
    });
    heading.append(copy, add);
    section.appendChild(heading);

    var rows = this.makeElement("div");
    if (!meta.headerRows.length) {
      rows.appendChild(this.makeElement("div", "empty-state", "No extra headers are configured."));
    }
    meta.headerRows.forEach(function (row, rowIndex) {
      var rowElement = self.makeElement("div", "header-secret-row");
      var nameField = self.makeElement("div", "field");
      var nameInput = self.makeElement("input");
      nameInput.type = "text";
      nameInput.id = self.nextFieldID("extra-header-name");
      nameInput.value = row.name;
      nameInput.autocomplete = "off";
      nameInput.spellcheck = false;
      nameInput.disabled = self.controlDisabled("/providers/" + index + "/extra_headers");
      nameInput.dataset.configPath = "/providers/" + index + "/extra_headers/" + jsonPointerEscape(row.name || String(rowIndex));
      var nameLabel = self.makeElement("label", "required-label", "Header name (required)");
      nameLabel.htmlFor = nameInput.id;
      nameInput.required = true;
      nameField.append(nameLabel, nameInput);

      var originalName = row.originName;
      nameInput.addEventListener("input", function () {
        row.name = nameInput.value.trim();
        row.touched = true;
        self.syncProviderExtraHeaders(provider, meta);
        self.markDirty();
      });
      nameInput.addEventListener("change", function () {
        if ((row.operation === "keep" || row.operation === "set") && row.name !== originalName) {
          row.operation = "";
          row.value = "";
        }
        self.renderProviders(index);
      });

      var canKeep = Boolean(row.originalPath) && row.name === row.originName && self.providerSecretCompatible(provider, meta);
      var secret = self.renderSecretControl({
        label: "Header secret",
        path: extraHeaderSecretPath(provider.id, row.name),
        fieldPath: "/providers/" + index + "/extra_headers/" + jsonPointerEscape(row.name || String(rowIndex)),
        entry: row,
        canKeep: canKeep,
        secretState: row.originalPath ? self.secretStateByPath.get(row.originalPath) : null,
        readOnly: self.providerFieldReadOnly(String(provider.type || ""), "extra_headers")
      });
      var operationField = secret.children[0];
      var valueField = secret.children[1];

      var remove = self.makeElement("button", "button button-danger-secondary button-small", "Remove");
      remove.type = "button";
      remove.disabled = self.controlDisabled("/providers/" + index + "/extra_headers");
      remove.setAttribute("aria-label", "Remove extra header " + (row.name || String(rowIndex + 1)));
      remove.addEventListener("click", function () {
        if (row.originalPath) meta.removedHeaderPaths.push(row.originalPath);
        meta.headerRows.splice(rowIndex, 1);
        self.syncProviderExtraHeaders(provider, meta);
        self.markDirty("Extra header removed from the draft.");
        self.renderProviders(index);
      });
      rowElement.append(nameField, operationField, valueField, remove);
      rows.appendChild(rowElement);
    });
    section.appendChild(rows);
    return section;
  };

  ConfigEditor.prototype.syncProviderExtraHeaders = function (provider, meta) {
    var headers = {};
    meta.headerRows.forEach(function (row) {
      if (row.name) headers[row.name] = "";
    });
    provider.extra_headers = headers;
  };

  ConfigEditor.prototype.renderCopilotHeadersSection = function (provider, index) {
    var section = this.makeElement("section", "subsection");
    var heading = this.makeElement("div", "subsection-heading");
    var copy = this.makeElement("div");
    copy.append(
      this.makeElement("h4", "", "Copilot header profiles"),
      this.makeElement("p", "", "Endpoint-specific values inherit from the provider default profile and then Vekil defaults.")
    );
    heading.appendChild(copy);
    section.appendChild(heading);
    var self = this;
    COPILOT_HEADER_PROFILES.forEach(function (profile) {
      var details = self.makeElement("details", "card-details");
      if (profile[0] === "default") details.open = true;
      details.appendChild(self.makeElement("summary", "", profile[1]));
      var grid = self.makeElement("div", "form-grid form-grid-3");
      COPILOT_HEADER_FIELDS.forEach(function (field) {
        grid.appendChild(self.makeNestedTextField(
          provider,
          ["headers", profile[0], field[0]],
          field[1],
          "/providers/" + index + "/headers/" + profile[0] + "/" + field[0]
        ));
      });
      details.appendChild(grid);
      section.appendChild(details);
    });
    return section;
  };

  ConfigEditor.prototype.defaultModelEndpoints = function (providerType) {
    if (providerType === "anthropic-compatible") return ["/v1/messages"];
    if (providerType === "openai-codex") return ["/responses"];
    return ["/chat/completions"];
  };

  ConfigEditor.prototype.renderProviderModelsSection = function (provider, providerIndex, providerType) {
    var self = this;
    var section = this.makeElement("section", "subsection");
    var heading = this.makeElement("div", "subsection-heading");
    var copy = this.makeElement("div");
    copy.append(
      this.makeElement("h4", "", "Static models and metadata"),
      this.makeElement("p", "", "Endpoint allowlists describe verified native upstream support. Do not add Chat merely because Vekil can adapt a Responses-backed route.")
    );
    var add = this.makeElement("button", "button button-secondary button-small", "Add model");
    add.type = "button";
    add.disabled = this.controlDisabled("/providers/" + providerIndex + "/models");
    add.addEventListener("click", function () {
      if (!Array.isArray(provider.models)) provider.models = [];
      provider.models.push({ public_id: "", endpoints: self.defaultModelEndpoints(providerType) });
      self.markDirty("Static model added to the provider draft.");
      self.renderProviders(providerIndex);
    });
    heading.append(copy, add);
    section.appendChild(heading);

    var models = Array.isArray(provider.models) ? provider.models : [];
    if (!models.length) {
      section.appendChild(this.makeElement("div", "empty-state", provider.model_discovery && provider.model_discovery !== "static"
        ? "No static metadata overrides are configured. Models will be discovered while applying."
        : "No static models are configured."));
      return section;
    }

    models.forEach(function (model, modelIndex) {
      if (!isPlainObject(model)) {
        section.appendChild(self.makeElement("div", "notice notice-error", "Model " + (modelIndex + 1) + " is not a JSON object. Use Structured JSON to correct it."));
        return;
      }
      var modelPath = "/providers/" + providerIndex + "/models/" + modelIndex;
      var card = self.makeElement("div", "subcard");
      var modelHeader = self.makeElement("div", "subcard-header");
      modelHeader.appendChild(self.makeElement("h4", "", "Model " + (modelIndex + 1) + " · " + (model.public_id || "Untitled")));
      var remove = self.makeElement("button", "button button-danger-secondary button-small", "Remove model");
      remove.type = "button";
      remove.disabled = self.controlDisabled(modelPath);
      remove.setAttribute("aria-label", "Remove model " + (model.public_id || String(modelIndex + 1)));
      remove.addEventListener("click", function () {
        provider.models.splice(modelIndex, 1);
        self.markDirty("Static model removed from the provider draft.");
        self.renderProviders(providerIndex);
      });
      modelHeader.appendChild(remove);
      card.appendChild(modelHeader);
      var grid = self.makeElement("div", "form-grid form-grid-3");
      grid.append(
        self.makeTextField(model, "public_id", "Public model ID", modelPath + "/public_id", { required: true, keepEmpty: true, spellcheck: false }),
        self.makeTextField(model, "deployment", "Upstream deployment or model", modelPath + "/deployment", { hint: "Defaults to the public model ID when omitted." }),
        self.makeTextField(model, "name", "Display name", modelPath + "/name", {}),
        self.makeListField(model, "endpoints", "Native endpoints", modelPath + "/endpoints", {
          required: true,
          hint: "Examples: /chat/completions, /responses, /v1/messages."
        }),
        self.makeListField(model, "reasoning_effort", "Reasoning effort values", modelPath + "/reasoning_effort", {
          hint: "One supported value per line, such as low, medium, or high."
        }),
        self.makeNumberField(model, "context_window", "Context window", modelPath + "/context_window", { min: 1, step: 1 }),
        self.makeTextField(model, "model_picker_category", "Model picker category", modelPath + "/model_picker_category", {}),
        self.makeTriStateField(model, "model_picker_enabled", "Show in model picker", modelPath + "/model_picker_enabled", {}),
        self.makeTriStateField(model, "vision", "Vision supported", modelPath + "/vision", {}),
        self.makeTriStateField(model, "parallel_tool_calls", "Parallel tool calls", modelPath + "/parallel_tool_calls", {}),
        self.makeTriStateField(model, "drop_sampling_params", "Drop sampling parameters", modelPath + "/drop_sampling_params", {}),
        self.makeTriStateField(model, "use_max_completion_tokens", "Use max_completion_tokens", modelPath + "/use_max_completion_tokens", {})
      );
      card.appendChild(grid);
      section.appendChild(card);
    });
    return section;
  };

  ConfigEditor.prototype.renderRetainedProviderFields = function (body, provider, providerType) {
    var visible = new Set(COMMON_PROVIDER_FIELDS.concat(PROVIDER_FIELDS_BY_TYPE[providerType] || []));
    var retained = Object.keys(provider).filter(function (key) {
      return key !== "api_key" && !visible.has(key);
    });
    if (!retained.length) return;
    var details = this.makeElement("details", "card-details");
    details.appendChild(this.makeElement("summary", "", "Retained structured fields not shown for this provider type (" + retained.length + ")"));
    var copy = this.makeElement("p", "inline-note", "These fields remain in the draft unchanged. Use Structured JSON to inspect them; server validation remains authoritative after a provider type change.");
    var list = this.makeElement("ul", "retained-list");
    retained.sort().forEach(function (field) {
      var item = document.createElement("li");
      var code = document.createElement("code");
      code.textContent = field;
      item.appendChild(code);
      list.appendChild(item);
    });
    details.append(copy, list);
    body.appendChild(details);
  };

  ConfigEditor.prototype.addProvider = function () {
    if (!this.state.canWrite || this.state.pendingApply) return;
    if (!this.state.draft) this.state.draft = { providers: [] };
    if (!Array.isArray(this.state.draft.providers)) this.state.draft.providers = [];
    var existing = new Set(this.state.draft.providers.map(function (provider) { return provider && provider.id; }));
    var counter = this.state.draft.providers.length + 1;
    var id = "provider-" + counter;
    while (existing.has(id)) {
      counter++;
      id = "provider-" + counter;
    }
    var provider = {
      id: id,
      type: "openai-compatible",
      default: this.state.draft.providers.length === 0,
      base_url: "",
      model_discovery: "static",
      models: []
    };
    this.state.draft.providers.push(provider);
    this.providerMeta.set(provider, {
      origin: null,
      apiKey: { statePath: "", originalPath: "", operation: "clear", value: "" },
      headerRows: [],
      removedHeaderPaths: []
    });
    this.markDirty("Provider added to the draft.");
    this.renderProviders(this.state.draft.providers.length - 1);
    this.renderRoutes();
  };

  ConfigEditor.prototype.syncOptionalObject = function (root, key, object) {
    if (Object.keys(object).length) root[key] = object;
    else delete root[key];
  };

  ConfigEditor.prototype.renderRoutes = function (focusTarget) {
    var self = this;
    var container = this.byId("routesList");
    container.replaceChildren();
    var routes = this.state.draft && Array.isArray(this.state.draft.model_routes) ? this.state.draft.model_routes : [];
    if (!routes.length) {
      container.appendChild(this.makeElement("div", "empty-state", "No explicit model routes are configured. Add a route to define a schema-v2 public or internal contract."));
      return;
    }
    routes.forEach(function (route, index) {
      if (!isPlainObject(route)) {
        container.appendChild(self.makeElement("div", "notice notice-error", "Model route " + (index + 1) + " is not a JSON object. Use Structured JSON to correct it."));
        return;
      }
      container.appendChild(self.renderRouteCard(route, index));
    });
    if (focusTarget) {
      var target = this.byId("route-" + focusTarget.routeIndex + "-target-" + focusTarget.targetIndex);
      if (target) target.focus();
    }
  };

  ConfigEditor.prototype.renderRouteCard = function (route, routeIndex) {
    var self = this;
    var path = "/model_routes/" + routeIndex;
    var card = this.makeElement("article", "editor-card");
    card.id = "route-card-" + routeIndex;
    card.tabIndex = -1;

    var header = this.makeElement("div", "editor-card-header");
    var copy = this.makeElement("div");
    copy.append(
      this.makeElement("h3", "", "Route " + (routeIndex + 1) + " · " + (route.id || "Untitled")),
      this.makeElement("p", "card-subtitle", (route.exposure || "public") + " contract · " + ((Array.isArray(route.targets) ? route.targets.length : 0)) + " target(s)")
    );
    var badges = this.makeElement("div", "badge-row");
    badges.appendChild(this.makeElement("span", "badge badge-accent", "Schema v2 editor"));
    copy.appendChild(badges);
    var remove = this.makeElement("button", "button button-danger-secondary button-small", "Remove route");
    remove.type = "button";
    remove.disabled = this.controlDisabled(path);
    remove.setAttribute("aria-label", "Remove model route " + (route.id || String(routeIndex + 1)));
    remove.addEventListener("click", function () {
      self.state.draft.model_routes.splice(routeIndex, 1);
      self.touchSchemaV2();
      self.markDirty("Model route removed from the draft.");
      self.renderRoutes();
      self.renderPolicies();
    });
    header.append(copy, remove);

    var body = this.makeElement("div", "card-body");
    var contract = this.makeElement("section", "subsection");
    contract.appendChild(this.makeElement("h4", "fieldset-title", "Route contract"));
    var grid = this.makeElement("div", "form-grid form-grid-3");
    grid.append(
      this.makeTextField(route, "id", "Route ID", path + "/id", {
        required: true,
        keepEmpty: true,
        spellcheck: false,
        schemaV2: true,
        onChange: function () { self.renderRoutes(); self.renderPolicies(); }
      }),
      this.makeSelectField(route, "exposure", "Exposure", path + "/exposure", [
        ["", "Public (default)"],
        ["public", "Public"],
        ["internal", "Internal"]
      ], {
        schemaV2: true,
        hint: "Internal routes cannot be requested directly by clients.",
        onChange: function () { self.renderRoutes(); self.renderPolicies(); }
      }),
      this.makeTextField(route, "name", "Display name", path + "/name", { schemaV2: true })
    );
    if (String(route.exposure || "public") === "internal") {
      grid.appendChild(this.makeSelectField(route, "internal_purpose", "Internal purpose", path + "/internal_purpose", [
        ["", "No special purpose"],
        ["policy_classifier", "Policy classifier"]
      ], {
        schemaV2: true,
        hint: "Classifier routes must use policy_classifier.",
        onChange: function () { self.renderPolicies(); }
      }));
    } else {
      grid.append(
        this.makeTextField(route, "public_id", "Public model ID", path + "/public_id", {
          required: true,
          keepEmpty: true,
          spellcheck: false,
          schemaV2: true,
          hint: "Canonical model ID exposed by the proxy."
        }),
        this.makeTriStateField(route, "model_picker_enabled", "Show in model picker", path + "/model_picker_enabled", { schemaV2: true }),
        this.makeTextField(route, "model_picker_category", "Model picker category", path + "/model_picker_category", { schemaV2: true })
      );
    }
    grid.append(
      this.makeListField(route, "endpoints", "Native endpoints", path + "/endpoints", {
        required: true,
        schemaV2: true,
        hint: "Advertise only endpoints verified across every target."
      }),
      this.makeListField(route, "reasoning_effort", "Reasoning effort values", path + "/reasoning_effort", {
        schemaV2: true,
        hint: "One supported value per line."
      }),
      this.makeNumberField(route, "context_window", "Context window", path + "/context_window", { min: 1, step: 1, schemaV2: true }),
      this.makeTriStateField(route, "parallel_tool_calls", "Parallel tool calls", path + "/parallel_tool_calls", { schemaV2: true }),
      this.makeTriStateField(route, "vision", "Vision supported", path + "/vision", { schemaV2: true }),
      this.makeTriStateField(route, "drop_sampling_params", "Drop sampling parameters", path + "/drop_sampling_params", { schemaV2: true })
    );
    contract.appendChild(grid);
    body.appendChild(contract);

    body.appendChild(this.renderRouteTargetsSection(route, routeIndex));
    body.appendChild(this.renderRouteRoutingSection(route, routeIndex));
    card.append(header, body);
    return card;
  };

  ConfigEditor.prototype.renderRouteTargetsSection = function (route, routeIndex) {
    var self = this;
    var section = this.makeElement("section", "subsection");
    var heading = this.makeElement("div", "subsection-heading");
    var copy = this.makeElement("div");
    copy.append(
      this.makeElement("h4", "", "Ordered targets"),
      this.makeElement("p", "", "Target order is configuration-owned. Use the explicit Up and Down controls to change failover priority.")
    );
    var add = this.makeElement("button", "button button-secondary button-small", "Add target");
    add.type = "button";
    add.disabled = this.controlDisabled("/model_routes/" + routeIndex + "/targets");
    add.addEventListener("click", function () {
      if (!Array.isArray(route.targets)) route.targets = [];
      var provider = self.state.draft.providers && self.state.draft.providers[0];
      route.targets.push({
        id: "target-" + (route.targets.length + 1),
        provider: provider && provider.id || "",
        upstream_model: ""
      });
      self.touchSchemaV2();
      self.markDirty("Route target added.");
      self.renderRoutes({ routeIndex: routeIndex, targetIndex: route.targets.length - 1 });
    });
    heading.append(copy, add);
    section.appendChild(heading);

    var targets = Array.isArray(route.targets) ? route.targets : [];
    if (!targets.length) {
      section.appendChild(this.makeElement("div", "empty-state", "This route has no targets. Add at least one provider target before validation."));
      return section;
    }

    targets.forEach(function (target, targetIndex) {
      if (!isPlainObject(target)) {
        section.appendChild(self.makeElement("div", "notice notice-error", "Target " + (targetIndex + 1) + " is not a JSON object."));
        return;
      }
      var targetPath = "/model_routes/" + routeIndex + "/targets/" + targetIndex;
      var card = self.makeElement("div", "subcard");
      card.id = "route-" + routeIndex + "-target-" + targetIndex;
      card.tabIndex = -1;
      var targetHeader = self.makeElement("div", "subcard-header");
      var targetTitle = self.makeElement("div");
      targetTitle.append(
        self.makeElement("h4", "", "Priority " + (targetIndex + 1) + " · " + (target.id || "Untitled target")),
        self.makeElement("p", "card-subtitle", (target.provider || "No provider") + " → " + (target.upstream_model || "No upstream model"))
      );
      var controls = self.makeElement("div", "order-controls");
      var up = self.makeElement("button", "button button-secondary button-small", "Up");
      up.type = "button";
      up.disabled = targetIndex === 0 || self.controlDisabled(targetPath);
      up.setAttribute("aria-label", "Move target " + (target.id || String(targetIndex + 1)) + " up");
      up.addEventListener("click", function () {
        route.targets = moveArrayItem(route.targets, targetIndex, targetIndex - 1);
        self.touchSchemaV2();
        self.markDirty("Target moved up in failover priority.");
        self.renderRoutes({ routeIndex: routeIndex, targetIndex: targetIndex - 1 });
      });
      var down = self.makeElement("button", "button button-secondary button-small", "Down");
      down.type = "button";
      down.disabled = targetIndex === targets.length - 1 || self.controlDisabled(targetPath);
      down.setAttribute("aria-label", "Move target " + (target.id || String(targetIndex + 1)) + " down");
      down.addEventListener("click", function () {
        route.targets = moveArrayItem(route.targets, targetIndex, targetIndex + 1);
        self.touchSchemaV2();
        self.markDirty("Target moved down in failover priority.");
        self.renderRoutes({ routeIndex: routeIndex, targetIndex: targetIndex + 1 });
      });
      var remove = self.makeElement("button", "button button-danger-secondary button-small", "Remove");
      remove.type = "button";
      remove.disabled = self.controlDisabled(targetPath);
      remove.setAttribute("aria-label", "Remove target " + (target.id || String(targetIndex + 1)));
      remove.addEventListener("click", function () {
        route.targets.splice(targetIndex, 1);
        self.touchSchemaV2();
        self.markDirty("Route target removed.");
        self.renderRoutes();
      });
      controls.append(up, down, remove);
      targetHeader.append(targetTitle, controls);
      card.appendChild(targetHeader);

      var grid = self.makeElement("div", "form-grid form-grid-3");
      var providerChoices = [["", "Select a provider"]].concat((self.state.draft.providers || []).filter(isPlainObject).map(function (provider) {
        return [String(provider.id || ""), String(provider.id || "Untitled") + " · " + String(provider.type || "unknown")];
      }));
      grid.append(
        self.makeTextField(target, "id", "Target ID", targetPath + "/id", { required: true, keepEmpty: true, spellcheck: false, schemaV2: true }),
        self.makeSelectField(target, "provider", "Provider", targetPath + "/provider", providerChoices, { required: true, optional: false, schemaV2: true }),
        self.makeTextField(target, "upstream_model", "Upstream model or deployment", targetPath + "/upstream_model", { required: true, keepEmpty: true, schemaV2: true }),
        self.makeTriStateField(target, "use_max_completion_tokens", "Use max_completion_tokens", targetPath + "/use_max_completion_tokens", { schemaV2: true })
      );
      card.appendChild(grid);
      section.appendChild(card);
    });
    return section;
  };

  ConfigEditor.prototype.renderRouteRoutingSection = function (route, routeIndex) {
    var self = this;
    var section = this.makeElement("section", "subsection");
    section.appendChild(this.makeElement("h4", "fieldset-title", "Routing budgets"));
    var routing = isPlainObject(route.routing) ? route.routing : {};
    var path = "/model_routes/" + routeIndex + "/routing";
    var grid = this.makeElement("div", "form-grid form-grid-3");
    grid.appendChild(this.makeSelectField(routing, "mode", "Routing mode", path + "/mode", [
      ["", "Primary only (default)"],
      ["primary_only", "Primary only"],
      ["priority_failover", "Priority failover"]
    ], {
      schemaV2: true,
      hint: "Priority failover tries ordered semantically equivalent targets within the configured budgets.",
      onChange: function () { self.syncOptionalObject(route, "routing", routing); }
    }));
    grid.appendChild(this.makeNumberField(routing, "max_target_attempts", "Maximum target attempts", path + "/max_target_attempts", {
      min: 1,
      max: 32,
      step: 1,
      schemaV2: true,
      onInput: function () { self.syncOptionalObject(route, "routing", routing); }
    }));
    grid.appendChild(this.makeNumberField(routing, "max_upstream_sends", "Maximum upstream sends", path + "/max_upstream_sends", {
      min: 1,
      step: 1,
      schemaV2: true,
      onInput: function () { self.syncOptionalObject(route, "routing", routing); }
    }));
    section.appendChild(grid);
    return section;
  };

  ConfigEditor.prototype.addRoute = function () {
    if (!this.state.canWrite || this.state.pendingApply) return;
    if (!Array.isArray(this.state.draft.model_routes)) this.state.draft.model_routes = [];
    var number = this.state.draft.model_routes.length + 1;
    var firstProvider = this.state.draft.providers && this.state.draft.providers[0];
    this.state.draft.model_routes.push({
      id: "route-" + number,
      exposure: "public",
      public_id: "model-" + number,
      name: "Model " + number,
      endpoints: ["/chat/completions"],
      targets: [{ id: "primary", provider: firstProvider && firstProvider.id || "", upstream_model: "" }],
      routing: { mode: "primary_only" }
    });
    this.touchSchemaV2();
    this.markDirty("Schema-v2 model route added.");
    this.renderRoutes();
    this.renderPolicies();
    var card = this.byId("route-card-" + (this.state.draft.model_routes.length - 1));
    if (card) card.focus();
  };

  ConfigEditor.prototype.renderPolicies = function (focusPolicyIndex) {
    var self = this;
    var container = this.byId("policiesList");
    container.replaceChildren();
    var policies = this.state.draft && Array.isArray(this.state.draft.policy_profiles) ? this.state.draft.policy_profiles : [];
    if (!policies.length) {
      container.appendChild(this.makeElement("div", "empty-state", "No semantic policy profiles are configured. Add a profile to start the schema-v2 policy wizard."));
      return;
    }
    policies.forEach(function (policy, index) {
      if (!isPlainObject(policy)) {
        container.appendChild(self.makeElement("div", "notice notice-error", "Policy profile " + (index + 1) + " is not a JSON object. Use Structured JSON to correct it."));
        return;
      }
      container.appendChild(self.renderPolicyCard(policy, index));
    });
    if (focusPolicyIndex !== undefined) {
      var card = this.byId("policy-card-" + focusPolicyIndex);
      if (card) card.focus();
    }
  };

  ConfigEditor.prototype.renderPolicyCard = function (policy, policyIndex) {
    var self = this;
    var path = "/policy_profiles/" + policyIndex;
    var meta = this.policyMeta.get(policy);
    if (!meta) {
      meta = { step: 0 };
      this.policyMeta.set(policy, meta);
    }
    meta.step = Math.max(0, Math.min(POLICY_STEPS.length - 1, Number(meta.step) || 0));

    var card = this.makeElement("article", "editor-card");
    card.id = "policy-card-" + policyIndex;
    card.tabIndex = -1;
    var header = this.makeElement("div", "editor-card-header");
    var copy = this.makeElement("div");
    copy.append(
      this.makeElement("h3", "", "Policy " + (policyIndex + 1) + " · " + (policy.id || "Untitled")),
      this.makeElement("p", "card-subtitle", "Configured mode: " + (policy.mode || "off") + " · process ceiling: " + formatValue(readFirst(this.state.policy || {}, ["ceiling", "process_ceiling", "maximum_mode"], "off")))
    );
    var badges = this.makeElement("div", "badge-row");
    badges.appendChild(this.makeElement("span", "badge badge-accent", "Schema v2"));
    if (String(policy.mode || "off") !== "off") badges.appendChild(this.makeElement("span", "badge badge-warning", "Classifier preflight required on apply"));
    copy.appendChild(badges);
    var remove = this.makeElement("button", "button button-danger-secondary button-small", "Remove policy");
    remove.type = "button";
    remove.disabled = this.controlDisabled(path);
    remove.setAttribute("aria-label", "Remove policy profile " + (policy.id || String(policyIndex + 1)));
    remove.addEventListener("click", function () {
      self.state.draft.policy_profiles.splice(policyIndex, 1);
      self.touchSchemaV2();
      self.markDirty("Policy profile removed from the draft.");
      self.renderPolicies(Math.max(0, policyIndex - 1));
    });
    header.append(copy, remove);

    var body = this.makeElement("div", "card-body");
    var stepper = this.makeElement("ol", "wizard-stepper");
    POLICY_STEPS.forEach(function (step, index) {
      var item = self.makeElement("li", "", (index + 1) + ". " + step);
      if (index === meta.step) item.setAttribute("aria-current", "step");
      stepper.appendChild(item);
    });
    body.appendChild(stepper);

    var panel = this.makeElement("fieldset", "wizard-panel");
    panel.appendChild(this.makeElement("legend", "", "Step " + (meta.step + 1) + ": " + POLICY_STEPS[meta.step]));
    if (meta.step === 0) this.renderPolicyIdentityStep(panel, policy, policyIndex);
    else if (meta.step === 1) this.renderPolicyRoutesStep(panel, policy, policyIndex);
    else if (meta.step === 2) this.renderPolicyClassifierStep(panel, policy, policyIndex);
    else this.renderPolicyDataStep(panel, policy, policyIndex);
    body.appendChild(panel);

    var actions = this.makeElement("div", "wizard-actions");
    var back = this.makeElement("button", "button button-secondary", "Back");
    back.type = "button";
    back.disabled = meta.step === 0;
    back.addEventListener("click", function () {
      meta.step--;
      self.renderPolicies(policyIndex);
    });
    var progress = this.makeElement("span", "inline-note", "Step " + (meta.step + 1) + " of " + POLICY_STEPS.length);
    var next = this.makeElement("button", "button button-primary", meta.step === POLICY_STEPS.length - 1 ? "Review in Structured JSON" : "Next step");
    next.type = "button";
    next.addEventListener("click", function () {
      if (meta.step === POLICY_STEPS.length - 1) {
        self.selectTab("raw", true);
        self.byId("rawConfig").focus();
        return;
      }
      meta.step++;
      self.renderPolicies(policyIndex);
      var updated = self.byId("policy-card-" + policyIndex);
      if (updated) updated.focus();
    });
    actions.append(back, progress, next);
    body.appendChild(actions);
    card.append(header, body);
    return card;
  };

  ConfigEditor.prototype.renderPolicyIdentityStep = function (panel, policy, policyIndex) {
    var self = this;
    var path = "/policy_profiles/" + policyIndex;
    var grid = this.makeElement("div", "form-grid form-grid-3");
    grid.append(
      this.makeTextField(policy, "id", "Policy ID", path + "/id", { required: true, keepEmpty: true, spellcheck: false, schemaV2: true }),
      this.makeTextField(policy, "public_id", "Public model ID", path + "/public_id", { required: true, keepEmpty: true, spellcheck: false, schemaV2: true }),
      this.makeTextField(policy, "name", "Display name", path + "/name", { schemaV2: true }),
      this.makeSelectField(policy, "mode", "Configured mode", path + "/mode", [
        ["", "Off (default)"],
        ["off", "Off"],
        ["observe", "Observe"],
        ["enforce", "Enforce"]
      ], {
        schemaV2: true,
        hint: "The effective mode cannot exceed the process-wide ceiling.",
        onChange: function () { self.renderPolicies(policyIndex); }
      }),
      this.makeTriStateField(policy, "model_picker_enabled", "Show in model picker", path + "/model_picker_enabled", { schemaV2: true }),
      this.makeTextField(policy, "model_picker_category", "Model picker category", path + "/model_picker_category", { schemaV2: true })
    );
    panel.appendChild(grid);
  };

  ConfigEditor.prototype.policyRouteChoices = function (kind, current) {
    var routes = this.state.draft && Array.isArray(this.state.draft.model_routes) ? this.state.draft.model_routes : [];
    var baseRoutes = this.state.baseConfig && Array.isArray(this.state.baseConfig.model_routes) ? this.state.baseConfig.model_routes : [];
    var baseByID = new Map();
    baseRoutes.filter(isPlainObject).forEach(function (route) { baseByID.set(String(route.id || ""), route); });
    var metadata = this.state.policyEligibility || {};
    var metadataRows = kind === "classifier" ? metadata.classifier_routes : metadata.terminal_routes;
    var eligibleIDs = new Set((Array.isArray(metadataRows) ? metadataRows : []).map(function (entry) {
      return String(isPlainObject(entry) ? entry.id : entry || "");
    }).filter(Boolean));
    var hasServerEligibility = Array.isArray(metadataRows);
    var filtered = routes.filter(function (route) {
      if (!isPlainObject(route)) return false;
      var classifier = route.internal_purpose === "policy_classifier";
      var locallyEligible = kind === "classifier" ? classifier : !classifier;
      if (!locallyEligible) return false;
      if (!hasServerEligibility) return true;
      var routeID = String(route.id || "");
      if (eligibleIDs.has(routeID) || routeID === String(current || "")) return true;
      var base = baseByID.get(routeID);
      return !base || JSON.stringify(base) !== JSON.stringify(route);
    });
    if (!filtered.length && !hasServerEligibility) filtered = routes.filter(isPlainObject);
    var choices = [["", kind === "classifier" ? "Select an internal classifier route" : "Select a terminal route"]];
    filtered.forEach(function (route) {
      choices.push([String(route.id || ""), String(route.id || "Untitled") + " · " + String(route.exposure || "public")]);
    });
    if (current && !choices.some(function (choice) { return choice[0] === String(current); })) {
      choices.push([String(current), String(current) + " (current value)"]);
    }
    return choices;
  };

  ConfigEditor.prototype.renderPolicyRoutesStep = function (panel, policy, policyIndex) {
    var path = "/policy_profiles/" + policyIndex;
    var grid = this.makeElement("div", "form-grid form-grid-3");
    grid.append(
      this.makeSelectField(policy, "lightweight_route", "Lightweight terminal route", path + "/lightweight_route", this.policyRouteChoices("terminal", policy.lightweight_route), {
        required: true,
        optional: false,
        schemaV2: true
      }),
      this.makeSelectField(policy, "powerful_route", "Powerful terminal route", path + "/powerful_route", this.policyRouteChoices("terminal", policy.powerful_route), {
        required: true,
        optional: false,
        schemaV2: true
      }),
      this.makeSelectField(policy, "baseline_tier", "Baseline tier", path + "/baseline_tier", [
        ["", "Lightweight (default)"],
        ["lightweight", "Lightweight"],
        ["powerful", "Powerful"]
      ], { schemaV2: true }),
      this.makeSelectField(policy, "classifier_unavailable_tier", "Classifier unavailable tier", path + "/classifier_unavailable_tier", [
        ["", "Use baseline tier"],
        ["lightweight", "Lightweight"],
        ["powerful", "Powerful"]
      ], {
        schemaV2: true,
        hint: "Used when admission or classifier infrastructure is unavailable."
      }),
      this.makeSelectField(policy, "classifier_uncertain_tier", "Classifier uncertain tier", path + "/classifier_uncertain_tier", [
        ["", "Powerful (default)"],
        ["lightweight", "Lightweight"],
        ["powerful", "Powerful"]
      ], {
        schemaV2: true,
        hint: "Used after abstention or malformed/uncertain classifier output."
      })
    );
    panel.appendChild(grid);
  };

  ConfigEditor.prototype.renderPolicyClassifierStep = function (panel, policy, policyIndex) {
    var self = this;
    var classifier = isPlainObject(policy.classifier) ? policy.classifier : {};
    var path = "/policy_profiles/" + policyIndex + "/classifier";
    function sync() { self.syncOptionalObject(policy, "classifier", classifier); }
    var grid = this.makeElement("div", "form-grid form-grid-3");
    grid.append(
      this.makeSelectField(classifier, "route", "Classifier route", path + "/route", this.policyRouteChoices("classifier", classifier.route), {
        required: true,
        optional: false,
        schemaV2: true,
        hint: "Use an internal route with internal_purpose policy_classifier.",
        onChange: sync
      }),
      this.makeSelectField(classifier, "profile", "Classifier profile", path + "/profile", [
        ["", "coding_agent_v1 (default)"],
        ["coding_agent_v1", "coding_agent_v1"]
      ], { schemaV2: true, onChange: sync }),
      this.makeNumberField(classifier, "timeout_ms", "Timeout (milliseconds)", path + "/timeout_ms", {
        min: 100, max: 10000, step: 1, schemaV2: true, hint: "Allowed range: 100–10,000.", onInput: sync
      }),
      this.makeNumberField(classifier, "max_completion_tokens", "Maximum completion tokens", path + "/max_completion_tokens", {
        min: 32, max: 1024, step: 1, schemaV2: true, hint: "Allowed range: 32–1,024.", onInput: sync
      }),
      this.makeNumberField(classifier, "max_request_bytes", "Maximum request bytes", path + "/max_request_bytes", {
        min: 1024, max: 65536, step: 1, schemaV2: true, hint: "Caps serialized canonical classifier facts.", onInput: sync
      }),
      this.makeNumberField(classifier, "recent_turns", "Recent turns", path + "/recent_turns", {
        min: 0, max: 8, step: 1, schemaV2: true, hint: "Zero is meaningful and is preserved.", onInput: sync
      }),
      this.makeNumberField(classifier, "max_concurrency", "Maximum concurrency", path + "/max_concurrency", {
        min: 1, max: 32, step: 1, schemaV2: true, onInput: sync
      }),
      this.makeNumberField(classifier, "observe_sample_rate", "Observe sample rate", path + "/observe_sample_rate", {
        min: 0, max: 1, step: 0.01, schemaV2: true, hint: "A finite value from 0 through 1. Zero is preserved.", onInput: sync
      })
    );
    panel.appendChild(grid);
  };

  ConfigEditor.prototype.renderPolicyDataStep = function (panel, policy, policyIndex) {
    var self = this;
    var dataPolicy = isPlainObject(policy.data_policy) ? policy.data_policy : {};
    var path = "/policy_profiles/" + policyIndex + "/data_policy";
    function sync() { self.syncOptionalObject(policy, "data_policy", dataPolicy); }
    var notice = this.makeElement("div", "notice notice-warning");
    notice.append(
      this.makeElement("h3", "", "Operator review required"),
      this.makeElement("p", "", "Classifier facts include bounded user and system text. These acknowledgements record reviewed risk; they are not redaction or retention controls.")
    );
    panel.appendChild(notice);
    var grid = this.makeElement("div", "form-grid");
    grid.append(
      this.makeCheckboxField(dataPolicy, "content_forwarding_acknowledged", "I acknowledge bounded request content is forwarded to the classifier provider", path + "/content_forwarding_acknowledged", {
        schemaV2: true,
        wide: true,
        hint: "Required for every policy profile.",
        onChange: sync
      }),
      this.makeCheckboxField(dataPolicy, "allow_cross_trust_domain", "Allow classifier routing across trust domains", path + "/allow_cross_trust_domain", {
        schemaV2: true,
        hint: "Enable only after reviewing the data-governance boundary.",
        onChange: sync
      }),
      this.makeCheckboxField(dataPolicy, "allow_provider_retention", "Allow classifier provider retention", path + "/allow_provider_retention", {
        schemaV2: true,
        hint: "Required when the classifier provider cannot accept the configured non-storage behavior.",
        onChange: sync
      })
    );
    panel.appendChild(grid);
  };

  ConfigEditor.prototype.addPolicy = function () {
    if (!this.state.canWrite || this.state.pendingApply) return;
    if (!Array.isArray(this.state.draft.policy_profiles)) this.state.draft.policy_profiles = [];
    var number = this.state.draft.policy_profiles.length + 1;
    var terminalRoutes = this.policyRouteChoices("terminal").filter(function (choice) { return choice[0]; });
    var classifierRoutes = this.policyRouteChoices("classifier").filter(function (choice) { return choice[0]; });
    var policy = {
      id: "policy-" + number,
      public_id: "policy-model-" + number,
      name: "Policy model " + number,
      mode: "off",
      lightweight_route: terminalRoutes[0] ? terminalRoutes[0][0] : "",
      powerful_route: terminalRoutes[1] ? terminalRoutes[1][0] : (terminalRoutes[0] ? terminalRoutes[0][0] : ""),
      baseline_tier: "lightweight",
      classifier_unavailable_tier: "lightweight",
      classifier_uncertain_tier: "powerful",
      classifier: {
        route: classifierRoutes[0] ? classifierRoutes[0][0] : "",
        profile: "coding_agent_v1",
        timeout_ms: 3000,
        max_completion_tokens: 256,
        max_request_bytes: 16000,
        recent_turns: 4,
        max_concurrency: 4,
        observe_sample_rate: 1
      },
      data_policy: {
        content_forwarding_acknowledged: false,
        allow_cross_trust_domain: false,
        allow_provider_retention: false
      }
    };
    this.state.draft.policy_profiles.push(policy);
    this.policyMeta.set(policy, { step: 0 });
    this.touchSchemaV2();
    this.markDirty("Schema-v2 policy profile added. Complete all four wizard steps before applying.");
    this.renderPolicies(this.state.draft.policy_profiles.length - 1);
  };

  ConfigEditor.prototype.collectSecretEntries = function () {
    var self = this;
    var entries = [];
    var localErrors = [];
    var representedPaths = new Set();
    var providers = this.state.draft && Array.isArray(this.state.draft.providers) ? this.state.draft.providers : [];

    providers.forEach(function (provider, providerIndex) {
      if (!isPlainObject(provider)) return;
      var meta = self.providerMeta.get(provider);
      if (!meta) return;
      var providerID = String(provider.id || "");
      var compatible = self.providerSecretCompatible(provider, meta);
      var currentAPIPath = providerSecretPath(providerID);
      var api = meta.apiKey;
      if (api.originalPath) {
        representedPaths.add(normalizeConfigPath(api.originalPath));
        if (normalizeConfigPath(api.originalPath) !== normalizeConfigPath(currentAPIPath)) {
          if (!api.operation) {
            localErrors.push({ path: "/providers/" + providerIndex + "/api_key", message: "Choose set or clear for the API key after renaming this provider." });
          } else if (api.operation === "set") {
            entries.push({ path: currentAPIPath, operation: "set", value: api.value, canKeep: false, fieldPath: "/providers/" + providerIndex + "/api_key" });
          } else if (api.operation === "keep") {
            localErrors.push({ path: "/providers/" + providerIndex + "/api_key", message: "A renamed provider cannot keep the prior API key. Set a new value or clear it." });
          }
        } else {
          entries.push({
            path: currentAPIPath,
            operation: api.operation,
            value: api.value,
            canKeep: compatible,
            fieldPath: "/providers/" + providerIndex + "/api_key"
          });
        }
      } else if (api.operation === "set") {
        entries.push({ path: currentAPIPath, operation: "set", value: api.value, canKeep: false, fieldPath: "/providers/" + providerIndex + "/api_key" });
      } else if (api.touched && api.operation === "clear") {
        entries.push({ path: currentAPIPath, operation: "clear", canKeep: false, fieldPath: "/providers/" + providerIndex + "/api_key" });
      }

      var names = new Map();
      meta.headerRows.forEach(function (row, rowIndex) {
        var fieldPath = "/providers/" + providerIndex + "/extra_headers/" + jsonPointerEscape(row.name || String(rowIndex));
        if (!row.name) {
          if (row.operation === "set" || row.value) localErrors.push({ path: fieldPath, message: "Enter a header name for this secret operation." });
          return;
        }
        var normalizedName = row.name.toLowerCase();
        if (names.has(normalizedName)) {
          localErrors.push({ path: fieldPath, message: "Extra header names must be unique (case-insensitive)." });
          return;
        }
        names.set(normalizedName, rowIndex);
        var currentPath = extraHeaderSecretPath(providerID, row.name);
        if (row.originalPath) {
          representedPaths.add(normalizeConfigPath(row.originalPath));
          if (normalizeConfigPath(row.originalPath) !== normalizeConfigPath(currentPath)) {
            var providerIDUnchanged = meta.origin && String(meta.origin.id || "").trim() === providerID.trim();
            if (!row.operation) {
              localErrors.push({ path: fieldPath, message: "Choose set or clear after renaming this extra header or provider." });
            } else if (providerIDUnchanged) {
              entries.push({ path: row.originalPath, operation: "clear", canKeep: true, fieldPath: fieldPath });
            }
            if (row.operation === "set") {
              entries.push({ path: currentPath, operation: "set", value: row.value, canKeep: false, fieldPath: fieldPath });
            } else if (row.operation === "keep") {
              localErrors.push({ path: fieldPath, message: "A renamed extra header cannot keep the prior secret. Set a new value or clear it." });
            }
          } else {
            entries.push({
              path: currentPath,
              operation: row.operation,
              value: row.value,
              canKeep: compatible && row.name === row.originName,
              fieldPath: fieldPath
            });
          }
        } else if (row.operation === "set") {
          entries.push({ path: currentPath, operation: "set", value: row.value, canKeep: false, fieldPath: fieldPath });
        } else if (row.touched && row.operation === "clear") {
          entries.push({ path: currentPath, operation: "clear", canKeep: false, fieldPath: fieldPath });
        }
      });

      meta.removedHeaderPaths.forEach(function (removedPath) {
        representedPaths.add(normalizeConfigPath(removedPath));
        entries.push({ path: removedPath, operation: "clear", canKeep: true, fieldPath: "/providers/" + providerIndex + "/extra_headers" });
      });
    });

    this.state.secretStates.forEach(function (secretState) {
      if (!secretState || !secretState.path) return;
      if (secretState.state !== "configured" || secretState.source !== "inline") return;
      var path = normalizeConfigPath(secretState.path);
      if (representedPaths.has(path)) return;
      var segments = pointerSegments(path);
      if (segments[0] === "providers" && segments.length >= 3) {
        var provider = providers.find(function (candidate, index) {
          return isPlainObject(candidate) && (String(candidate.id || "") === segments[1] || String(index) === segments[1]);
        });
        if (!provider) return;
        var meta = self.providerMeta.get(provider);
        entries.push({
          path: path,
          operation: meta && self.providerSecretCompatible(provider, meta) ? "keep" : "clear",
          canKeep: Boolean(meta && self.providerSecretCompatible(provider, meta)),
          fieldPath: "/providers"
        });
      } else {
        entries.push({ path: path, operation: "keep", canKeep: true, fieldPath: "/" });
      }
    });

    var serialized = buildSecretOperations(entries);
    return { operations: serialized.operations, errors: localErrors.concat(serialized.errors) };
  };

  ConfigEditor.prototype.preparePayload = function () {
    if (this.state.rawDirty && !this.commitRawEditor(true)) return null;
    if (!this.state.draft) {
      this.showErrors([{ path: "/", message: "No provider configuration is available to serialize." }]);
      return null;
    }
    var serialized = serializeConfigDraft(
      this.state.draft,
      this.state.baseConfig,
      this.state.preservedPaths,
      this.state.baseSchemaVersion,
      this.state.forceV2
    );
    if (serialized.promoted) {
      this.state.draft.schema_version = 2;
      this.state.forceV2 = true;
      this.renderOverview();
      this.syncRawEditor();
    }
    var secrets = this.collectSecretEntries();
    if (secrets.errors.length) {
      this.showErrors(secrets.errors);
      this.announce("Resolve the secret-operation errors before continuing.");
      return null;
    }
    return {
      base_revision: this.state.revision,
      config: serialized.config,
      secret_operations: secrets.operations
    };
  };

  ConfigEditor.prototype.mutationHeaders = function () {
    var headers = {
      "Accept": "application/json",
      "Content-Type": "application/json",
      "X-Vekil-CSRF": this.state.csrfToken
    };
    if (this.state.etag || this.state.revision) headers["If-Match"] = this.state.etag || this.state.revision;
    return headers;
  };

  ConfigEditor.prototype.validateDraft = async function () {
    if (!this.state.canValidate || this.state.pendingApply) return;
    this.clearErrors();
    var payload = this.preparePayload();
    if (!payload) return;
    this.announce("Validating the staged configuration…");
    this.byId("validateButton").disabled = true;
    try {
      var response = await fetch(API_ROOT + "/validate", {
        method: "POST",
        headers: this.mutationHeaders(),
        body: JSON.stringify(payload),
        cache: "no-store",
        credentials: "same-origin"
      });
      var body = await this.readJSONResponse(response);
      if (response.status === 412) {
        this.handleStale(body);
        return;
      }
      var responseErrors = this.extractErrors(body, response.status);
      if (!response.ok || (body && body.valid === false) || responseErrors.length) {
        this.showErrors(responseErrors.length ? responseErrors : [{ path: "/", message: "Validation failed." }]);
        this.announce("Validation found problems in the draft.");
        return;
      }
      this.announce("Validation succeeded. The active runtime was not changed.");
      this.byId("actionHint").textContent = "Validation succeeded for revision " + (this.state.revision || "the active revision") + ".";
    } catch (error) {
      this.showErrors([{ path: "/", message: error.message || "Validation request failed." }]);
      this.announce("Validation request failed.");
    } finally {
      this.updateActionState();
    }
  };

  ConfigEditor.prototype.applyDraft = async function () {
    if (!this.state.canWrite || this.state.pendingApply || this.state.stale) return;
    this.clearErrors();
    var payload = this.preparePayload();
    if (!payload) return;
    this.announce("Submitting a candidate runtime generation…");
    try {
      var response = await fetch(API_ROOT + "/applies", {
        method: "POST",
        headers: this.mutationHeaders(),
        body: JSON.stringify(payload),
        cache: "no-store",
        credentials: "same-origin"
      });
      var body = await this.readJSONResponse(response);
      if (response.status === 412) {
        this.handleStale(body);
        return;
      }
      if (!response.ok) {
        var errors = this.extractErrors(body, response.status);
        this.showErrors(errors.length ? errors : [{ path: "/", message: "Apply request failed with HTTP " + response.status + "." }]);
        this.announce(response.status === 409 ? "Another apply is already in progress." : "The candidate was rejected before background apply began.");
        return;
      }
      var location = response.headers.get("Location") || readFirst(body || {}, ["location", "status_url"], "");
      this.beginApplyPolling(body || {}, location);
    } catch (error) {
      this.showErrors([{ path: "/", message: error.message || "Apply request failed." }]);
      this.announce("Apply request failed.");
    }
  };

  ConfigEditor.prototype.openResetDialog = function () {
    if (!this.state.canWrite || this.state.pendingApply) return;
    var dialog = this.byId("resetDialog");
    dialog.returnValue = "";
    if (typeof dialog.showModal === "function") dialog.showModal();
    else if (globalScope.confirm("Reset the managed override and restore the bootstrap configuration?")) this.resetManaged();
  };

  ConfigEditor.prototype.resetManaged = async function () {
    if (!this.state.canWrite || this.state.pendingApply) return;
    this.clearErrors();
    this.announce("Submitting managed-override reset…");
    try {
      var response = await fetch(API_ROOT + "/managed", {
        method: "DELETE",
        headers: this.mutationHeaders(),
        cache: "no-store",
        credentials: "same-origin"
      });
      var body = await this.readJSONResponse(response);
      if (response.status === 412) {
        this.handleStale(body);
        return;
      }
      if (!response.ok) {
        var errors = this.extractErrors(body, response.status);
        this.showErrors(errors.length ? errors : [{ path: "/", message: "Reset failed with HTTP " + response.status + "." }]);
        this.announce("Managed-override reset was rejected.");
        return;
      }
      if (response.status === 202 || (body && (body.id || body.apply_id))) {
        this.beginApplyPolling(body || {}, response.headers.get("Location") || readFirst(body || {}, ["location", "status_url"], ""));
      } else {
        this.announce("Managed override reset succeeded. Reloading the active configuration…");
        await this.loadConfig();
      }
    } catch (error) {
      this.showErrors([{ path: "/", message: error.message || "Managed-override reset failed." }]);
      this.announce("Managed-override reset failed.");
    }
  };

  ConfigEditor.prototype.normalizeApplyStatus = function (payload, location) {
    var root = isPlainObject(payload && payload.apply) ? payload.apply
      : (isPlainObject(payload && payload.job) ? payload.job : (isPlainObject(payload) ? payload : {}));
    var statusValue = root.status;
    if (isPlainObject(statusValue)) statusValue = statusValue.state || statusValue.status;
    var message = String(readFirst(root, ["message", "detail", "reason", "error_message"], "Candidate accepted."));
    if (root.warning) message += " Warning: " + String(root.warning);
    return {
      id: String(readFirst(root, ["id", "apply_id"], readFirst(payload || {}, ["id", "apply_id"], ""))),
      status: String(readFirst(root, ["state", "phase"], statusValue || "accepted")),
      message: message,
      location: location || String(readFirst(root, ["location", "status_url"], "")),
      errors: this.extractErrors(root, 0)
    };
  };

  ConfigEditor.prototype.beginApplyPolling = function (payload, location) {
    var apply = this.normalizeApplyStatus(payload, location);
    if (!apply.location && apply.id) apply.location = API_ROOT + "/applies/" + encodeURIComponent(apply.id);
    this.state.activeApply = apply;
    this.state.pendingApply = !isTerminalApplyStatus(apply.status);
    this.state.pollToken++;
    var token = this.state.pollToken;
    this.renderAll();
    this.announce("Candidate apply accepted: " + apply.status + ". The current runtime remains active until publication succeeds.");
    if (isTerminalApplyStatus(apply.status)) {
      this.finishApply(apply);
      return;
    }
    if (!apply.location) {
      this.state.pendingApply = false;
      this.showErrors([{ path: "/", message: "The apply response did not include an apply ID or status location." }]);
      this.renderAll();
      return;
    }
    this.pollApply(apply.location, token, APPLY_POLL_INITIAL_MS, 0);
  };

  ConfigEditor.prototype.pollApply = async function (location, token, delay, failures) {
    if (token !== this.state.pollToken) return;
    await new Promise(function (resolve) { globalScope.setTimeout(resolve, delay); });
    if (token !== this.state.pollToken) return;
    try {
      var response = await fetch(location, {
        method: "GET",
        headers: { "Accept": "application/json" },
        cache: "no-store",
        credentials: "same-origin"
      });
      var payload = await this.readJSONResponse(response);
      if (!response.ok) {
        var errors = this.extractErrors(payload, response.status);
        if (response.status === 404 || failures >= 4) {
          this.state.pendingApply = false;
          this.showErrors(errors.length ? errors : [{ path: "/", message: "Apply status is no longer available." }]);
          this.renderAll();
          this.announce("Apply-status polling stopped.");
          return;
        }
        throw new Error(errors[0] ? errors[0].message : "Apply status request failed.");
      }
      var apply = this.normalizeApplyStatus(payload || {}, location);
      if (!apply.id && this.state.activeApply) apply.id = this.state.activeApply.id;
      this.state.activeApply = apply;
      this.renderApplyStatus();
      this.announce("Candidate apply state: " + apply.status + ".");
      if (isTerminalApplyStatus(apply.status)) {
        this.finishApply(apply);
        return;
      }
      var retryAfter = Number(response.headers.get("Retry-After"));
      var nextDelay = Number.isFinite(retryAfter) && retryAfter > 0
        ? Math.min(APPLY_POLL_MAX_MS, retryAfter * 1000)
        : Math.min(APPLY_POLL_MAX_MS, Math.round(delay * 1.35));
      this.pollApply(location, token, nextDelay, 0);
    } catch (error) {
      if (token !== this.state.pollToken) return;
      this.announce("Apply-status polling was interrupted; retrying. " + (error.message || ""));
      this.pollApply(location, token, Math.min(APPLY_POLL_MAX_MS, Math.round(delay * 1.6)), failures + 1);
    }
  };

  ConfigEditor.prototype.finishApply = async function (apply) {
    this.state.pendingApply = false;
    this.state.activeApply = apply;
    if (isSuccessfulApplyStatus(apply.status)) {
      this.state.dirty = false;
      this.renderApplyStatus();
      this.updateActionState();
      this.announce("Configuration applied successfully. Reloading the published generation…");
      await this.loadConfig();
      return;
    }
    if (String(apply.status).toLowerCase() === "failed_revision") this.state.stale = true;
    this.renderAll();
    var errors = apply.errors && apply.errors.length ? apply.errors : [{ path: "/", message: apply.message || "The candidate apply failed before publication." }];
    this.showErrors(errors);
    this.announce("Candidate apply ended in " + apply.status + ". The prior runtime remains active.");
  };

  ConfigEditor.prototype.renderApplyStatus = function () {
    var card = this.byId("applyStatusCard");
    var apply = this.state.activeApply;
    card.hidden = !apply;
    if (!apply) return;
    this.byId("applyIdValue").textContent = apply.id || "—";
    var stateValue = this.byId("applyStateValue");
    stateValue.textContent = apply.status || "—";
    stateValue.className = isSuccessfulApplyStatus(apply.status)
      ? "status-good"
      : (isTerminalApplyStatus(apply.status) ? "status-bad" : "status-warning");
    this.byId("applyMessageValue").textContent = apply.message || "—";
  };

  ConfigEditor.prototype.handleStale = function (payload) {
    this.state.stale = true;
    this.byId("staleBanner").hidden = false;
    var errors = this.extractErrors(payload, 412);
    this.showErrors(errors.length ? errors : [{ path: "/", message: "The base revision is stale. Reload the active configuration before applying." }]);
    this.updateActionState();
    this.announce("The active revision changed. This draft was not applied.");
  };

  ConfigEditor.prototype.readJSONResponse = async function (response) {
    var text = await response.text();
    if (!text) return null;
    try {
      return JSON.parse(text);
    } catch (_error) {
      return { error: { message: "The server returned a non-JSON response." } };
    }
  };

  ConfigEditor.prototype.extractErrors = function (payload, status) {
    var errors = [];
    function add(value, fallbackPath) {
      if (!value) return;
      if (typeof value === "string") {
        errors.push({ path: fallbackPath || "/", message: value });
        return;
      }
      if (Array.isArray(value)) {
        value.forEach(function (item) { add(item, fallbackPath); });
        return;
      }
      if (!isPlainObject(value)) return;
      if (Array.isArray(value.errors)) add(value.errors, fallbackPath);
      var message = value.message || value.detail || value.reason || value.title;
      if (message) {
        errors.push({
          path: normalizeConfigPath(value.path || value.pointer || value.field || fallbackPath || "/"),
          message: String(message),
          code: value.code ? String(value.code) : ""
        });
      }
    }
    if (payload) {
      if (Array.isArray(payload.errors)) add(payload.errors, "/");
      if (payload.error) add(payload.error, "/");
      if (!errors.length && (payload.message || payload.detail)) add(payload, "/");
    }
    if (!errors.length && status >= 400) errors.push({ path: "/", message: "Request failed with HTTP " + status + "." });
    var seen = new Set();
    return errors.filter(function (error) {
      var key = normalizeConfigPath(error.path) + "\n" + error.message;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  };

  ConfigEditor.prototype.clearErrors = function () {
    var summary = this.byId("errorSummary");
    summary.hidden = true;
    this.byId("errorSummaryList").replaceChildren();
    Array.from(this.document.querySelectorAll('[aria-invalid="true"]')).forEach(function (control) {
      control.removeAttribute("aria-invalid");
      var described = String(control.getAttribute("aria-describedby") || "").split(/\s+/).filter(function (id) {
        return id && id.indexOf("editor-field-error-") !== 0;
      });
      if (described.length) control.setAttribute("aria-describedby", described.join(" "));
      else control.removeAttribute("aria-describedby");
    });
    Array.from(this.document.querySelectorAll('[data-editor-error="true"]')).forEach(function (node) { node.remove(); });
  };

  ConfigEditor.prototype.findControlForPath = function (path) {
    var normalized = normalizeConfigPath(path);
    var controls = Array.from(this.document.querySelectorAll("[data-config-path]"));
    function exact(candidate) {
      return controls.find(function (control) { return normalizeConfigPath(control.dataset.configPath) === candidate; });
    }
    var found = exact(normalized);
    if (found) return found;

    var segments = pointerSegments(normalized);
    if (segments[0] === "config") {
      segments.shift();
      normalized = segments.length ? "/" + segments.map(jsonPointerEscape).join("/") : "/";
      found = exact(normalized);
      if (found) return found;
    }
    if (segments[0] === "providers" && segments[1] && !/^\d+$/.test(segments[1])) {
      var providers = this.state.draft && Array.isArray(this.state.draft.providers) ? this.state.draft.providers : [];
      var index = providers.findIndex(function (provider) { return isPlainObject(provider) && String(provider.id || "") === segments[1]; });
      if (index >= 0) {
        segments[1] = String(index);
        found = exact("/" + segments.map(jsonPointerEscape).join("/"));
        if (found) return found;
      }
    }

    while (segments.length > 0) {
      segments.pop();
      found = exact(segments.length ? "/" + segments.map(jsonPointerEscape).join("/") : "/");
      if (found) return found;
    }
    return this.byId("rawConfig");
  };

  ConfigEditor.prototype.revealErrorTarget = function (path) {
    var segments = pointerSegments(path);
    if (segments[0] === "config") segments.shift();
    if (!segments.length) return;
    if (segments[0] === "providers") {
      this.selectTab("providers", false);
      return;
    }
    if (segments[0] === "model_routes") {
      this.selectTab("routes", false);
      return;
    }
    if (segments[0] === "policy_profiles") {
      var index = Number(segments[1]);
      var policies = this.state.draft && Array.isArray(this.state.draft.policy_profiles) ? this.state.draft.policy_profiles : [];
      var policy = Number.isInteger(index) ? policies[index] : null;
      if (isPlainObject(policy)) {
        var meta = this.policyMeta.get(policy) || { step: 0 };
        var field = segments[2] || "";
        if (field === "data_policy") meta.step = 3;
        else if (field === "classifier") meta.step = 2;
        else if (["lightweight_route", "powerful_route", "baseline_tier", "classifier_unavailable_tier", "classifier_uncertain_tier"].indexOf(field) !== -1) meta.step = 1;
        else meta.step = 0;
        this.policyMeta.set(policy, meta);
        this.renderPolicies();
      }
      this.selectTab("policies", false);
      return;
    }
    this.selectTab("raw", false);
  };

  ConfigEditor.prototype.showErrors = function (errors) {
    var self = this;
    var visibleErrors = errors && errors.length ? errors : [{ path: "/", message: "An unknown configuration error occurred." }];
    this.revealErrorTarget(visibleErrors[0].path || "/");
    this.clearErrors();
    var list = this.byId("errorSummaryList");
    visibleErrors.forEach(function (error, index) {
      var path = normalizeConfigPath(error.path || "/");
      var control = self.findControlForPath(path);
      var item = self.makeElement("li");
      var link = self.makeElement("a", "", (error.code ? error.code + ": " : "") + error.message + (path !== "/" ? " — " + path : ""));
      if (control) {
        if (!control.id) control.id = self.nextFieldID("error-target");
        link.href = "#" + control.id;
        link.addEventListener("click", function (event) {
          event.preventDefault();
          control.focus();
          control.scrollIntoView({ block: "center" });
        });
        control.setAttribute("aria-invalid", "true");
        var errorID = "editor-field-error-" + index;
        var inline = self.makeElement("p", "field-error", error.message);
        inline.id = errorID;
        inline.dataset.editorError = "true";
        var described = String(control.getAttribute("aria-describedby") || "").split(/\s+/).filter(Boolean);
        described.push(errorID);
        control.setAttribute("aria-describedby", described.join(" "));
        var field = control.closest(".field, .checkbox-field");
        if (field) field.appendChild(inline);
      }
      item.appendChild(link);
      list.appendChild(item);
    });
    var summary = this.byId("errorSummary");
    summary.hidden = false;
    summary.focus();
  };

  ConfigEditor.prototype.updateActionState = function () {
    var editable = this.state.canWrite && !this.state.pendingApply;
    this.byId("addProviderButton").disabled = !editable;
    this.byId("addRouteButton").disabled = !editable;
    this.byId("addPolicyButton").disabled = !editable;
    this.byId("validateButton").disabled = !this.state.canValidate || this.state.pendingApply;
    this.byId("applyButton").disabled = !this.state.canWrite || this.state.pendingApply || this.state.stale || !this.state.dirty;
    this.byId("resetButton").disabled = !this.state.canWrite || this.state.pendingApply || this.state.stale;
    this.byId("reloadButton").disabled = this.state.pendingApply;
    this.byId("formatRawButton").disabled = !editable;
    this.byId("updateFromRawButton").disabled = !editable;
    this.byId("rawFileInput").disabled = !editable;
    this.byId("rawConfig").readOnly = !editable;
    this.byId("copyRawButton").disabled = !this.state.configAvailable;

    var draftState = this.byId("draftState");
    if (this.state.pendingApply) draftState.textContent = "Candidate apply in progress";
    else if (this.state.stale) draftState.textContent = "Stale draft — reload required";
    else if (this.state.dirty) draftState.textContent = "Unapplied browser draft";
    else draftState.textContent = "No draft changes";

    if (!this.state.canWrite) this.byId("actionHint").textContent = capabilityReason(this.state.capability) || "This server exposes the configuration as read-only.";
    else if (this.state.pendingApply) this.byId("actionHint").textContent = "The prior runtime remains active while the candidate is built and preflighted.";
    else if (this.state.stale) this.byId("actionHint").textContent = "Reload the active configuration before another validation or apply.";
    else this.byId("actionHint").textContent = "Validation does not change the active runtime.";
  };

  if (typeof document !== "undefined") {
    document.addEventListener("DOMContentLoaded", function () {
      var editor = new ConfigEditor(document);
      editor.init();
    });
  }
})(typeof globalThis !== "undefined" ? globalThis : this);
