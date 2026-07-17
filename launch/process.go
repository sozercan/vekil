package launch

import "os"

type processController interface {
	afterStart() error
	wait() commandOutcome
	signal(os.Signal) error
	kill() error
	close() error
}
