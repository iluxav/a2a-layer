//go:build !unix

package runner

import "os/exec"

// setProcessGroup: without process groups, canceling kills the CLI process itself.
func setProcessGroup(cmd *exec.Cmd) {}
