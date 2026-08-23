//go:build windows

package macosruntime

// The native helper is a macOS product. Stdin EOF and process signals remain
// authoritative on Windows test builds where kill(pid, 0) has no equivalent.
func parentProcessAlive(int) bool { return true }
