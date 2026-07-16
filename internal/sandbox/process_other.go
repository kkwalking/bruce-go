//go:build !darwin && !linux

package sandbox

import "os/exec"

func configureProcess(*exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
