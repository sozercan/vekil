package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsesChatReplayPublishesOpaqueIDsAndResolvesFullProjection(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	originalArguments := `{"query":"synthetic"}`
	request := responsesChatReplayPublishRequest{
		Route: responsesChatReplayRoute{
			ProviderID:    "provider-a",
			PublicModel:   "model-a",
			UpstreamModel: "deployment-a",
		},
		AssistantContent: json.RawMessage(`null`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"reasoning_synthetic","encrypted_content":"synthetic_not_replayable"}`),
			json.RawMessage(`{"type":"function_call","id":"item_synthetic","call_id":"upstream-call-1","name":"lookup","arguments":"{\"query\":\"synthetic\"}"}`),
		},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID:   "upstream-call-1",
			Name:             "lookup",
			VisibleArguments: originalArguments,
			OutputItemIndex:  1,
		}},
	}

	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if len(published.Projection.Calls) != 1 {
		t.Fatalf("published call count = %d, want 1", len(published.Projection.Calls))
	}
	proxyID := published.Projection.Calls[0].ID
	if !strings.HasPrefix(proxyID, responsesChatReplayCallIDPrefix) {
		t.Fatalf("proxy ID = %q, want prefix %q", proxyID, responsesChatReplayCallIDPrefix)
	}
	encoded := strings.TrimPrefix(proxyID, responsesChatReplayCallIDPrefix)
	if len(encoded) != 22 {
		t.Fatalf("encoded proxy ID length = %d, want 22", len(encoded))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode proxy ID: %v", err)
	}
	if len(decoded) != 16 {
		t.Fatalf("decoded proxy ID length = %d, want 16", len(decoded))
	}

	resolved, err := store.Resolve(request.Route, published.Projection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.ProjectionMatch != responsesChatReplayProjectionVisible {
		t.Fatalf("ProjectionMatch = %v, want visible", resolved.ProjectionMatch)
	}
	if len(resolved.OutputItems) != len(request.OutputItems) {
		t.Fatalf("resolved item count = %d, want %d", len(resolved.OutputItems), len(request.OutputItems))
	}
	for i := range request.OutputItems {
		if !bytes.Equal(resolved.OutputItems[i], request.OutputItems[i]) {
			t.Fatalf("resolved item %d = %s, want %s", i, resolved.OutputItems[i], request.OutputItems[i])
		}
	}
	if len(resolved.Calls) != 1 {
		t.Fatalf("resolved call count = %d, want 1", len(resolved.Calls))
	}
	call := resolved.Calls[0]
	if call.ProxyCallID != proxyID || call.UpstreamCallID != "upstream-call-1" || call.OutputItemIndex != 1 {
		t.Fatalf("resolved call = %+v", call)
	}
	if !bytes.Equal(call.OutputItem, request.OutputItems[1]) {
		t.Fatalf("resolved call output item = %s, want %s", call.OutputItem, request.OutputItems[1])
	}
}

func TestResponsesChatReplayMissingAndCrossRouteAreTypedStateLoss(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	request := newResponsesChatReplayTestRequest("missing", replayTestCallSpec{
		upstreamID: "upstream-missing",
		name:       "lookup",
		visible:    `{"value":"synthetic"}`,
	})
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	forged := cloneReplayTestProjection(published.Projection)
	forged.Calls[0].ID = responsesChatReplayCallIDPrefix + strings.Repeat("A", 22)
	assertResponsesChatReplayMissing(t, func() error {
		_, resolveErr := store.Resolve(request.Route, forged)
		return resolveErr
	}())

	routeMutations := []struct {
		name   string
		mutate func(*responsesChatReplayRoute)
	}{
		{name: "provider", mutate: func(route *responsesChatReplayRoute) { route.ProviderID = "provider-b" }},
		{name: "public model", mutate: func(route *responsesChatReplayRoute) { route.PublicModel = "model-b" }},
		{name: "upstream model", mutate: func(route *responsesChatReplayRoute) { route.UpstreamModel = "deployment-b" }},
	}
	for _, mutation := range routeMutations {
		t.Run("cross "+mutation.name, func(t *testing.T) {
			otherRoute := request.Route
			mutation.mutate(&otherRoute)
			assertResponsesChatReplayMissing(t, func() error {
				_, resolveErr := store.Resolve(otherRoute, published.Projection)
				return resolveErr
			}())
		})
	}

	if _, err := store.Resolve(request.Route, published.Projection); err != nil {
		t.Fatalf("same-route Resolve() after rejected lookups error = %v", err)
	}
}

type replayTestCallSpec struct {
	upstreamID string
	name       string
	visible    string
	original   *string
}

func newResponsesChatReplayTestRequest(tag string, specs ...replayTestCallSpec) responsesChatReplayPublishRequest {
	items := []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"reasoning_` + tag + `","encrypted_content":"synthetic"}`)}
	calls := make([]responsesChatReplayPublishCall, len(specs))
	for i, spec := range specs {
		original := spec.visible
		if spec.original != nil {
			original = *spec.original
		}
		item, err := json.Marshal(map[string]any{
			"type":      "function_call",
			"id":        "item_" + tag,
			"call_id":   spec.upstreamID,
			"name":      spec.name,
			"arguments": original,
		})
		if err != nil {
			panic(err)
		}
		items = append(items, item)
		calls[i] = responsesChatReplayPublishCall{
			UpstreamCallID:    spec.upstreamID,
			Name:              spec.name,
			VisibleArguments:  spec.visible,
			OriginalArguments: spec.original,
			OutputItemIndex:   i + 1,
		}
	}
	return responsesChatReplayPublishRequest{
		Route: responsesChatReplayRoute{
			ProviderID:    "provider-a",
			PublicModel:   "model-a",
			UpstreamModel: "deployment-a",
		},
		AssistantContent: json.RawMessage(`null`),
		OutputItems:      items,
		Calls:            calls,
	}
}

func cloneReplayTestProjection(projection responsesChatReplayAssistantProjection) responsesChatReplayAssistantProjection {
	cloned := responsesChatReplayAssistantProjection{
		Content: bytes.Clone(projection.Content),
		Calls:   append([]responsesChatReplayProjectedCall(nil), projection.Calls...),
	}
	return cloned
}

func assertResponsesChatReplayMissing(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errResponsesChatReplayMissing) {
		t.Fatalf("error = %v, want errors.Is(missing)", err)
	}
	var missing *responsesChatReplayMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error type = %T, want *responsesChatReplayMissingError", err)
	}
	if got := missing.Error(); got != responsesChatReplayMissingMessage {
		t.Fatalf("missing error message = %q, want %q", got, responsesChatReplayMissingMessage)
	}
	if got := missing.ReplayCode(); got != responsesChatReplayMissingCode {
		t.Fatalf("missing error code = %q, want %q", got, responsesChatReplayMissingCode)
	}
}

func TestResponsesChatReplayRequiresTheFullOrderedAssistantProjection(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	request := newResponsesChatReplayTestRequest("projection",
		replayTestCallSpec{upstreamID: "upstream-1", name: "first", visible: `{"n":1}`},
		replayTestCallSpec{upstreamID: "upstream-2", name: "second", visible: `{"n":2}`},
	)
	request.AssistantContent = json.RawMessage(`{"b":2,"a":1}`)
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	semanticallyEquivalent := cloneReplayTestProjection(published.Projection)
	semanticallyEquivalent.Content = json.RawMessage(`{"a":1,"b":2}`)
	if _, err := store.Resolve(request.Route, semanticallyEquivalent); err != nil {
		t.Fatalf("Resolve() semantically equivalent content error = %v", err)
	}
	semanticallyEquivalentArguments := cloneReplayTestProjection(published.Projection)
	semanticallyEquivalentArguments.Calls[0].Arguments = `{ "n" : 1 }`
	if _, err := store.Resolve(request.Route, semanticallyEquivalentArguments); err != nil {
		t.Fatalf("Resolve() semantically equivalent arguments error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*responsesChatReplayAssistantProjection)
	}{
		{
			name: "missing call",
			mutate: func(projection *responsesChatReplayAssistantProjection) {
				projection.Calls = projection.Calls[:1]
			},
		},
		{
			name: "reordered calls",
			mutate: func(projection *responsesChatReplayAssistantProjection) {
				projection.Calls[0], projection.Calls[1] = projection.Calls[1], projection.Calls[0]
			},
		},
		{
			name: "changed name",
			mutate: func(projection *responsesChatReplayAssistantProjection) {
				projection.Calls[0].Name = "other"
			},
		},
		{
			name: "changed arguments",
			mutate: func(projection *responsesChatReplayAssistantProjection) {
				projection.Calls[0].Arguments = `{"n":99}`
			},
		},
		{
			name: "changed content",
			mutate: func(projection *responsesChatReplayAssistantProjection) {
				projection.Content = json.RawMessage(`{"a":1,"b":3}`)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := cloneReplayTestProjection(published.Projection)
			test.mutate(&projection)
			_, resolveErr := store.Resolve(request.Route, projection)
			assertResponsesChatReplayProjection(t, resolveErr)
		})
	}
}

func assertResponsesChatReplayProjection(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errResponsesChatReplayProjection) {
		t.Fatalf("error = %v, want errors.Is(projection)", err)
	}
	var projection *responsesChatReplayProjectionError
	if !errors.As(err, &projection) {
		t.Fatalf("error type = %T, want *responsesChatReplayProjectionError", err)
	}
	if got := projection.Error(); got != responsesChatReplayProjectionMessage {
		t.Fatalf("projection error message = %q, want %q", got, responsesChatReplayProjectionMessage)
	}
	if got := projection.ReplayCode(); got != responsesChatReplayProjectionCode {
		t.Fatalf("projection error code = %q, want %q", got, responsesChatReplayProjectionCode)
	}
}

func TestResponsesChatReplayRejectsMixedGroupsWithTypedError(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	firstRequest := newResponsesChatReplayTestRequest("mixed-first", replayTestCallSpec{
		upstreamID: "upstream-first", name: "first", visible: `{}`,
	})
	secondRequest := newResponsesChatReplayTestRequest("mixed-second", replayTestCallSpec{
		upstreamID: "upstream-second", name: "second", visible: `{}`,
	})
	first, err := store.Publish(firstRequest)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	second, err := store.Publish(secondRequest)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}

	mixed := responsesChatReplayAssistantProjection{
		Content: json.RawMessage(`null`),
		Calls: []responsesChatReplayProjectedCall{
			first.Projection.Calls[0],
			second.Projection.Calls[0],
		},
	}
	_, err = store.Resolve(firstRequest.Route, mixed)
	if !errors.Is(err, errResponsesChatReplayMixed) {
		t.Fatalf("Resolve() error = %v, want errors.Is(mixed)", err)
	}
	var mixedErr *responsesChatReplayMixedError
	if !errors.As(err, &mixedErr) {
		t.Fatalf("Resolve() error type = %T, want *responsesChatReplayMixedError", err)
	}
	if mixedErr.Error() != responsesChatReplayMixedMessage || mixedErr.ReplayCode() != responsesChatReplayMixedCode {
		t.Fatalf("mixed error = %q code=%q", mixedErr.Error(), mixedErr.ReplayCode())
	}
}

func TestResponsesChatReplayValidatesVisibleAndOriginalOptimizerProjections(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	originalOne := `{"value":"original-one"}`
	originalTwo := `{"value":"original-two"}`
	request := newResponsesChatReplayTestRequest("optimizer",
		replayTestCallSpec{
			upstreamID: "upstream-1",
			name:       "first",
			visible:    `{"value":"visible-one"}`,
			original:   &originalOne,
		},
		replayTestCallSpec{
			upstreamID: "upstream-2",
			name:       "second",
			visible:    `{"value":"visible-two"}`,
			original:   &originalTwo,
		},
	)
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	visible, err := store.Resolve(request.Route, published.Projection)
	if err != nil {
		t.Fatalf("Resolve(visible) error = %v", err)
	}
	if visible.ProjectionMatch != responsesChatReplayProjectionVisible {
		t.Fatalf("visible ProjectionMatch = %v", visible.ProjectionMatch)
	}
	original, err := store.Resolve(request.Route, published.OriginalProjection)
	if err != nil {
		t.Fatalf("Resolve(original) error = %v", err)
	}
	if original.ProjectionMatch != responsesChatReplayProjectionOriginal {
		t.Fatalf("original ProjectionMatch = %v", original.ProjectionMatch)
	}

	hybrid := cloneReplayTestProjection(published.Projection)
	hybrid.Calls[1].Arguments = published.OriginalProjection.Calls[1].Arguments
	_, err = store.Resolve(request.Route, hybrid)
	assertResponsesChatReplayProjection(t, err)
}

func TestResponsesChatReplayOptionalDefaultsArePublicationOwned(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	defaults := responsesChatReplayOptionalDefaults{"replace_all": json.RawMessage("false")}
	request := newResponsesChatReplayTestRequest("defaults", replayTestCallSpec{
		upstreamID: "upstream-default", name: "edit", visible: `{}`,
	})
	request.Calls[0].OptionalDefaults = defaults
	published, err := store.Publish(request)
	if err != nil {
		t.Fatal(err)
	}
	defaults["replace_all"] = json.RawMessage("true")
	call := published.Projection.Calls[0]
	call.Arguments = `{"replace_all":false}`
	if _, err := store.Resolve(request.Route, responsesChatReplayAssistantProjection{Content: published.Projection.Content, Calls: []responsesChatReplayProjectedCall{call}}); err != nil {
		t.Fatalf("published optional default was not accepted: %v", err)
	}
	for _, arguments := range []string{`{"replace_all":true}`, `{"admin":true}`} {
		call.Arguments = arguments
		_, err := store.Resolve(request.Route, responsesChatReplayAssistantProjection{Content: published.Projection.Content, Calls: []responsesChatReplayProjectedCall{call}})
		assertResponsesChatReplayProjection(t, err)
	}
}

func TestResponsesChatReplayOptionalDefaultsPreserveStoredDefaultValues(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	request := newResponsesChatReplayTestRequest("stored-default", replayTestCallSpec{
		upstreamID: "upstream-stored-default", name: "edit", visible: `{"mode":"standard"}`,
	})
	request.Calls[0].OptionalDefaults = responsesChatReplayOptionalDefaults{
		"mode":    json.RawMessage(`"standard"`),
		"verbose": json.RawMessage("false"),
	}
	published, err := store.Publish(request)
	if err != nil {
		t.Fatal(err)
	}
	call := published.Projection.Calls[0]
	call.Arguments = `{"mode":"standard","verbose":false}`
	if _, err := store.Resolve(request.Route, responsesChatReplayAssistantProjection{Content: published.Projection.Content, Calls: []responsesChatReplayProjectedCall{call}}); err != nil {
		t.Fatalf("inserted optional default changed a stored default-valued property: %v", err)
	}
	for _, arguments := range []string{
		`{"verbose":false}`,
		`{"mode":"other","verbose":false}`,
		`{"mode":"standard","verbose":true}`,
	} {
		call.Arguments = arguments
		_, err = store.Resolve(request.Route, responsesChatReplayAssistantProjection{Content: published.Projection.Content, Calls: []responsesChatReplayProjectedCall{call}})
		assertResponsesChatReplayProjection(t, err)
	}
}

func TestResponsesChatReplayOptionalDefaultsAreProjectionSpecific(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	original := `{"mode":"standard"}`
	request := newResponsesChatReplayTestRequest("projection-defaults", replayTestCallSpec{
		upstreamID: "upstream-projection-defaults", name: "edit", visible: `{"view":"optimized"}`, original: &original,
	})
	request.Calls[0].OptionalDefaults = responsesChatReplayOptionalDefaults{
		"mode":    json.RawMessage(`"standard"`),
		"verbose": json.RawMessage("false"),
	}
	published, err := store.Publish(request)
	if err != nil {
		t.Fatal(err)
	}

	visible := published.Projection
	visible.Calls[0].Arguments = `{"view":"optimized","mode":"standard"}`
	visibleResolution, err := store.Resolve(request.Route, visible)
	if err != nil || visibleResolution.ProjectionMatch != responsesChatReplayProjectionVisible {
		t.Fatalf("visible projection default resolution = match:%v error:%v", visibleResolution.ProjectionMatch, err)
	}

	originalProjection := published.OriginalProjection
	originalProjection.Calls[0].Arguments = `{"mode":"standard","verbose":false}`
	originalResolution, err := store.Resolve(request.Route, originalProjection)
	if err != nil || originalResolution.ProjectionMatch != responsesChatReplayProjectionOriginal {
		t.Fatalf("original projection default resolution = match:%v error:%v", originalResolution.ProjectionMatch, err)
	}
}

func TestResponsesChatReplayOptionalDefaultsAllowOpaquePropertyNames(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	request := newResponsesChatReplayTestRequest("opaque-default-names", replayTestCallSpec{
		upstreamID: "upstream-opaque-default-names", name: "edit", visible: `{"":false,"   ":"standard"}`,
	})
	request.Calls[0].OptionalDefaults = responsesChatReplayOptionalDefaults{
		"":    json.RawMessage("false"),
		"   ": json.RawMessage(`"standard"`),
	}
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() rejected opaque JSON property names: %v", err)
	}
	if _, err := store.Resolve(request.Route, published.Projection); err != nil {
		t.Fatalf("Resolve() rejected exact opaque-name projection: %v", err)
	}
}

func TestResponsesChatReplayResolutionSupportsFullOrPerCallPartialReplay(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	request := responsesChatReplayPublishRequest{
		Route: responsesChatReplayRoute{
			ProviderID:    "provider-a",
			PublicModel:   "model-a",
			UpstreamModel: "deployment-a",
		},
		AssistantContent: json.RawMessage(`"synthetic preface"`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"reasoning-partial","encrypted_content":"synthetic"}`),
			json.RawMessage(`{"type":"message","id":"message-partial","role":"assistant","content":[{"type":"output_text","text":"synthetic preface"}]}`),
			json.RawMessage(`{"type":"function_call","id":"item-partial-1","call_id":"upstream-partial-1","name":"first","arguments":"{\"n\":1}"}`),
			json.RawMessage(`{"type":"function_call","id":"item-partial-2","call_id":"upstream-partial-2","name":"second","arguments":"{\"n\":2}"}`),
		},
		Calls: []responsesChatReplayPublishCall{
			{UpstreamCallID: "upstream-partial-1", Name: "first", VisibleArguments: `{"n":1}`, OutputItemIndex: 2},
			{UpstreamCallID: "upstream-partial-2", Name: "second", VisibleArguments: `{"n":2}`, OutputItemIndex: 3},
		},
	}
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	resolved, err := store.Resolve(request.Route, published.Projection)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if len(resolved.OutputItems) != 4 {
		t.Fatalf("full replay item count = %d, want 4", len(resolved.OutputItems))
	}
	for i := range request.OutputItems {
		if !bytes.Equal(resolved.OutputItems[i], request.OutputItems[i]) {
			t.Fatalf("full replay item %d changed", i)
		}
	}
	if len(resolved.Calls) != 2 {
		t.Fatalf("resolved call count = %d, want 2", len(resolved.Calls))
	}
	second := resolved.Calls[1]
	if second.ProxyCallID != published.Projection.Calls[1].ID || second.UpstreamCallID != "upstream-partial-2" {
		t.Fatalf("second resolved call = %+v", second)
	}
	if second.OutputItemIndex != 3 || !bytes.Equal(second.OutputItem, request.OutputItems[3]) {
		t.Fatalf("second per-call replay item = index %d body %s", second.OutputItemIndex, second.OutputItem)
	}
}

func TestResponsesChatReplayDefensivelyCopiesPublishAndResolveData(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	request := newResponsesChatReplayTestRequest("copies", replayTestCallSpec{
		upstreamID: "upstream-copies", name: "copy", visible: `{"copy":true}`,
	})
	request.AssistantContent = json.RawMessage(`{"text":"synthetic"}`)
	wantItems := cloneReplayRawMessages(request.OutputItems)
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	projection := cloneReplayTestProjection(published.Projection)

	request.OutputItems[0][0] = 'X'
	request.AssistantContent[0] = 'X'
	published.Projection.Content[0] = 'X'
	published.Projection.Calls[0].Arguments = `{"copy":false}`
	published.Calls[0].OutputItem[0] = 'X'

	resolved, err := store.Resolve(request.Route, projection)
	if err != nil {
		t.Fatalf("Resolve() after caller mutations error = %v", err)
	}
	for i := range wantItems {
		if !bytes.Equal(resolved.OutputItems[i], wantItems[i]) {
			t.Fatalf("resolved item %d was aliased: %s", i, resolved.OutputItems[i])
		}
	}
	resolved.OutputItems[0][0] = 'Y'
	resolved.Calls[0].OutputItem[0] = 'Y'

	again, err := store.Resolve(request.Route, projection)
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}
	for i := range wantItems {
		if !bytes.Equal(again.OutputItems[i], wantItems[i]) {
			t.Fatalf("second resolved item %d was aliased: %s", i, again.OutputItems[i])
		}
	}
	if !bytes.Equal(again.Calls[0].OutputItem, wantItems[1]) {
		t.Fatalf("second per-call output item was aliased: %s", again.Calls[0].OutputItem)
	}
}

func TestResponsesChatReplayTTLIsAbsoluteAndDoesNotSlideOnResolve(t *testing.T) {
	if responsesChatReplayTTL != time.Hour {
		t.Fatalf("responsesChatReplayTTL = %v, want 1h", responsesChatReplayTTL)
	}
	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(base.UnixNano())
	store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{
		Now: func() time.Time { return time.Unix(0, nowNanos.Load()) },
	})
	defer func() { _ = store.Close() }()

	request := newResponsesChatReplayTestRequest("ttl", replayTestCallSpec{
		upstreamID: "upstream-ttl", name: "clock", visible: `{}`,
	})
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	nowNanos.Store(base.Add(59 * time.Minute).UnixNano())
	if _, err := store.Resolve(request.Route, published.Projection); err != nil {
		t.Fatalf("Resolve() before expiry error = %v", err)
	}

	nowNanos.Store(base.Add(time.Hour).UnixNano())
	_, err = store.Resolve(request.Route, published.Projection)
	assertResponsesChatReplayMissing(t, err)
	stats := store.Stats()
	if stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("stats after expiry = %+v, want empty", stats)
	}
}

func TestResponsesChatReplayEnforcesFrozenPerGroupLimitsWithTypedErrors(t *testing.T) {
	if responsesChatReplayMaxGroups != 2048 || responsesChatReplayMaxGroupBytes != 2<<20 ||
		responsesChatReplayMaxTotalBytes != 64<<20 || responsesChatReplayMaxItems != 256 ||
		responsesChatReplayMaxCalls != 128 {
		t.Fatalf("unexpected frozen replay limits: groups=%d groupBytes=%d totalBytes=%d items=%d calls=%d",
			responsesChatReplayMaxGroups, responsesChatReplayMaxGroupBytes,
			responsesChatReplayMaxTotalBytes, responsesChatReplayMaxItems, responsesChatReplayMaxCalls)
	}

	t.Run("items", func(t *testing.T) {
		store := newResponsesChatReplayStore()
		defer func() { _ = store.Close() }()
		request := newResponsesChatReplayTestRequest("limit-items", replayTestCallSpec{
			upstreamID: "upstream-limit-items", name: "limit", visible: `{}`,
		})
		for len(request.OutputItems) <= responsesChatReplayMaxItems {
			request.OutputItems = append(request.OutputItems, json.RawMessage(`{"type":"reasoning"}`))
		}
		_, err := store.Publish(request)
		assertResponsesChatReplayTooLarge(t, err, responsesChatReplayLimitItems, len(request.OutputItems), responsesChatReplayMaxItems)
	})

	t.Run("calls", func(t *testing.T) {
		store := newResponsesChatReplayStore()
		defer func() { _ = store.Close() }()
		specs := make([]replayTestCallSpec, responsesChatReplayMaxCalls+1)
		for i := range specs {
			specs[i] = replayTestCallSpec{
				upstreamID: fmt.Sprintf("upstream-limit-call-%03d", i),
				name:       fmt.Sprintf("call_%03d", i),
				visible:    `{}`,
			}
		}
		_, err := store.Publish(newResponsesChatReplayTestRequest("limit-calls", specs...))
		assertResponsesChatReplayTooLarge(t, err, responsesChatReplayLimitCalls, len(specs), responsesChatReplayMaxCalls)
	})

	t.Run("group bytes", func(t *testing.T) {
		store := newResponsesChatReplayStore()
		defer func() { _ = store.Close() }()
		request := newResponsesChatReplayTestRequest("limit-bytes", replayTestCallSpec{
			upstreamID: "upstream-limit-bytes", name: "limit", visible: `{}`,
		})
		request.AssistantContent, _ = json.Marshal(strings.Repeat("x", responsesChatReplayMaxGroupBytes))
		_, err := store.Publish(request)
		assertResponsesChatReplayTooLargeLimit(t, err, responsesChatReplayLimitGroupBytes, responsesChatReplayMaxGroupBytes)
	})

	t.Run("single group cannot exceed total", func(t *testing.T) {
		store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{
			MaxTotalBytes: 64,
		})
		defer func() { _ = store.Close() }()
		request := newResponsesChatReplayTestRequest("limit-total", replayTestCallSpec{
			upstreamID: "upstream-limit-total", name: "limit", visible: `{}`,
		})
		_, err := store.Publish(request)
		assertResponsesChatReplayTooLargeLimit(t, err, responsesChatReplayLimitTotalBytes, 64)
	})
}

func assertResponsesChatReplayTooLarge(t *testing.T, err error, limit responsesChatReplayLimit, actual, maximum int) {
	t.Helper()
	assertResponsesChatReplayTooLargeLimit(t, err, limit, maximum)
	var tooLarge *responsesChatReplayTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error type = %T, want *responsesChatReplayTooLargeError", err)
	}
	if tooLarge.Actual != actual {
		t.Fatalf("too-large Actual = %d, want %d", tooLarge.Actual, actual)
	}
}

func assertResponsesChatReplayTooLargeLimit(t *testing.T, err error, limit responsesChatReplayLimit, maximum int) {
	t.Helper()
	if !errors.Is(err, errResponsesChatReplayTooLarge) {
		t.Fatalf("error = %v, want errors.Is(too large)", err)
	}
	var tooLarge *responsesChatReplayTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error type = %T, want *responsesChatReplayTooLargeError", err)
	}
	if tooLarge.Limit != limit || tooLarge.Maximum != maximum {
		t.Fatalf("too-large error = %+v, want limit=%q maximum=%d", tooLarge, limit, maximum)
	}
	if tooLarge.Error() != responsesChatReplayTooLargeMessage || tooLarge.ReplayCode() != responsesChatReplayTooLargeCode {
		t.Fatalf("too-large error = %q code=%q", tooLarge.Error(), tooLarge.ReplayCode())
	}
}

func TestResponsesChatReplayDeterministicLRUEvictionByGroupAndTotalBytes(t *testing.T) {
	requestA := newResponsesChatReplayTestRequest("aaa", replayTestCallSpec{upstreamID: "upstream-aaa", name: "lookup", visible: `{}`})
	requestB := newResponsesChatReplayTestRequest("bbb", replayTestCallSpec{upstreamID: "upstream-bbb", name: "lookup", visible: `{}`})
	requestC := newResponsesChatReplayTestRequest("ccc", replayTestCallSpec{upstreamID: "upstream-ccc", name: "lookup", visible: `{}`})

	probe := newResponsesChatReplayStore()
	probePublished, err := probe.Publish(requestA)
	if err != nil {
		t.Fatalf("probe Publish() error = %v", err)
	}
	groupBytes := probePublished.ByteSize
	_ = probe.Close()

	tests := []struct {
		name    string
		options responsesChatReplayStoreOptions
	}{
		{
			name: "group count",
			options: responsesChatReplayStoreOptions{
				MaxGroups: 2,
			},
		},
		{
			name: "total bytes",
			options: responsesChatReplayStoreOptions{
				MaxGroups:     10,
				MaxTotalBytes: 2 * groupBytes,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newResponsesChatReplayStoreWithOptions(test.options)
			defer func() { _ = store.Close() }()

			first, err := store.Publish(requestA)
			if err != nil {
				t.Fatalf("first Publish() error = %v", err)
			}
			second, err := store.Publish(requestB)
			if err != nil {
				t.Fatalf("second Publish() error = %v", err)
			}
			if _, err := store.Resolve(requestA.Route, first.Projection); err != nil {
				t.Fatalf("touch first Resolve() error = %v", err)
			}
			third, err := store.Publish(requestC)
			if err != nil {
				t.Fatalf("third Publish() error = %v", err)
			}

			assertResponsesChatReplayMissing(t, func() error {
				_, resolveErr := store.Resolve(requestB.Route, second.Projection)
				return resolveErr
			}())
			if _, err := store.Resolve(requestA.Route, first.Projection); err != nil {
				t.Fatalf("recently used first group was evicted: %v", err)
			}
			if _, err := store.Resolve(requestC.Route, third.Projection); err != nil {
				t.Fatalf("newest third group was evicted: %v", err)
			}
			stats := store.Stats()
			if stats.Groups != 2 || stats.Calls != 2 {
				t.Fatalf("stats = %+v, want two groups/calls", stats)
			}
			if test.options.MaxTotalBytes > 0 && stats.TotalBytes > test.options.MaxTotalBytes {
				t.Fatalf("total bytes = %d, max = %d", stats.TotalBytes, test.options.MaxTotalBytes)
			}
		})
	}
}

func TestResponsesChatReplayRetriesStoredAndInFlightIDCollisions(t *testing.T) {
	block := func(value byte) []byte { return bytes.Repeat([]byte{value}, responsesChatReplayRandomBytes) }
	randomBytes := make([]byte, 0, 7*responsesChatReplayRandomBytes)
	for _, value := range []byte{1, 1, 2, 3, 3, 4} {
		randomBytes = append(randomBytes, block(value)...)
	}
	store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{
		Random: bytes.NewReader(randomBytes),
	})
	defer func() { _ = store.Close() }()

	first, err := store.Publish(newResponsesChatReplayTestRequest("collision-a", replayTestCallSpec{
		upstreamID: "upstream-collision-a", name: "lookup", visible: `{}`,
	}))
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	second, err := store.Publish(newResponsesChatReplayTestRequest("collision-b", replayTestCallSpec{
		upstreamID: "upstream-collision-b", name: "lookup", visible: `{}`,
	}))
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	third, err := store.Publish(newResponsesChatReplayTestRequest("collision-c",
		replayTestCallSpec{upstreamID: "upstream-collision-c1", name: "first", visible: `{}`},
		replayTestCallSpec{upstreamID: "upstream-collision-c2", name: "second", visible: `{}`},
	))
	if err != nil {
		t.Fatalf("third Publish() error = %v", err)
	}

	wantID := func(value byte) string {
		return responsesChatReplayCallIDPrefix + base64.RawURLEncoding.EncodeToString(block(value))
	}
	got := []string{
		first.Projection.Calls[0].ID,
		second.Projection.Calls[0].ID,
		third.Projection.Calls[0].ID,
		third.Projection.Calls[1].ID,
	}
	want := []string{wantID(1), wantID(2), wantID(3), wantID(4)}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("proxy ID %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResponsesChatReplayPublishIsAtomicWhenIDGenerationFails(t *testing.T) {
	store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{
		Random: bytes.NewReader(bytes.Repeat([]byte{7}, responsesChatReplayRandomBytes)),
	})
	defer func() { _ = store.Close() }()

	_, err := store.Publish(newResponsesChatReplayTestRequest("atomic",
		replayTestCallSpec{upstreamID: "upstream-atomic-1", name: "first", visible: `{}`},
		replayTestCallSpec{upstreamID: "upstream-atomic-2", name: "second", visible: `{}`},
	))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Publish() error = %v, want EOF", err)
	}
	stats := store.Stats()
	if stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("failed publish left state behind: %+v", stats)
	}
}

func TestResponsesChatReplayCloseClearsStateAndReturnsTypedClosedErrors(t *testing.T) {
	store := newResponsesChatReplayStore()
	request := newResponsesChatReplayTestRequest("closed", replayTestCallSpec{
		upstreamID: "upstream-closed", name: "close", visible: `{}`,
	})
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if stats := store.Stats(); !stats.Closed || stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("closed stats = %+v", stats)
	}

	_, resolveErr := store.Resolve(request.Route, published.Projection)
	assertResponsesChatReplayClosed(t, resolveErr)
	_, publishErr := store.Publish(request)
	assertResponsesChatReplayClosed(t, publishErr)
}

func assertResponsesChatReplayClosed(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errResponsesChatReplayClosed) {
		t.Fatalf("error = %v, want errors.Is(closed)", err)
	}
	var closed *responsesChatReplayClosedError
	if !errors.As(err, &closed) {
		t.Fatalf("error type = %T, want *responsesChatReplayClosedError", err)
	}
	if closed.Error() != responsesChatReplayClosedMessage || closed.ReplayCode() != responsesChatReplayClosedCode {
		t.Fatalf("closed error = %q code=%q", closed.Error(), closed.ReplayCode())
	}
}

func TestResponsesChatReplayConcurrentPublishResolveClose(t *testing.T) {
	store := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{
		MaxGroups: 32,
	})
	const (
		workers    = 12
		iterations = 100
	)
	start := make(chan struct{})
	publishedOnce := make(chan struct{}, 1)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				tag := fmt.Sprintf("race-%02d-%03d", worker, iteration)
				request := newResponsesChatReplayTestRequest(tag, replayTestCallSpec{
					upstreamID: "upstream-" + tag,
					name:       "race",
					visible:    `{}`,
				})
				published, err := store.Publish(request)
				if errors.Is(err, errResponsesChatReplayClosed) {
					return
				}
				if err != nil {
					errorsCh <- fmt.Errorf("Publish(%s): %w", tag, err)
					return
				}
				select {
				case publishedOnce <- struct{}{}:
				default:
				}
				_, err = store.Resolve(request.Route, published.Projection)
				if err != nil && !errors.Is(err, errResponsesChatReplayMissing) && !errors.Is(err, errResponsesChatReplayClosed) {
					errorsCh <- fmt.Errorf("Resolve(%s): %w", tag, err)
					return
				}
				_ = store.Stats()
			}
		}()
	}
	close(start)
	<-publishedOnce
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}

	request := newResponsesChatReplayTestRequest("race-after-close", replayTestCallSpec{
		upstreamID: "upstream-race-after-close", name: "race", visible: `{}`,
	})
	_, err := store.Publish(request)
	assertResponsesChatReplayClosed(t, err)
}

func TestResponsesChatReplayByteAccountingTracksOnlyResidentGroups(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	firstRequest := newResponsesChatReplayTestRequest("bytes-first", replayTestCallSpec{
		upstreamID: "upstream-bytes-first", name: "bytes", visible: `{}`,
	})
	secondRequest := newResponsesChatReplayTestRequest("bytes-second", replayTestCallSpec{
		upstreamID: "upstream-bytes-second", name: "bytes", visible: `{}`,
	})
	first, err := store.Publish(firstRequest)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	second, err := store.Publish(secondRequest)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	stats := store.Stats()
	if want := first.ByteSize + second.ByteSize; stats.TotalBytes != want {
		t.Fatalf("TotalBytes = %d, want %d", stats.TotalBytes, want)
	}
	if _, err := store.Resolve(firstRequest.Route, first.Projection); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if afterResolve := store.Stats(); afterResolve.TotalBytes != stats.TotalBytes {
		t.Fatalf("Resolve changed byte accounting: before=%d after=%d", stats.TotalBytes, afterResolve.TotalBytes)
	}
}

func TestResponsesChatReplayErrorsNeverContainRawReplayContent(t *testing.T) {
	const marker = "RAW_REPLAY_CONTENT_MUST_NOT_APPEAR"

	tooSmall := newResponsesChatReplayStoreWithOptions(responsesChatReplayStoreOptions{MaxGroupBytes: 64})
	request := newResponsesChatReplayTestRequest("redaction", replayTestCallSpec{
		upstreamID: "upstream-redaction", name: "redaction", visible: `{"marker":"` + marker + `"}`,
	})
	_, err := tooSmall.Publish(request)
	_ = tooSmall.Close()
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("too-large error leaked raw content: %v", err)
	}

	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	changed := cloneReplayTestProjection(published.Projection)
	changed.Calls[0].Arguments = marker
	_, err = store.Resolve(request.Route, changed)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("projection error leaked raw content: %v", err)
	}
}

func TestResponsesChatReplayPublishesOnlyStructurallyCompleteGroups(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()

	request := newResponsesChatReplayTestRequest("incomplete", replayTestCallSpec{
		upstreamID: "upstream-incomplete", name: "expected", visible: `{}`,
	})
	request.Calls[0].Name = "different"
	_, err := store.Publish(request)
	assertResponsesChatReplayProjection(t, err)

	reordered := newResponsesChatReplayTestRequest("reordered-publish",
		replayTestCallSpec{upstreamID: "upstream-reordered-1", name: "first", visible: `{}`},
		replayTestCallSpec{upstreamID: "upstream-reordered-2", name: "second", visible: `{}`},
	)
	reordered.Calls[0], reordered.Calls[1] = reordered.Calls[1], reordered.Calls[0]
	_, err = store.Publish(reordered)
	assertResponsesChatReplayProjection(t, err)

	if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("invalid group was partially published: %+v", stats)
	}
}
