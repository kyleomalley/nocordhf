//go:build unix

package main

import (
	"os"
	"syscall"
)

// redirectStderr points FD 2 (and the Go runtime's panic destination) at
// the given file. The Go runtime writes panic stacks to FD 2 directly via
// syscall.Write, bypassing os.Stderr, so we have to dup at the FD level
// rather than just reassigning os.Stderr. Best-effort — failure means we
// just don't capture panics, no impact on normal execution.
func redirectStderr(f *os.File) error {
	return syscall.Dup2(int(f.Fd()), int(os.Stderr.Fd()))
}
