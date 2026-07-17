package launch

import (
	"errors"
	"os"
)

type processController interface {
	afterStart() error
	wait() commandOutcome
	signal(os.Signal) error
	kill() error
	close() error
}

func waitAndCloseController(controller processController) commandOutcome {
	outcome := controller.wait()
	outcome.err = errors.Join(outcome.err, controller.close())
	return outcome
}
