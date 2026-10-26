//go:build !unix

package runner

import (
	"os"
	"os/exec"
	"syscall"
)

// Process groups are a unix concept. Elsewhere a cancelled launch reaches the
// child alone, which leaves the orphan problem setGroup exists to solve: said
// here rather than discovered, because the platform that cannot fix it is the
// one nobody tests on.
func setGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}

func exitSignal(*os.ProcessState) int { return 0 }

func leftRunning(int) []string { return nil }

// processAlive is FindProcess, which opens a handle on this platform and fails
// for a pid that is gone.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
