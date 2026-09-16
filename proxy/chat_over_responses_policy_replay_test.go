package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func policyReplayUnitRoutes() (responsesChatReplayRoute, responsesChatReplayRoute) {
	lightweight := responsesChatReplayRoute{
		ProviderID: "provider-a", PublicModel: "semantic", UpstreamModel: "deployment-a",
		RouteID: "low-route", PolicyTier: policyTierLightweight.String(),
	}
	powerful := lightweight
	powerful.RouteID = "max-route"
	powerful.PolicyTier = policyTierPowerful.String()
	return lightweight, powerful
}

func publishPolicyReplayUnitTurn(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute, tag string, count int) responsesChatReplayPublished {
	t.Helper()
	specs := make([]replayTestCallSpec, count)
	for index := range specs {
		specs[index] = replayTestCallSpec{
			upstreamID: fmt.Sprintf("upstream-%s-%d", tag, index),
			name:       "lookup",
			visible:    fmt.Sprintf(`{"index":%d}`, index),
		}
	}
	request := newResponsesChatReplayTestRequest(tag, specs...)
	request.Route = route
	published, err := store.Publish(request)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return published
}

func policyReplayUnitResult(published responsesChatReplayPublished, index int) any {
	return map[string]any{
		"role": "tool", "tool_call_id": published.Projection.Calls[index].ID,
		"content": fmt.Sprintf("result %d", index),
	}
}

func policyReplayUnitMessages(published responsesChatReplayPublished, results ...int) []any {
	calls := make([]any, len(published.Projection.Calls))
	for index, call := range published.Projection.Calls {
		calls[index] = map[string]any{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		}
	}
	messages := []any{
		map[string]any{"role": "user", "content": "Look up the config."},
		map[string]any{"role": "assistant", "content": published.Projection.Content, "tool_calls": calls},
	}
	for _, index := range results {
		messages = append(messages, policyReplayUnitResult(published, index))
	}
	return messages
}

func policyReplayUnitBody(t *testing.T, messages []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "semantic", "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestChatOverResponsesPolicyReplayRequiresCompletedTurnBeforeSwitch(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	lightweight, powerful := policyReplayUnitRoutes()
	published := publishPolicyReplayUnitTurn(t, store, powerful, "boundary", 2)
	finalReply := map[string]any{"role": "assistant", "content": "The config is valid."}
	nextUser := map[string]any{"role": "user", "content": "Hello."}
	tests := []struct {
		name       string
		results    []int
		tail       []any
		wantSwitch bool
	}{
		{name: "new user after completed turn", results: []int{0, 1}, tail: []any{finalReply, nextUser}, wantSwitch: true},
		{name: "reversed complete results", results: []int{1, 0}, tail: []any{finalReply, nextUser}, wantSwitch: true},
		{name: "text block user", results: []int{0, 1}, tail: []any{finalReply,
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Hello."}}}}, wantSwitch: true},
		{name: "complete results still need assistant reply", results: []int{0, 1}},
		{name: "assistant reply still needs new user", results: []int{0, 1}, tail: []any{finalReply}},
		{name: "new user without assistant reply", results: []int{0, 1}, tail: []any{nextUser}},
		{name: "blank user does not finish continuation", results: []int{0, 1}, tail: []any{finalReply,
			map[string]any{"role": "user", "content": " \t"}}},
		{name: "partial results remain pinned", results: []int{1}, tail: []any{finalReply, nextUser}},
		{name: "late result remains pinned", results: []int{0}, tail: []any{finalReply, nextUser, policyReplayUnitResult(published, 1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := append(policyReplayUnitMessages(published, test.results...), test.tail...)
			body := policyReplayUnitBody(t, messages)
			if active := chatRequestHasActiveResponsesPolicyReplay(body); active == test.wantSwitch {
				t.Fatalf("active replay = %v, want %v", active, !test.wantSwitch)
			}
			options := responsesChatRequestOptions{
				ReplayStore: store, ReplayRoute: lightweight,
				CompletedPolicyReplayRoutes: []responsesChatReplayRoute{lightweight, powerful},
			}
			plan, err := translateChatRequestToResponses(body, options)
			if test.wantSwitch {
				if err != nil {
					t.Fatalf("completed turn could not change effort: %v", err)
				}
				if !bytes.Contains(plan.Body, []byte(`"id":"reasoning_boundary"`)) {
					t.Fatalf("completed replay lost hidden reasoning: %s", plan.Body)
				}
			} else if !isMissingResponsesChatReplayError(err) {
				t.Fatalf("wrong-tier continuation error = %v, want replay state missing", err)
			}
			options.ReplayRoute = powerful
			if _, err := translateChatRequestToResponses(body, options); err != nil {
				t.Fatalf("original tier could not continue: %v", err)
			}
		})
	}
}

func TestChatOverResponsesPolicyReplayMixedHistoryKeepsActiveTier(t *testing.T) {
	for _, sharedRoute := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared_route_%v", sharedRoute), func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			lightweight, powerful := policyReplayUnitRoutes()
			if sharedRoute {
				powerful.RouteID = lightweight.RouteID
			}
			history := publishPolicyReplayUnitTurn(t, store, lightweight, "history-low", 1)
			active := publishPolicyReplayUnitTurn(t, store, powerful, "active-max", 1)
			messages := append(policyReplayUnitMessages(history, 0),
				map[string]any{"role": "assistant", "content": "Done."},
				map[string]any{"role": "user", "content": "Investigate the harder problem."},
			)
			messages = append(messages, policyReplayUnitMessages(active, 0)[1:]...)
			body := policyReplayUnitBody(t, messages)
			options := responsesChatRequestOptions{
				ReplayStore: store, ReplayRoute: powerful,
				CompletedPolicyReplayRoutes: []responsesChatReplayRoute{lightweight, powerful},
			}
			if !chatRequestHasActiveResponsesPolicyReplay(body) {
				t.Fatal("current tool result did not retain the active replay binding")
			}
			if _, err := translateChatRequestToResponses(body, options); err != nil {
				t.Fatalf("active max tier could not restore older low history: %v", err)
			}
			options.ReplayRoute = lightweight
			if _, err := translateChatRequestToResponses(body, options); !isMissingResponsesChatReplayError(err) {
				t.Fatalf("active max continuation accepted low tier: %v", err)
			}
			messages = append(messages,
				map[string]any{"role": "assistant", "content": "The investigation is complete."},
				map[string]any{"role": "user", "content": "Thanks."},
			)
			body = policyReplayUnitBody(t, messages)
			if chatRequestHasActiveResponsesPolicyReplay(body) {
				t.Fatal("completed history retained an active replay binding")
			}
			plan, err := translateChatRequestToResponses(body, options)
			if err != nil {
				t.Fatalf("completed low/max history could not return to low: %v", err)
			}
			for _, tag := range []string{"history-low", "active-max"} {
				if !bytes.Contains(plan.Body, []byte(`"id":"reasoning_`+tag+`"`)) {
					t.Fatalf("history lost reasoning for %s: %s", tag, plan.Body)
				}
			}
		})
	}
}

func TestChatOverResponsesPolicyReplayInterruptedTurnKeepsCompletedHistory(t *testing.T) {
	tests := []struct {
		name        string
		results     []int
		sharedRoute bool
	}{
		{name: "partial results", results: []int{1}},
		{name: "complete results without final reply", results: []int{0, 1}},
		{name: "partial results on shared route", results: []int{1}, sharedRoute: true},
		{name: "complete results on shared route", results: []int{0, 1}, sharedRoute: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			lightweight, powerful := policyReplayUnitRoutes()
			if test.sharedRoute {
				powerful.RouteID = lightweight.RouteID
			}
			history := publishPolicyReplayUnitTurn(t, store, lightweight, "interrupted-history-low", 1)
			active := publishPolicyReplayUnitTurn(t, store, powerful, "interrupted-active-max", 2)
			messages := append(policyReplayUnitMessages(history, 0),
				map[string]any{"role": "assistant", "content": "The config is valid."},
				map[string]any{"role": "user", "content": "Investigate the harder problem."},
			)
			messages = append(messages, policyReplayUnitMessages(active, test.results...)[1:]...)
			messages = append(messages, map[string]any{"role": "user", "content": "Also inspect the lock."})
			body := policyReplayUnitBody(t, messages)
			if !chatRequestHasActiveResponsesPolicyReplay(body) {
				t.Fatal("user interruption released the unfinished max turn")
			}
			options := responsesChatRequestOptions{
				ReplayStore: store, ReplayRoute: powerful,
				CompletedPolicyReplayRoutes: []responsesChatReplayRoute{lightweight, powerful},
			}
			plan, err := translateChatRequestToResponses(body, options)
			if err != nil {
				t.Fatalf("interrupted max turn could not restore completed low history: %v", err)
			}
			if !bytes.Contains(plan.Body, []byte(`"id":"reasoning_interrupted-history-low"`)) {
				t.Fatalf("interruption discarded completed low reasoning: %s", plan.Body)
			}
			options.ReplayRoute = lightweight
			if _, err := translateChatRequestToResponses(body, options); !isMissingResponsesChatReplayError(err) {
				t.Fatalf("interrupted max turn accepted low tier: %v", err)
			}
		})
	}
}

func TestChatOverResponsesPolicyReplayRequiresExactAllowedUpstream(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	lightweight, powerful := policyReplayUnitRoutes()
	published := publishPolicyReplayUnitTurn(t, store, powerful, "affinity", 1)
	body := policyReplayUnitBody(t, append(policyReplayUnitMessages(published, 0),
		map[string]any{"role": "assistant", "content": "Done."},
		map[string]any{"role": "user", "content": "Hello."},
	))
	tests := []struct {
		name   string
		mutate func(*responsesChatRequestOptions)
	}{
		{name: "no policy allowance", mutate: func(options *responsesChatRequestOptions) { options.CompletedPolicyReplayRoutes = nil }},
		{name: "different provider", mutate: func(options *responsesChatRequestOptions) { options.ReplayRoute.ProviderID = "provider-b" }},
		{name: "different public model", mutate: func(options *responsesChatRequestOptions) { options.ReplayRoute.PublicModel = "another-policy" }},
		{name: "different upstream model", mutate: func(options *responsesChatRequestOptions) { options.ReplayRoute.UpstreamModel = "deployment-b" }},
		{name: "wrong allowed route", mutate: func(options *responsesChatRequestOptions) {
			options.CompletedPolicyReplayRoutes[1].RouteID = "other-route"
		}},
		{name: "wrong allowed tier", mutate: func(options *responsesChatRequestOptions) {
			options.CompletedPolicyReplayRoutes[1].PolicyTier = policyTierLightweight.String()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := responsesChatRequestOptions{
				ReplayStore: store, ReplayRoute: lightweight,
				CompletedPolicyReplayRoutes: []responsesChatReplayRoute{lightweight, powerful},
			}
			test.mutate(&options)
			if _, err := translateChatRequestToResponses(body, options); !isMissingResponsesChatReplayError(err) {
				t.Fatalf("disallowed replay error = %v, want replay state missing", err)
			}
		})
	}
	if _, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		ReplayStore: store, ReplayRoute: lightweight,
		CompletedPolicyReplayRoutes: []responsesChatReplayRoute{lightweight, powerful},
	}); err != nil {
		t.Fatalf("valid allowance failed after rejected probes: %v", err)
	}
}

func TestChatOverResponsesPolicyReplayPlanCopiesAllowlist(t *testing.T) {
	lightweight, powerful := policyReplayUnitRoutes()
	want := []responsesChatReplayRoute{lightweight, powerful}
	allowed := slices.Clone(want)
	route := &modelRoute{
		public:  publicModelContract{id: "internal", routeID: lightweight.RouteID, endpoints: []string{providerEndpointResponses}},
		targets: []targetBinding{{id: "target-a", upstreamModel: lightweight.UpstreamModel}},
		policy:  routePolicy{mode: routeModePrimaryOnly, maxTargetAttempts: 1, maxUpstreamSends: 1},
	}
	plan := newChatOperationPlan(chatOperationPlanOptions{
		PublicID: lightweight.PublicModel, Route: route,
		Contract:                    publicModelContract{id: lightweight.PublicModel, endpoints: []string{providerEndpointChatCompletions}},
		CompletedPolicyReplayRoutes: allowed,
	})
	allowed[0].ProviderID = "changed constructor input"
	if got := plan.completedPolicyReplayRouteSnapshot(); !slices.Equal(got, want) {
		t.Fatalf("constructor did not copy allowed routes: %+v", got)
	}
	snapshot := plan.completedPolicyReplayRouteSnapshot()
	snapshot[0].UpstreamModel = "changed snapshot"
	if got := plan.completedPolicyReplayRouteSnapshot(); !slices.Equal(got, want) {
		t.Fatalf("snapshot changed the plan: %+v", got)
	}
	operation := newRouteOperationFromChatPlan(plan, t.Context())
	if operation == nil {
		t.Fatal("plan did not produce an operation")
	}
	plan.completedPolicyReplayRoutes[0].RouteID = "changed admitted plan"
	sealed, ok := operation.policyPlan()
	if !ok || !slices.Equal(sealed.completedPolicyReplayRoutes, want) {
		t.Fatalf("operation did not seal allowed routes: %+v", sealed.completedPolicyReplayRoutes)
	}
	sealed.completedPolicyReplayRoutes[1].PolicyTier = "changed returned plan"
	again, ok := operation.policyPlan()
	if !ok || !slices.Equal(again.completedPolicyReplayRoutes, want) {
		t.Fatalf("returned plan changed the operation: %+v", again.completedPolicyReplayRoutes)
	}
}
