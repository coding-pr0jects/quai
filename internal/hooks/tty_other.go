//go:build !linux && !darwin

package hooks

// isTerminal cannot detect a terminal on this platform, so stdin is always read.
func isTerminal(uintptr) bool { return false }
