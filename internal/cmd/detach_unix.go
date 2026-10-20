//go:build unix

package cmd

import (
	"os/exec"
	"syscall"
)

// newSession starts c as the leader of a session of its own, with no
// controlling terminal, so a hangup or an interrupt aimed at the launcher's
// terminal or process group never reaches it.
func newSession(c *exec.Cmd) error {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
