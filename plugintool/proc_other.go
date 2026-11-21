//go:build !unix

package plugintool

import "os/exec"

func ownProcessGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func isRoot() bool { return false }
