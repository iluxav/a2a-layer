//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the CLI in its own process group, so canceling a task stops the CLI
// and everything it started (MCP clients, helpers), not just the top process.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
}
