package proxy

import (
	"bytes"
	"context"
	"encoding/json"
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
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return nil, nil, err
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
	flatten := func(raw json.RawMessage) (json.RawMessage, error) {
		parsed, err := parsePolicyResponsesTools(raw)
		if err != nil {
			return nil, fmt.Errorf("Copilot Grok supports function tools only: %w; disable hosted web search in the client and use a function-based search integration", err)
		}
		out := make([]map[string]any, 0, len(parsed))
		for _, tool := range parsed {
			alias, err := register(tool.descriptor)
			if err != nil {
				return nil, err
			}
			item := map[string]any{"type": "function", "name": alias, "description": tool.description, "parameters": tool.parameters}
			if tool.strict != nil {
				item["strict"] = *tool.strict
			}
			out = append(out, item)
		}
		return json.Marshal(out)
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
				return fmt.Errorf("Copilot Grok tool_choice supports function tools only")
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

// Rewrite only protocol-owned call fields. Tool arguments, output strings,
// schemas and vendor data must never be searched recursively for aliases.
func (tr *responsesNamespaceTranslation) restore(data []byte, _ string) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return data, false
	}
	changed := false
	call := func(item map[string]json.RawMessage) {
		if typ := rawJSONString(item["type"]); typ != "function_call" && typ != "response.function_call_arguments.done" {
			return
		}
		d, ok := tr.tools[rawJSONString(item["name"])]
		if !ok {
			return
		}
		item["name"], _ = json.Marshal(d.Name)
		if d.Namespace != "" {
			item["namespace"], _ = json.Marshal(d.Namespace)
		}
		changed = true
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
	}
	typ := rawJSONString(root["type"])
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
		call(root)
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
	if tr == nil {
		return
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		wrapResponsesSSEBody(resp, func(raw []byte) ([]byte, bool) { return rewriteResponsesSSEEventData(raw, tr.restore) })
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
			return 0, fmt.Errorf("Responses tool translation exceeds response size limit")
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
