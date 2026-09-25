package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// localAIToolSeparator joins a namespace and a child tool name into the
// flattened function name LocalAI sees, matching Codex's mcp__server__tool
// convention.
const localAIToolSeparator = "__"

// localAIToolAlias restores a flattened function name to its namespace tool.
type localAIToolAlias struct {
	namespace string
	name      string
}

type localAIToolAliases map[string]localAIToolAlias

// localAIAliasBodyLimit bounds how much of a non-streaming reply is buffered
// to restore tool names.
var localAIAliasBodyLimit = 64 << 20

// flattenLocalAINamespaceTools replaces Responses namespace tools with their
// function children, because LocalAI serves only top-level function tools.
// Function-call history items that name a namespace are flattened the same
// way. The returned aliases map flattened names back for the response.
func flattenLocalAINamespaceTools(body []byte) ([]byte, localAIToolAliases, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, nil, nil
	}
	var tools []map[string]json.RawMessage
	if raw, ok := payload["tools"]; ok && json.Unmarshal(raw, &tools) != nil {
		return body, nil, nil
	}
	topLevel := map[string]bool{}
	for _, tool := range tools {
		if jsonStringField(tool, "type") == "function" {
			topLevel[jsonStringField(tool, "name")] = true
		}
	}
	aliases := localAIToolAliases{}
	sawNamespace := false
	flattened := make([]map[string]json.RawMessage, 0, len(tools))
	for index, tool := range tools {
		if jsonStringField(tool, "type") != "namespace" {
			flattened = append(flattened, tool)
			continue
		}
		sawNamespace = true
		namespace := jsonStringField(tool, "name")
		if namespace == "" {
			return nil, nil, localAIToolError(fmt.Sprintf("tools[%d].name", index), "namespace tools need a name")
		}
		namespaceDescription := jsonStringField(tool, "description")
		var children []map[string]json.RawMessage
		if json.Unmarshal(tool["tools"], &children) != nil {
			return nil, nil, localAIToolError(fmt.Sprintf("tools[%d].tools", index), "is not a tool list")
		}
		for childIndex, child := range children {
			if jsonStringField(child, "type") != "function" {
				return nil, nil, localAIToolError(fmt.Sprintf("tools[%d].tools[%d].type", index, childIndex), "only function tools are supported inside a namespace")
			}
			name := jsonStringField(child, "name")
			alias := namespace + localAIToolSeparator + name
			if topLevel[alias] || aliases[alias] != (localAIToolAlias{}) {
				return nil, nil, localAIToolError(fmt.Sprintf("tools[%d].tools[%d].name", index, childIndex), fmt.Sprintf("flattened name %q collides with another tool", alias))
			}
			aliases[alias] = localAIToolAlias{namespace: namespace, name: name}
			flat := make(map[string]json.RawMessage, len(child))
			for key, value := range child {
				if key == "defer_loading" {
					continue
				}
				flat[key] = value
			}
			flat["name"] = mustMarshalJSON(alias)
			if description := combinePolicyResponsesToolDescriptions(namespaceDescription, jsonStringField(child, "description")); description != "" {
				flat["description"] = mustMarshalJSON(description)
			}
			flattened = append(flattened, flat)
		}
	}
	if sawNamespace {
		// An empty namespace has no callable tools; drop it rather than send a
		// declaration LocalAI would ignore.
		payload["tools"] = mustMarshalJSON(flattened)
	}
	if raw, ok := payload["input"]; ok {
		var items []map[string]json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			changed := false
			for index, item := range items {
				namespace := jsonStringField(item, "namespace")
				if namespace == "" || jsonStringField(item, "type") != "function_call" {
					continue
				}
				name := jsonStringField(item, "name")
				alias := namespace + localAIToolSeparator + name
				if topLevel[alias] {
					return nil, nil, localAIToolError(fmt.Sprintf("input[%d].name", index), fmt.Sprintf("flattened name %q collides with another tool", alias))
				}
				item["name"] = mustMarshalJSON(alias)
				delete(item, "namespace")
				if _, known := aliases[alias]; !known {
					aliases[alias] = localAIToolAlias{namespace: namespace, name: name}
				}
				changed = true
			}
			if changed {
				payload["input"] = mustMarshalJSON(items)
			}
		}
	}
	if len(aliases) == 0 && !sawNamespace {
		return body, nil, nil
	}
	if raw, ok := payload["tool_choice"]; ok {
		if rewritten, changed := flattenLocalAIToolChoice(raw); changed {
			payload["tool_choice"] = rewritten
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	if len(aliases) == 0 {
		return out, nil, nil
	}
	return out, aliases, nil
}

// flattenLocalAIToolChoice renames namespaced function choices, including the
// entries of an allowed_tools choice, to their flattened names.
func flattenLocalAIToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil {
		return raw, false
	}
	changed := flattenNamespacedToolReference(choice)
	if jsonStringField(choice, "type") == "allowed_tools" {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(choice["tools"], &tools) == nil {
			toolsChanged := false
			for _, tool := range tools {
				if flattenNamespacedToolReference(tool) {
					toolsChanged = true
				}
			}
			if toolsChanged {
				choice["tools"] = mustMarshalJSON(tools)
				changed = true
			}
		}
	}
	if !changed {
		return raw, false
	}
	return mustMarshalJSON(choice), true
}

func flattenNamespacedToolReference(reference map[string]json.RawMessage) bool {
	namespace := jsonStringField(reference, "namespace")
	if namespace == "" || jsonStringField(reference, "type") != "function" {
		return false
	}
	reference["name"] = mustMarshalJSON(namespace + localAIToolSeparator + jsonStringField(reference, "name"))
	delete(reference, "namespace")
	return true
}

func localAIToolError(param, detail string) error {
	return &providerRequestError{statusCode: http.StatusBadRequest, code: "unsupported_tool_type", err: fmt.Errorf("%s %s", param, detail)}
}

// restoreItem rewrites a flattened function_call item in place.
func (a localAIToolAliases) restoreItem(item map[string]json.RawMessage) bool {
	if jsonStringField(item, "type") != "function_call" {
		return false
	}
	alias, ok := a[jsonStringField(item, "name")]
	if !ok {
		return false
	}
	item["name"] = mustMarshalJSON(alias.name)
	item["namespace"] = mustMarshalJSON(alias.namespace)
	return true
}

func (a localAIToolAliases) restoreItems(raw json.RawMessage) (json.RawMessage, bool) {
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw, false
	}
	changed := false
	for _, item := range items {
		if a.restoreItem(item) {
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	return mustMarshalJSON(items), true
}

// restoreResponse rewrites the output of a Responses object.
func (a localAIToolAliases) restoreResponse(raw json.RawMessage) (json.RawMessage, bool) {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return raw, false
	}
	output, changed := a.restoreItems(response["output"])
	if !changed {
		return raw, false
	}
	response["output"] = output
	return mustMarshalJSON(response), true
}

// restoreEvent rewrites one Responses stream event payload.
func (a localAIToolAliases) restoreEvent(data []byte) []byte {
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return data
	}
	changed := false
	if raw, ok := event["item"]; ok {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil && a.restoreItem(item) {
			event["item"] = mustMarshalJSON(item)
			changed = true
		}
	}
	if raw, ok := event["response"]; ok {
		if restored, ok := a.restoreResponse(raw); ok {
			event["response"] = restored
			changed = true
		}
	}
	if strings.HasPrefix(jsonStringField(event, "type"), "response.function_call_arguments.") {
		if alias, ok := a[jsonStringField(event, "name")]; ok {
			event["name"] = mustMarshalJSON(alias.name)
			event["namespace"] = mustMarshalJSON(alias.namespace)
			changed = true
		}
	}
	if !changed {
		return data
	}
	return mustMarshalJSON(event)
}

// restoreLocalAIToolAliases rewrites a Responses reply so flattened tool calls
// carry their original namespace and name.
func restoreLocalAIToolAliases(resp *http.Response, aliases localAIToolAliases) *http.Response {
	if len(aliases) == 0 || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return resp
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = newLocalAIAliasStream(resp.Body, aliases)
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return resp
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(localAIAliasBodyLimit)+1))
	if err != nil || len(body) > localAIAliasBodyLimit {
		// Too large or unreadable to rewrite: pass it through unchanged.
		resp.Body = newLocalAIPrefixedBody(body, resp.Body)
		return resp
	}
	_ = resp.Body.Close()
	if restored, ok := aliases.restoreResponse(body); ok {
		body = restored
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return resp
}

// localAIAliasStream rewrites complete SSE events as they arrive.
type localAIAliasStream struct {
	localAIBodyLifecycle
	reader  *bufio.Reader
	aliases localAIToolAliases
	pending []byte
	event   bytes.Buffer
	err     error
}

func newLocalAIAliasStream(source io.ReadCloser, aliases localAIToolAliases) *localAIAliasStream {
	return &localAIAliasStream{localAIBodyLifecycle: localAIBodyLifecycle{source: source}, reader: bufio.NewReaderSize(source, 64<<10), aliases: aliases}
}

func (s *localAIAliasStream) Read(buf []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		line, err := s.reader.ReadBytes('\n')
		s.event.Write(line)
		if err != nil {
			s.err = err
			s.pending = s.rewrite(s.event.Bytes())
			s.event.Reset()
			continue
		}
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			s.pending = s.rewrite(s.event.Bytes())
			s.event.Reset()
		}
	}
	n := copy(buf, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *localAIAliasStream) rewrite(event []byte) []byte {
	if !bytes.Contains(event, []byte("function_call")) {
		return append([]byte(nil), event...)
	}
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(event, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			out.Write(line)
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		out.WriteString("data: ")
		out.Write(s.aliases.restoreEvent(payload))
		out.Write(line[len(trimmed):])
	}
	return out.Bytes()
}

func (s *localAIAliasStream) Close() error { return s.source.Close() }
