//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package proxy

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

type openAICodexProcessLock struct{ file *os.File }

func openOpenAICodexWritableFile(string) (*os.File, error) {
	return nil, fmt.Errorf("OpenAI Codex token refresh is unsupported on %s; run `codex login` to refresh credentials", runtime.GOOS)
}

func acquireOpenAICodexProcessLock(context.Context, string) (*openAICodexProcessLock, error) {
	return nil, fmt.Errorf("OpenAI Codex token refresh is unsupported on %s; run `codex login` to refresh credentials", runtime.GOOS)
}

func (*openAICodexProcessLock) release() error              { return nil }
func alignOpenAICodexFileOwner(*os.File, os.FileInfo) error { return nil }
func openAICodexRunningAsRoot() bool                        { return false }
