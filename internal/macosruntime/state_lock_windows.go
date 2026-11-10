//go:build windows

package macosruntime

import (
	"context"
	"errors"
)

type HelperStateLock struct{}

func AcquireHelperStateLock(context.Context, Paths) (*HelperStateLock, error) {
	return nil, errors.New("the macOS runtime state lock is unavailable on Windows")
}

func (l *HelperStateLock) Close() error { return nil }
