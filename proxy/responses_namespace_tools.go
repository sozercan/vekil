package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Keep this capability rule narrow: endpoint support alone does not imply
// namespace or hosted-tool support. OpenAI and other Copilot models retain
// their native request contract.
func needsCopilotFlatTools(provider *providerRuntime, path string, body []byte) bool {
	return provider != nil && provider.kind == providerTypeCopilot && path == providerEndpointResponses && strings.HasPrefix(extractRequestModel(body), "grok-")
}

type responsesNamespaceContextKey struct{}
type responsesNamespaceTranslation struct {
	tools   policyResponsesToolMap
	catalog json.RawMessage
}

func translateCopilotNamespaceTools(body []byte) ([]byte, *responsesNamespaceTranslation, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, nil, err
	}
	// Use the path-free scan: the config-diagnostics variant builds a path at
	// every nesting level, which costs quadratic memory on client-supplied
	// schemas that are deep but small.
	if err := validateUnambiguousResponsesJSON(body); err != nil {
		return nil, nil, fmt.Errorf("invalid ambiguous JSON request: %w", err)
	}
	tr := &responsesNamespaceTranslation{tools: make(policyResponsesToolMap), catalog: root["tools"]}
	register := func(d policyResponsesToolDescriptor) (string, error) {
		alias := policyResponsesStableToolAlias(d)
		if prior, ok := tr.tools[alias]; ok && prior != d {
			return "", fmt.Errorf("tool alias collision")
		}
		tr.tools[alias] = d
		return alias, nil
	}
	// The policy parser budgets each catalog independently, and flattening
	// copies a namespace description into every child. Share one budget
	// across all catalogs so many small additional_tools items cannot
	// amplify a request into an arbitrarily large upstream body.
	translatedCatalogBytes := 0
	flatten := func(raw json.RawMessage) (json.RawMessage, error) {
		normalized, optionals := normalizeNativeResponsesToolOptionals(raw)
		parsed, err := parsePolicyResponsesTools(normalized)
		if err != nil {
			return nil, fmt.Errorf("only function tools are supported for Copilot Grok: %w; disable hosted web search in the client and use a function-based search integration", err)
		}
		if optionals != nil && len(optionals) != len(parsed) {
			return nil, errors.New("tool catalog normalization did not match parsed function tools")
		}
		out := make([]map[string]any, 0, len(parsed))
		for i, tool := range parsed {
			alias, err := register(tool.descriptor)
			if err != nil {
				return nil, err
			}
			var opt nativeResponsesToolOptionals
			if optionals != nil {
				opt = optionals[i]
			}
			item := map[string]any{"type": "function", "name": alias, "description": tool.description}
			if opt.descriptionNull && tool.description == "" {
				// Keep a null description as written unless a namespace
				// description was folded into this flattened tool.
				item["description"] = nil
			}
			switch {
			case opt.parametersOmitted:
			case opt.parametersNull:
				item["parameters"] = nil
			default:
				item["parameters"] = tool.parameters
			}
			if opt.strictNull {
				item["strict"] = nil
			} else if tool.strict != nil {
				item["strict"] = *tool.strict
			}
			out = append(out, item)
		}
		value, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		if len(value) > maxLargeRequestBodySize-translatedCatalogBytes {
			return nil, fmt.Errorf("translated Copilot Grok tool catalogs exceed %d bytes", maxLargeRequestBodySize)
		}
		translatedCatalogBytes += len(value)
		return value, nil
	}
	if raw, ok := root["tools"]; ok {
		value, err := flatten(raw)
		if err != nil {
			return nil, nil, err
		}
		root["tools"] = value
	}
	var input []json.RawMessage
	if raw := root["input"]; len(raw) > 0 && bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, nil, err
		}
		for i, raw := range input {
			var item map[string]json.RawMessage
			if json.Unmarshal(raw, &item) != nil {
				continue
			}
			switch rawJSONString(item["type"]) {
			case "additional_tools":
				// Responses Lite catalogs are request-scoped; preserve their position.
				value, err := flatten(item["tools"])
				if err != nil {
					return nil, nil, err
				}
				item["tools"] = value
			case "function_call":
				d := policyResponsesToolDescriptor{Kind: "function", Name: rawJSONString(item["name"]), Namespace: rawJSONString(item["namespace"])}
				alias, err := register(d)
				if err != nil {
					return nil, nil, err
				}
				item["name"], _ = json.Marshal(alias)
				delete(item, "namespace")
			case "reasoning":
				// Codex replays reasoning with "content": null, which Copilot
				// Grok rejects with a bare 400. Omitting it is equivalent.
				if !bytes.Equal(bytes.TrimSpace(item["content"]), []byte("null")) {
					continue
				}
				delete(item, "content")
			default:
				continue
			}
			input[i], _ = json.Marshal(item)
		}
		root["input"], _ = json.Marshal(input)
	}
	if raw := root["tool_choice"]; len(raw) > 0 && bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(raw, &choice); err != nil {
			return nil, nil, err
		}
		rewrite := func(item map[string]json.RawMessage) error {
			if rawJSONString(item["type"]) != "function" {
				return errors.New("only function tool_choice values are supported for Copilot Grok")
			}
			d := policyResponsesToolDescriptor{Kind: "function", Name: rawJSONString(item["name"]), Namespace: rawJSONString(item["namespace"])}
			alias, err := register(d)
			if err != nil {
				return err
			}
			item["name"], _ = json.Marshal(alias)
			delete(item, "namespace")
			return nil
		}
		if rawJSONString(choice["type"]) == "allowed_tools" {
			var allowed []map[string]json.RawMessage
			if err := json.Unmarshal(choice["tools"], &allowed); err != nil {
				return nil, nil, err
			}
			for _, item := range allowed {
				if err := rewrite(item); err != nil {
					return nil, nil, err
				}
			}
			choice["tools"], _ = json.Marshal(allowed)
		} else if err := rewrite(choice); err != nil {
			return nil, nil, err
		}
		root["tool_choice"], _ = json.Marshal(choice)
	}
	out, err := json.Marshal(root)
	return out, tr, err
}

// nativeResponsesToolOptionals records optional native Responses function
// fields that the shared policy parser does not accept as written, so the
// flattened catalog can forward them exactly as the client sent them.
type nativeResponsesToolOptionals struct {
	parametersOmitted bool
	parametersNull    bool
	strictNull        bool
	descriptionNull   bool
}

// normalizeNativeResponsesToolOptionals adapts native Responses function tools
// (top-level or namespace children) for parsePolicyResponsesTools without
// relaxing that parser's contract: omitted or null parameters parse as an
// empty schema, and a null strict or description parses as unset. It returns one entry per
// function tool in parser order, or nil when raw is not a well-formed catalog,
// leaving the parser to report the error.
func normalizeNativeResponsesToolOptionals(raw json.RawMessage) (json.RawMessage, []nativeResponsesToolOptionals) {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil || tools == nil {
		return raw, nil
	}
	isNull := func(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }
	var optionals []nativeResponsesToolOptionals
	normalize := func(tool map[string]json.RawMessage) {
		var opt nativeResponsesToolOptionals
		if params, ok := tool["parameters"]; !ok {
			opt.parametersOmitted = true
			tool["parameters"] = json.RawMessage(`{}`)
		} else if isNull(params) {
			opt.parametersNull = true
			tool["parameters"] = json.RawMessage(`{}`)
		}
		if strict, ok := tool["strict"]; ok && isNull(strict) {
			opt.strictNull = true
			delete(tool, "strict")
		}
		if description, ok := tool["description"]; ok && isNull(description) {
			opt.descriptionNull = true
			delete(tool, "description")
		}
		optionals = append(optionals, opt)
	}
	for _, tool := range tools {
		if tool == nil {
			return raw, nil
		}
		switch rawJSONString(tool["type"]) {
		case "function":
			normalize(tool)
		case "namespace":
			var children []map[string]json.RawMessage
			if json.Unmarshal(tool["tools"], &children) != nil {
				return raw, nil
			}
			for _, child := range children {
				if child == nil || rawJSONString(child["type"]) != "function" {
					return raw, nil
				}
				normalize(child)
			}
			tool["tools"], _ = json.Marshal(children)
		}
	}
	out, err := json.Marshal(tools)
	if err != nil {
		return raw, nil
	}
	return out, optionals
}

// Rewrite only protocol-owned call fields. Tool arguments, output strings,
// schemas and vendor data must never be searched recursively for aliases.
func (tr *responsesNamespaceTranslation) restore(data []byte, eventName string) ([]byte, bool) {
	// Preserve ambiguous bytes for the downstream durable state validator.
	// Decoding into maps would collapse duplicate keys before it sees them.
	if validateUnambiguousResponsesJSON(data) != nil {
		return data, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return data, false
	}
	changed := false
	restoreName := func(item map[string]json.RawMessage) bool {
		d, ok := tr.tools[rawJSONString(item["name"])]
		if !ok {
			return false
		}
		item["name"], _ = json.Marshal(d.Name)
		if d.Namespace != "" {
			item["namespace"], _ = json.Marshal(d.Namespace)
		}
		changed = true
		return true
	}
	call := func(item map[string]json.RawMessage) {
		if rawJSONString(item["type"]) != "function_call" {
			return
		}
		restoreName(item)
	}
	// Echoed choices must reference the restored catalog, not upstream aliases.
	toolChoice := func(obj map[string]json.RawMessage) {
		raw := obj["tool_choice"]
		if len(raw) == 0 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			return
		}
		var choice map[string]json.RawMessage
		if json.Unmarshal(raw, &choice) != nil {
			return
		}
		switch rawJSONString(choice["type"]) {
		case "function":
			if !restoreName(choice) {
				return
			}
		case "allowed_tools":
			var allowed []map[string]json.RawMessage
			if json.Unmarshal(choice["tools"], &allowed) != nil {
				return
			}
			restored := false
			for _, item := range allowed {
				if rawJSONString(item["type"]) == "function" && restoreName(item) {
					restored = true
				}
			}
			if !restored {
				return
			}
			choice["tools"], _ = json.Marshal(allowed)
		default:
			return
		}
		obj["tool_choice"], _ = json.Marshal(choice)
	}
	response := func(obj map[string]json.RawMessage) {
		var output []map[string]json.RawMessage
		if json.Unmarshal(obj["output"], &output) == nil {
			for _, item := range output {
				call(item)
			}
			if changed {
				obj["output"], _ = json.Marshal(output)
			}
		}
		if _, ok := obj["tools"]; ok && len(tr.catalog) > 0 {
			obj["tools"] = tr.catalog
			changed = true
		}
		toolChoice(obj)
	}
	typ := rawJSONString(root["type"])
	if typ == "" {
		// SSE framing may carry the event type only in the event: field.
		typ = eventName
	}
	if typ == "response.output_item.added" || typ == "response.output_item.done" {
		var item map[string]json.RawMessage
		if json.Unmarshal(root["item"], &item) == nil {
			call(item)
			if changed {
				root["item"], _ = json.Marshal(item)
			}
		}
	} else if responsesLifecycleEventHasResponse(typ) {
		var obj map[string]json.RawMessage
		if json.Unmarshal(root["response"], &obj) == nil {
			response(obj)
			if changed {
				root["response"], _ = json.Marshal(obj)
			}
		}
	} else if typ == "response.function_call_arguments.done" {
		// The dispatched type may come from the SSE event name, so do not
		// require the payload's own type field here.
		restoreName(root)
	} else if typ == "" || typ == "response" {
		response(root)
	}
	if !changed {
		return data, false
	}
	out, err := json.Marshal(root)
	return out, err == nil
}

func restoreCopilotNamespaceResponse(req *http.Request, resp *http.Response) {
	if req == nil || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return
	}
	tr, _ := req.Context().Value(responsesNamespaceContextKey{}).(*responsesNamespaceTranslation)
	if tr == nil || len(tr.tools) == 0 {
		return
	}
	if isEventStreamContentType(resp.Header.Get("Content-Type")) {
		// Fail closed: an unrewritten event would expose aliases the client
		// cannot map back to its declared tools.
		wrapResponsesSSEBodyFailClosed(resp, func(raw []byte) ([]byte, bool) { return rewriteResponsesSSEEventData(raw, tr.restore) })
	} else {
		resp.Body = &responsesNamespaceJSONBody{ReadCloser: resp.Body, tr: tr}
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}
}

type responsesNamespaceJSONBody struct {
	io.ReadCloser
	tr     *responsesNamespaceTranslation
	reader *bytes.Reader
}

func (b *responsesNamespaceJSONBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		raw, err := io.ReadAll(io.LimitReader(b.ReadCloser, maxLargeRequestBodySize+1))
		if err != nil {
			return 0, err
		}
		if len(raw) > maxLargeRequestBodySize {
			return 0, errors.New("responses tool translation exceeds response size limit")
		}
		if rewritten, ok := b.tr.restore(raw, ""); ok {
			raw = rewritten
		}
		b.reader = bytes.NewReader(raw)
	}
	return b.reader.Read(p)
}
func (b *responsesNamespaceJSONBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	return routeAttemptTransportOwnership(b.ReadCloser)
}
func (b *responsesNamespaceJSONBody) cancelRouteAttempt() { cancelRouteAttemptBody(b.ReadCloser) }

func prepareCopilotNamespaceTools(ctx context.Context, provider *providerRuntime, path string, body []byte) (context.Context, []byte, error) {
	if !needsCopilotFlatTools(provider, path, body) {
		return ctx, body, nil
	}
	rewritten, tr, err := translateCopilotNamespaceTools(body)
	if err != nil {
		return ctx, nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	return context.WithValue(ctx, responsesNamespaceContextKey{}, tr), rewritten, nil
}

func (b *responsesNamespaceJSONBody) canceledAtFailure() bool {
	if observed, ok := b.ReadCloser.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}
