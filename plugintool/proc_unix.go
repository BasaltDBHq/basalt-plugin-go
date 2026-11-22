//go:build unix

package plugintool

import (
	"os"
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a group of its own, so killGroup stops it and
// anything it started.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) != nil {
		_ = cmd.Process.Kill()
	}
}

func isRoot() bool { return os.Geteuid() == 0 }
