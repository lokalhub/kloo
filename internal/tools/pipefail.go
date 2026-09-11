package tools

import (
	"os/exec"
	"sync"
)

// The exit status of a POSIX pipeline is the status of its LAST command, so
// `go test ./... | head -50` reports SUCCESS however badly the tests fail.
// Models pipe into head/tail constantly to keep output small, and the harness
// then told them their failing suite had passed — in one observed run the model
// spent four steps hunting a "stale build cache" to explain why a test it had
// been told passed was still failing, then tripped the churn rail.
//
// `set -o pipefail` makes the pipeline report the first failing stage. It is not
// in POSIX, but dash (the usual /bin/sh here), bash, ash, ksh and zsh all
// support it. We probe ONCE rather than assume: on a shell without it we run the
// command exactly as before, so a missing feature degrades to the old behaviour
// instead of breaking every command with a syntax error.
var pipefailOnce = sync.OnceValue(func() bool {
	return exec.Command("sh", "-c", "set -o pipefail").Run() == nil
})

// shellCommand wraps a model-supplied command so a piped failure is reported as
// a failure, where the shell can do that.
func shellCommand(command string) string {
	if pipefailOnce() {
		return "set -o pipefail\n" + command
	}
	return command
}
