//go:build !windows

package system

import (
	"os"
	"os/exec"
	"syscall"
)

// detachUnix starts a process in a session of its own, so it outlives the terminal.
func detachUnix(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// ScheduleRemoval deletes the program; on macOS and Linux a running program can be removed.
func ScheduleRemoval(path string) error { return os.Remove(path) }

// Restart replaces this process with the program at exe, running args; it returns only if
// that fails.
func Restart(exe string, args []string) error {
	return syscall.Exec(exe, append([]string{exe}, args...), append(os.Environ(), "ARCHIVIST_SYNC_RESTART=1"))
}
