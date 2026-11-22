package proxy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const optimizerProcessWaitDelay = 100 * time.Millisecond

type optimizerProcessController interface {
	afterStart() error
	terminate() error
	close() error
}

func runOptimizerCommand(ctx context.Context, cmd *exec.Cmd) (returnErr error) {
	if cmd == nil {
		return fmt.Errorf("optimizer command is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := optimizerContextErr(ctx); err != nil {
		return err
	}

	controller, err := newOptimizerProcessController(cmd)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := controller.close()
		if returnErr == nil && optimizerContextErr(ctx) == nil {
			returnErr = closeErr
		}
	}()

	cmd.WaitDelay = optimizerProcessWaitDelay
	cmd.Cancel = controller.terminate
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := controller.afterStart(); err != nil {
		terminateErr := controller.terminate()
		if terminateErr != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		if ctxErr := optimizerContextErr(ctx); ctxErr != nil {
			return ctxErr
		}
		return errors.Join(err, terminateErr, waitErr)
	}

	waitFinished := make(chan struct{})
	cancelWatchDone := make(chan struct{})
	go func() {
		defer close(cancelWatchDone)
		select {
		case <-ctx.Done():
			_ = controller.terminate()
		case <-waitFinished:
		}
	}()

	waitErr := cmd.Wait()
	close(waitFinished)
	<-cancelWatchDone
	terminateErr := controller.terminate()
	if ctxErr := optimizerContextErr(ctx); ctxErr != nil {
		return ctxErr
	}
	return errors.Join(waitErr, terminateErr)
}

func optimizerContextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
