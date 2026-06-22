//go:build !unix

package tools

import "os/exec"

// setProcGroup is a no-op on non-unix platforms; exec.CommandContext still kills
// the launched process on timeout (best-effort child handling).
func setProcGroup(cmd *exec.Cmd) {}

// killProcGroup falls back to killing the single process.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
