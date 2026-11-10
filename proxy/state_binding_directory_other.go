//go:build !linux && !darwin

package proxy

func createPrivateStateDirectory(string) error { return errDurableStatePlatform }
