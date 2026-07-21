package proxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type serializedPolicyRoutingController struct {
	calls         atomic.Int32
	firstStarted  chan struct{}
	releaseFirst  chan struct{}
	secondStarted chan struct{}
	releaseSecond chan struct{}
	secondErr     error
}

func (c *serializedPolicyRoutingController) Plan(context.Context, chatPolicyInput) (chatOperationPlan, error) {
	return chatOperationPlan{}, nil
}

func (*serializedPolicyRoutingController) Active() bool { return true }

func (c *serializedPolicyRoutingController) Initialize(context.Context) error {
	switch c.calls.Add(1) {
	case 1:
		close(c.firstStarted)
		<-c.releaseFirst
		return nil
	case 2:
		close(c.secondStarted)
		<-c.releaseSecond
		return c.secondErr
	default:
		return errors.New("unexpected initialization attempt")
	}
}

func (*serializedPolicyRoutingController) ReadinessDiagnostic() string { return "" }

func TestInitializePolicyRoutingSerializesAttemptsAndKeepsGateClosedOnLatestFailure(t *testing.T) {
	secondErr := errors.New("second preflight failed")
	controller := &serializedPolicyRoutingController{
		firstStarted:  make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
		secondErr:     secondErr,
	}
	h := &ProxyHandler{policyRoutingController: controller}
	h.policyPreflightPending.Store(true)

	firstDone := make(chan error, 1)
	go func() { firstDone <- h.InitializePolicyRouting(t.Context()) }()
	select {
	case <-controller.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first initialization did not start")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- h.InitializePolicyRouting(t.Context()) }()
	select {
	case <-controller.secondStarted:
		t.Fatal("second initialization started before the first completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(controller.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first InitializePolicyRouting() error = %v", err)
	}
	select {
	case <-controller.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second initialization did not start after the first completed")
	}
	if !h.PolicyRoutingPreflightPending() {
		t.Fatal("preflight gate opened while the second initialization was in flight")
	}

	close(controller.releaseSecond)
	if err := <-secondDone; !errors.Is(err, secondErr) {
		t.Fatalf("second InitializePolicyRouting() error = %v, want %v", err, secondErr)
	}
	if !h.PolicyRoutingPreflightPending() {
		t.Fatal("preflight gate opened after the latest initialization failed")
	}
}

func TestInitializePolicyRoutingWaitingAttemptHonorsCancellation(t *testing.T) {
	controller := &serializedPolicyRoutingController{
		firstStarted:  make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
	h := &ProxyHandler{policyRoutingController: controller}
	h.policyPreflightPending.Store(true)

	firstDone := make(chan error, 1)
	go func() { firstDone <- h.InitializePolicyRouting(t.Context()) }()
	select {
	case <-controller.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first initialization did not start")
	}

	waitCtx, cancel := context.WithCancel(t.Context())
	secondDone := make(chan error, 1)
	go func() { secondDone <- h.InitializePolicyRouting(waitCtx) }()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting InitializePolicyRouting() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiting initialization did not return promptly")
	}
	select {
	case <-controller.secondStarted:
		t.Fatal("canceled waiting initialization reached the controller")
	default:
	}

	close(controller.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first InitializePolicyRouting() error = %v", err)
	}
	if h.PolicyRoutingPreflightPending() {
		t.Fatal("successful active initialization did not clear the gate after canceled waiter exited")
	}
}
