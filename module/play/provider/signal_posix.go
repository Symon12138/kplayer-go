//go:build !windows

package provider

import (
	"os"
	"syscall"
)

// killProcess force-kills the process (SIGKILL on POSIX platforms).
func killProcess(p *os.Process) error {
	return p.Kill()
}

// processAlive reports whether the process is still running
// (signal 0 probe — no signal is actually delivered).
func processAlive(p *os.Process) bool {
	return p.Signal(syscall.Signal(0)) == nil
}
