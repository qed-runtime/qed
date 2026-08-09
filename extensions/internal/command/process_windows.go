//go:build windows

package command

import "os/exec"

func prepareProcess(*exec.Cmd) {}

func terminateProcess(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}
