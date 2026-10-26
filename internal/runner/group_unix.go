//go:build unix

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// setGroup puts the child in its own process group.
//
// Without it a cancelled or timed-out launch kills the agent and leaves
// whatever the agent started: a dev server it brought up to test its own work
// holds the inherited stdout, so the pipe never reaches EOF and the caller waits
// on a child that is already dead. The group is what makes the whole tree
// killable as one.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the child's whole process group, falling back to the child
// alone when the group is not addressable. Negating the pid is how a signal
// reaches every descendant rather than the one process drover started.
func killGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}

// exitSignal is the signal that ended the child, zero when it exited on its
// own. ExitCode reports -1 for both a signal and no child at all, and a kill
// from the host's memory pressure is a different report from either.
func exitSignal(ps *os.ProcessState) int {
	if ps == nil {
		return 0
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0
	}
	return int(ws.Signal())
}

// leftRunning names what is still alive in the child's process group once the
// child is gone, and kills it.
//
// The group is the session's own (setGroup), so anything still in it is
// something the session started: a gate it backgrounded, a server it brought
// up. A session that exits while that still runs reports on work that has not
// finished, and the process keeps writing to a worktree drover is about to
// push, review or remove. Kill(-pgid, 0) answers ESRCH the moment the group is
// empty, so a session that cleaned up costs nothing here; lingerGrace covers a
// child that signalled its own helpers and exited before they did.
func leftRunning(pgid int) []string {
	deadline := time.Now().Add(lingerGrace)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(lingerPoll)
	}
	procs := groupMembers(pgid)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	if len(procs) == 0 {
		procs = []string{fmt.Sprintf("process group %d", pgid)}
	}
	return procs
}

// groupMembers lists a process group as "pid N: command". ps rather than
// /proc, because the same flags read on macOS and Linux and /proc exists on one.
func groupMembers(pgid int) []string {
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,command=").Output() // #nosec G204 -- fixed argv
	if err != nil {
		return nil
	}
	var procs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != strconv.Itoa(pgid) {
			continue
		}
		procs = append(procs, "pid "+f[0]+": "+strings.Join(f[2:], " "))
	}
	return procs
}

// processAlive asks the kernel whether pid exists. EPERM is a process that
// exists and belongs to someone else, which is alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
