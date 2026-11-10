package macosruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/sozercan/vekil/internal/appcontrol"
)

func TestOperationCoordinatorWaitsForCleanedControllerOperation(t *testing.T) {
	var coordinator operationCoordinator
	first, err := coordinator.beginController(
		t.Context(),
		"stop",
		nil,
		func() (appcontrol.Operation, error) {
			return appcontrol.Operation{ID: "op_stop", Kind: appcontrol.OperationStop}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := coordinator.finish(first.ID)
	if cleaned == nil {
		t.Fatal("finished controller operation was not retained through terminal delivery")
	}

	waitingContext, cancel := context.WithCancel(t.Context())
	cancel()
	beginCalled := false
	_, err = coordinator.beginController(
		waitingContext,
		"start",
		nil,
		func() (appcontrol.Operation, error) {
			beginCalled = true
			return appcontrol.Operation{ID: "op_start", Kind: appcontrol.OperationStart}, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("begin during terminal delivery error = %v, want context cancellation", err)
	}
	if beginCalled {
		t.Fatal("next controller operation began before terminal delivery completed")
	}

	coordinator.completeOperation(cleaned)
	second, err := coordinator.beginController(
		t.Context(),
		"start",
		nil,
		func() (appcontrol.Operation, error) {
			return appcontrol.Operation{ID: "op_start", Kind: appcontrol.OperationStart}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != "op_start" {
		t.Fatalf("second operation ID = %q, want op_start", second.ID)
	}
}

func TestOperationCoordinatorStillRejectsOverlappingActiveOperation(t *testing.T) {
	var coordinator operationCoordinator
	if _, err := coordinator.beginAsync(t.Context(), "first", false); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.beginAsync(t.Context(), "second", false); !errors.Is(err, appcontrol.ErrOperationInProgress) {
		t.Fatalf("overlapping operation error = %v, want operation in progress", err)
	}
}
