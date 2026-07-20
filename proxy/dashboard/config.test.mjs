import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";

const require = createRequire(import.meta.url);
const helpers = require("./config.js");

class ImportFileReader {
  constructor() {
    this.listeners = new Map();
    this.result = "";
  }

  addEventListener(type, listener) {
    this.listeners.set(type, listener);
  }

  readAsText(file) {
    queueMicrotask(() => {
      if (file.readError) {
        const listener = this.listeners.get("error");
        if (listener) listener();
        return;
      }
      this.result = file.contents;
      const listener = this.listeners.get("load");
      if (listener) listener();
    });
  }
}

async function withImportGlobals(fetchImplementation, callback) {
  const hadFileReader = Object.hasOwn(globalThis, "FileReader");
  const previousFileReader = globalThis.FileReader;
  const hadFetch = Object.hasOwn(globalThis, "fetch");
  const previousFetch = globalThis.fetch;
  globalThis.FileReader = ImportFileReader;
  if (fetchImplementation) globalThis.fetch = fetchImplementation;
  try {
    return await callback();
  } finally {
    if (hadFileReader) globalThis.FileReader = previousFileReader;
    else delete globalThis.FileReader;
    if (hadFetch) globalThis.fetch = previousFetch;
    else delete globalThis.fetch;
  }
}

function makeImportEditor(initialRaw = "existing raw text\n") {
  const rawConfig = { value: initialRaw };
  const editor = new helpers.ConfigEditor({
    getElementById(id) {
      assert.equal(id, "rawConfig");
      return rawConfig;
    }
  });
  const draft = { existing: true };
  const calls = {
    announcements: [],
    errors: [],
    selectedTabs: [],
    clearErrors: 0,
    updateActionState: 0
  };
  editor.state.draft = draft;
  editor.state.csrfToken = "csrf-token";
  editor.announce = (message) => calls.announcements.push(message);
  editor.showErrors = (errors) => calls.errors.push(errors);
  editor.selectTab = (name, commitRaw) => {
    calls.selectedTabs.push([name, commitRaw]);
    editor.state.currentTab = name;
    return true;
  };
  editor.clearErrors = () => { calls.clearErrors++; };
  editor.updateActionState = () => { calls.updateActionState++; };
  return { editor, rawConfig, draft, calls };
}

test("raw import control accepts JSON and YAML", () => {
  const html = readFileSync(new URL("./config.html", import.meta.url), "utf8");
  assert.match(html, />Import JSON or YAML</);
  assert.match(html, /accept="[^"]*\.json[^"]*\.yaml[^"]*\.yml[^"]*"/);
});

test("JSON import stays client-side and stages canonical non-secret JSON", async () => {
  await withImportGlobals(() => assert.fail("JSON import must not call fetch"), async () => {
    const { editor, rawConfig, draft, calls } = makeImportEditor();
    const input = {
      files: [{
        name: "providers.json",
        type: "application/json",
        contents: JSON.stringify({
          schema_version: 2,
          providers: [{
            id: "example",
            type: "openai-compatible",
            api_key: "fixture-a",
            extra_headers: { Authorization: "fixture-b" }
          }],
          future: { enabled: false, limit: 0 }
        })
      }],
      value: "/fake/providers.json"
    };

    assert.equal(await editor.importRawFile({ target: input }), true);
    assert.equal(input.value, "");
    assert.equal(rawConfig.value, JSON.stringify({
      schema_version: 2,
      providers: [{
        id: "example",
        type: "openai-compatible",
        extra_headers: { Authorization: "" }
      }],
      future: { enabled: false, limit: 0 }
    }, null, 2) + "\n");
    assert.strictEqual(editor.state.draft, draft, "import must not bypass commitRawEditor");
    assert.equal(editor.state.rawDirty, true);
    assert.equal(editor.state.dirty, true);
    assert.deepEqual(calls.selectedTabs, [["raw", false]]);
    assert.equal(calls.clearErrors, 1);
    assert.equal(calls.updateActionState, 1);
    assert.deepEqual(calls.errors, []);
    assert.match(calls.announcements.at(-1), /2 secret values were omitted/);
  });
});

test("YAML and YML imports use the conversion endpoint and stage its canonical JSON", async () => {
  for (const filename of ["providers.yaml", "providers.yml"]) {
    const requests = [];
    await withImportGlobals(async (url, options) => {
      requests.push([url, options]);
      return {
        ok: true,
        status: 200,
        async text() {
          return JSON.stringify({
            config: {
              schema_version: 2,
              providers: [{ id: "yaml-provider", type: "copilot" }],
              future: { enabled: false, limit: 0 }
            },
            stripped_secret_paths: ["/providers/yaml-provider/api_key", "/providers/yaml-provider/extra_headers/X-Key"]
          });
        }
      };
    }, async () => {
      const { editor, rawConfig, draft, calls } = makeImportEditor();
      const yaml = "schema_version: 2\nproviders:\n  - id: yaml-provider\n    type: copilot\n";
      const input = {
        files: [{ name: filename, type: "", contents: yaml }],
        value: "/fake/" + filename
      };

      assert.equal(await editor.importRawFile({ target: input }), true);
      assert.equal(input.value, "");
      assert.equal(requests.length, 1);
      assert.equal(requests[0][0], "/dashboard/api/v1/config/import");
      assert.equal(requests[0][1].method, "POST");
      assert.equal(requests[0][1].headers.Accept, "application/json");
      assert.equal(requests[0][1].headers["Content-Type"], "application/yaml");
      assert.equal(requests[0][1].headers["X-Vekil-CSRF"], "csrf-token");
      assert.equal(requests[0][1].body, yaml);
      assert.equal(requests[0][1].cache, "no-store");
      assert.equal(requests[0][1].credentials, "same-origin");
      assert.deepEqual(JSON.parse(rawConfig.value), {
        schema_version: 2,
        providers: [{ id: "yaml-provider", type: "copilot" }],
        future: { enabled: false, limit: 0 }
      });
      assert.ok(rawConfig.value.endsWith("\n"));
      assert.strictEqual(editor.state.draft, draft, "import must not bypass commitRawEditor");
      assert.equal(editor.state.rawDirty, true);
      assert.equal(editor.state.dirty, true);
      assert.deepEqual(calls.selectedTabs, [["raw", false]]);
      assert.match(calls.announcements.at(-1), /2 secret values were omitted/);
    });
  }
});

test("failed YAML import preserves the existing raw text and draft and displays server errors", async () => {
  await withImportGlobals(async () => ({
    ok: false,
    status: 400,
    async text() {
      return JSON.stringify({ error: { path: "/providers/0", message: "duplicate YAML key: id", code: "duplicate_field" } });
    }
  }), async () => {
    const { editor, rawConfig, draft, calls } = makeImportEditor("keep this raw draft\n");
    editor.state.rawDirty = false;
    editor.state.dirty = false;
    const input = {
      files: [{ name: "invalid.yaml", type: "application/yaml", contents: "providers:\n  - id: one\n    id: two\n" }],
      value: "/fake/invalid.yaml"
    };

    assert.equal(await editor.importRawFile({ target: input }), false);
    assert.equal(rawConfig.value, "keep this raw draft\n");
    assert.strictEqual(editor.state.draft, draft);
    assert.equal(editor.state.rawDirty, false);
    assert.equal(editor.state.dirty, false);
    assert.deepEqual(calls.selectedTabs, []);
    assert.equal(calls.clearErrors, 0);
    assert.equal(calls.updateActionState, 0);
    assert.equal(calls.errors.length, 1);
    assert.deepEqual(calls.errors[0], [{ path: "/providers/0", message: "duplicate YAML key: id", code: "duplicate_field" }]);
    assert.match(calls.announcements.at(-1), /current draft was not changed/i);
  });
});

test("invalid client-side JSON import preserves the existing raw text and draft", async () => {
  await withImportGlobals(() => assert.fail("invalid JSON import must not call fetch"), async () => {
    const { editor, rawConfig, draft, calls } = makeImportEditor("keep this JSON draft\n");
    const input = {
      files: [{ name: "invalid.json", type: "application/json", contents: "{not-json" }],
      value: "/fake/invalid.json"
    };

    assert.equal(await editor.importRawFile({ target: input }), false);
    assert.equal(rawConfig.value, "keep this JSON draft\n");
    assert.strictEqual(editor.state.draft, draft);
    assert.equal(editor.state.rawDirty, false);
    assert.equal(editor.state.dirty, false);
    assert.deepEqual(calls.selectedTabs, []);
    assert.equal(calls.errors.length, 1);
    assert.match(calls.errors[0][0].message, /Imported JSON is invalid/);
  });
});

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

test("present empty policy eligibility filters unchanged routes but keeps draft additions and edits", () => {
  const unchanged = { id: "unchanged", public_id: "unchanged", endpoints: ["/responses"] };
  const editedBase = { id: "edited", public_id: "edited", endpoints: ["/responses"] };
  const editor = new helpers.ConfigEditor({});
  editor.state.baseConfig = { model_routes: [unchanged, editedBase] };
  editor.state.draft = {
    model_routes: [
      { ...unchanged },
      { ...editedBase, endpoints: ["/chat/completions"] },
      { id: "new-route", public_id: "new-route", endpoints: ["/chat/completions"] }
    ]
  };
  editor.state.policyEligibility = { terminal_routes: [], classifier_routes: [] };

  assert.deepEqual(
    editor.policyRouteChoices("terminal").map((choice) => choice[0]),
    ["", "edited", "new-route"]
  );
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
