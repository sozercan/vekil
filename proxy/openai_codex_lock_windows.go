//go:build windows

package proxy

import (
	"context"
	"os"
)

type openAICodexProcessLock struct{ file *os.File }

func openOpenAICodexWritableFile(string) (*os.File, error) {
	return nil, openAICodexWindowsRefreshError()
}

func acquireOpenAICodexProcessLock(context.Context, string) (*openAICodexProcessLock, error) {
	return nil, openAICodexWindowsRefreshError()
}

func (*openAICodexProcessLock) release() error              { return nil }
func alignOpenAICodexFileOwner(*os.File, os.FileInfo) error { return nil }
func openAICodexRunningAsRoot() bool                        { return false }
