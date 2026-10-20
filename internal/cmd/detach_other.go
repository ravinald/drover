//go:build !unix

package cmd

import (
	"errors"
	"os/exec"
)

func newSession(*exec.Cmd) error {
	return errors.New("--detach needs a unix session (setsid), which this platform does not have")
}
