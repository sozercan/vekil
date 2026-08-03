package macosruntime

import (
	"context"
	"sync"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
)

type stateProjector struct {
	mu       sync.Mutex
	revision uint64
	last     runtimeStatePayload
}

func (p *stateProjector) publishState(h *helper) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	revision, event := p.nextLocked(h)
	return h.writer.SendState(revision, event)
}

// publishCriticalPair keeps revision allocation and writer admission in one
// critical section. A newer replaceable state therefore cannot be admitted
// before the correlated event and its authoritative state snapshot.
func (p *stateProjector) publishCriticalPair(
	ctx context.Context,
	h *helper,
	buildEvent func(eventRevision uint64) eventEnvelope,
) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	eventRevision, stateRevision, state := p.criticalPairLocked(h)
	return h.writer.SendCriticalWithState(ctx, buildEvent(eventRevision), stateRevision, state)
}

func (p *stateProjector) nextLocked(h *helper) (uint64, eventEnvelope) {
	p.revision++
	p.last = h.buildStatePayload()
	return p.revision, eventEnvelope{
		Version: ProtocolMax, Event: "state", HelperEpoch: h.epoch,
		StateRevision: p.revision, Payload: p.last,
	}
}

func (p *stateProjector) criticalPairLocked(h *helper) (uint64, uint64, eventEnvelope) {
	p.revision++
	eventRevision := p.revision
	p.revision++
	stateRevision := p.revision
	p.last = h.buildStatePayload()
	return eventRevision, stateRevision, eventEnvelope{
		Version: ProtocolMax, Event: "state", HelperEpoch: h.epoch,
		StateRevision: stateRevision, Payload: p.last,
	}
}

func (p *stateProjector) current(h *helper) (uint64, runtimeStatePayload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.revision == 0 {
		p.revision = 1
		p.last = h.buildStatePayload()
	}
	return p.revision, p.last
}

func (h *helper) buildStatePayload() runtimeStatePayload {
	state := h.opts.Controller.Snapshot()
	description, _ := h.opts.Configuration.Describe()
	payload := runtimeStatePayload{
		Helper:            "connected",
		Service:           state.Service,
		Readiness:         state.Readiness,
		Auth:              state.Auth,
		RuntimeGeneration: state.RuntimeGeneration,
		ConfigRevision:    state.ConfigRevision,
		SecretGeneration:  state.SecretGeneration,
		Addr:              state.Addr,
		LastFailureCode:   state.LastFailureCode,
		Configuration:     description,
	}

	requiresCopilot := state.Auth != appcontrol.AuthNotRequired
	if state.Service != appcontrol.ServiceStarting && state.Service != appcontrol.ServiceRunning {
		requiresCopilot = descriptionRequiresCopilot(description)
	}
	if !requiresCopilot {
		payload.Auth = appcontrol.AuthNotRequired
	} else if h.opts.Authenticator != nil && state.Service != appcontrol.ServiceStarting {
		status := h.opts.Authenticator.Status()
		payload.AuthSource = protocolAuthSource(status.Source)
		if state.Auth == appcontrol.AuthFailed {
			payload.Auth = appcontrol.AuthFailed
		} else if status.SignedIn {
			payload.Auth = appcontrol.AuthSignedIn
		} else {
			payload.Auth = appcontrol.AuthSignedOut
		}
	}
	active := h.operations.activeSnapshot()
	if active != nil && !active.controller {
		payload.Operation = &runtimeOperationState{ID: active.id, Kind: active.kind}
	} else if state.Operation != nil {
		payload.Operation = &runtimeOperationState{ID: state.Operation.ID, Kind: string(state.Operation.Kind), Phase: string(state.Operation.Phase)}
	} else if active != nil {
		payload.Operation = &runtimeOperationState{ID: active.id, Kind: active.kind}
	}
	return payload
}

func descriptionRequiresCopilot(description ConfigDescription) bool {
	if description.Mode == ConfigModeLegacy || !description.Available {
		return true
	}
	for _, provider := range description.Providers {
		if provider.Type == "copilot" {
			return true
		}
	}
	return false
}

func protocolAuthSource(source auth.AuthSource) string {
	switch source {
	case auth.AuthSourceEnv:
		return "environment"
	case auth.AuthSourceVekil:
		return "vekil"
	case auth.AuthSourceGitHubCLI:
		return "github_cli"
	default:
		return "none"
	}
}
