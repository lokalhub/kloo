//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the command in its own process group so the whole child
// tree (a shell plus anything it spawns) can be killed together on timeout.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup SIGKILLs the command's process group (negative PID), reaping
// children a plain CommandContext kill would leave behind.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
