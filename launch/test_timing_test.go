package launch

import "time"

func launcherTestTimeout(base time.Duration) time.Duration {
	if raceDetectorEnabled {
		return 4 * base
	}
	return base
}
