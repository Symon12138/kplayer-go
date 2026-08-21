//go:build windows

package provider

import (
	"os"
	"syscall"
)

const _stillActive = 259 // Windows STILL_ACTIVE exit code

// killProcess force-kills the process (TerminateProcess on Windows).
func killProcess(p *os.Process) error {
	return p.Kill()
}

// processAlive reports whether the process is still running.
// Windows cannot probe with signal 0, so the exit code is queried via
// OpenProcess + GetExitCodeProcess instead.
func processAlive(p *os.Process) bool {
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(p.Pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	// 259 (STILL_ACTIVE) means the process has not exited yet.
	return exitCode == _stillActive
}
