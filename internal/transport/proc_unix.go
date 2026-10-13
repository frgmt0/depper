//go:build unix

package transport

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a new process group, so terminal signals
// (Ctrl-C) reach only depper, which then cancels the command deliberately.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
