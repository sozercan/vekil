import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const helpers = require("./config.js");

test("normalizes typed and JSON-pointer field paths", () => {
  assert.equal(helpers.normalizeConfigPath("config.providers[2].models[1].public_id"), "/providers/2/models/1/public_id");
  assert.equal(helpers.normalizeConfigPath("/providers/example~1id/api_key"), "/providers/example~1id/api_key");
  assert.equal(helpers.normalizeConfigPath("$"), "/");
});

test("restores server-preserved paths without discarding unknown fields", () => {
  const base = {
    providers: [{ id: "p", type: "copilot" }],
    tool_optimizers: { enabled: true, nested: { limit: 0 } },
    insight_model: "dashboard-model",
    future_field: { keep: true }
  };
  const candidate = {
    providers: [{ id: "p", type: "copilot", default: true }],
    tool_optimizers: { enabled: false },
    insight_model: "changed",
    future_field: { keep: true, added: "untouched" }
  };

  const restored = helpers.restorePreservedPaths(candidate, base, ["/tool_optimizers", "/insight_model"]);
  assert.deepEqual(restored.tool_optimizers, base.tool_optimizers);
  assert.equal(restored.insight_model, "dashboard-model");
  assert.deepEqual(restored.future_field, { keep: true, added: "untouched" });
  assert.equal(restored.providers[0].default, true);
});

test("redacts raw provider secrets and never treats placeholders as stored values", () => {
  const result = helpers.stripSecretValues({
    providers: [
      {
        id: "provider",
        type: "openai-compatible",
        api_key: "***",
        extra_headers: {
          Authorization: "secret",
          "X-Placeholder": "***",
          "X-Empty": ""
        }
      }
    ]
  });

  assert.equal(Object.hasOwn(result.config.providers[0], "api_key"), false);
  assert.deepEqual(result.config.providers[0].extra_headers, {
    Authorization: "",
    "X-Placeholder": "",
    "X-Empty": ""
  });
  assert.equal(result.stripped, 3);
});

test("schema promotion preserves v2 and never masks unsupported future versions", () => {
  assert.deepEqual(
    helpers.enforceSchemaVersion({ schema_version: 1, providers: [] }, 2, false),
    { config: { schema_version: 2, providers: [] }, promoted: true }
  );

  const promoted = helpers.enforceSchemaVersion({
    schema_version: 1,
    providers: [{ id: "p", type: "openai-compatible", trust_domain: "org" }]
  }, 1, false);
  assert.equal(promoted.config.schema_version, 2);
  assert.equal(promoted.promoted, true);

  const future = helpers.enforceSchemaVersion({ schema_version: 3, providers: [] }, 2, true);
  assert.equal(future.config.schema_version, 3);
  assert.equal(future.promoted, false);
});

test("serialization preserves explicit zero and false values while restoring protected fields", () => {
  const result = helpers.serializeConfigDraft({
    schema_version: 2,
    providers: [],
    policy_profiles: [{
      id: "policy",
      classifier: { recent_turns: 0, observe_sample_rate: 0 },
      data_policy: { content_forwarding_acknowledged: false }
    }],
    insight_model: "attempted-change",
    unknown: { value: 0 }
  }, {
    schema_version: 2,
    providers: [],
    insight_model: "preserved-model"
  }, ["/insight_model"], 2, false);

  assert.equal(result.config.policy_profiles[0].classifier.recent_turns, 0);
  assert.equal(result.config.policy_profiles[0].classifier.observe_sample_rate, 0);
  assert.equal(result.config.policy_profiles[0].data_policy.content_forwarding_acknowledged, false);
  assert.equal(result.config.insight_model, "preserved-model");
  assert.deepEqual(result.config.unknown, { value: 0 });
});

test("ordered target movement is deterministic and non-mutating", () => {
  const original = ["primary", "secondary", "tertiary"];
  assert.deepEqual(helpers.moveArrayItem(original, 2, 0), ["tertiary", "primary", "secondary"]);
  assert.deepEqual(original, ["primary", "secondary", "tertiary"]);
  assert.deepEqual(helpers.moveArrayItem(original, -1, 1), original);
});

test("secret operations serialize deterministically and reject unsafe placeholder sets", () => {
  const valid = helpers.buildSecretOperations([
    { path: "/providers/z/api_key", operation: "set", value: "actual-secret", canKeep: false },
    { path: "/providers/a/api_key", operation: "keep", canKeep: true },
    { path: "/providers/m/extra_headers/X-Key", operation: "clear", canKeep: true }
  ]);
  assert.deepEqual(valid.errors, []);
  assert.deepEqual(valid.operations, [
    { path: "/providers/a/api_key", operation: "keep" },
    { path: "/providers/m/extra_headers/X-Key", operation: "clear" },
    { path: "/providers/z/api_key", operation: "set", value: "actual-secret" }
  ]);

  const invalid = helpers.buildSecretOperations([
    { path: "/providers/p/api_key", operation: "set", value: "***", canKeep: false },
    { path: "/providers/q/api_key", operation: "keep", canKeep: false }
  ]);
  assert.equal(invalid.operations.length, 0);
  assert.equal(invalid.errors.length, 2);
  assert.match(invalid.errors[0].message, /non-placeholder/);
  assert.match(invalid.errors[1].message, /cannot be kept/);
});

test("provider secret identity normalizes the base URL and detects auth changes", () => {
  const base = {
    id: "provider",
    type: "openai-compatible",
    base_url: "https://example.test/v1/",
    auth_type: "bearer",
    api_key_env: "PROVIDER_KEY"
  };
  const equivalent = { ...base, base_url: "https://example.test/v1" };
  const changed = { ...equivalent, auth_type: "api-key-header", auth_header: "x-api-key" };

  assert.equal(helpers.providerSecretFingerprint(base), helpers.providerSecretFingerprint(equivalent));
  assert.notEqual(helpers.providerSecretFingerprint(base), helpers.providerSecretFingerprint(changed));
});


test("provider credential change callback retains the editor instance", () => {
  class FakeElement {
    constructor(tagName) {
      this.tagName = tagName;
      this.children = [];
      this.dataset = {};
      this.listeners = new Map();
      this.classList = { add() {} };
    }

    append(...children) {
      this.children.push(...children);
    }

    appendChild(child) {
      this.children.push(child);
      return child;
    }

    addEventListener(type, listener) {
      this.listeners.set(type, listener);
    }

    setAttribute(name, value) {
      this[name] = value;
    }

    dispatch(type) {
      const listener = this.listeners.get(type);
      assert.ok(listener, `expected ${type} listener`);
      listener();
    }
  }

  const elements = [];
  const document = {
    createElement(tagName) {
      const element = new FakeElement(tagName);
      elements.push(element);
      return element;
    }
  };
  const editor = new helpers.ConfigEditor(document);
  const provider = { id: "provider", api_key_env: "OLD_KEY" };
  const calls = [];

  editor.providerFieldSupported = (_providerType, field) => field === "api_key_env";
  editor.providerFieldReadOnly = () => false;
  editor.reconcileProviderSecretCompatibility = (candidate) => calls.push(["reconcile", candidate]);
  editor.renderProviders = (index) => calls.push(["render", index]);

  editor.renderProviderCredentialSection(provider, 2, "openai-compatible");
  const input = elements.find((element) => element.tagName === "input");
  assert.ok(input, "expected API key environment input");

  input.value = "NEW_KEY";
  input.dispatch("change");

  assert.deepEqual(calls, [
    ["reconcile", provider],
    ["render", 2]
  ]);
});

test("capability and apply-state helpers match the wire contract", () => {
  assert.equal(helpers.deriveWriteCapability({ available: true, writable: true, mode: "cli" }), true);
  assert.equal(helpers.deriveWriteCapability({ available: true, writable: false, reason: "read only" }), false);
  assert.equal(helpers.isTerminalApplyStatus("discovering"), false);
  assert.equal(helpers.isTerminalApplyStatus("failed_preflight"), true);
  assert.equal(helpers.isSuccessfulApplyStatus("succeeded"), true);
});
