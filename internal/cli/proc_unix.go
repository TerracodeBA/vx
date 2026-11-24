//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs c in a process group of its own, so stopping it
// (the effort moved on while it ran, or vx was interrupted) stops the
// commands it started too, such as the test binaries of go test.
func ownProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}
