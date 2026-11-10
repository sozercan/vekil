package macosruntime

import (
	"context"
	"time"
)

func monitorParent(ctx context.Context, parentPID int, interval time.Duration, onExit func()) {
	if parentPID <= 0 || onExit == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !parentProcessAlive(parentPID) {
				onExit()
				return
			}
		}
	}
}
