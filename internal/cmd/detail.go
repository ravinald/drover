package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
	"github.com/ravinald/drover/internal/runner"
)

// Why the session or the push under a park failed. park is what drive stopped
// on and reason the sentence; detail is the cause a supervisor would otherwise
// learn by reading session logs: a reviewer denied Bash, a gate left running in the
// background, an agy stream cut, a SIGKILL beside another item's gate, an
// OAuth token that expired, a pre-push hook or ssh refusing the push.
//
// A token rather than prose for the reason park is one: a wrapper keys on it.
// unknown is a park whose cause none of the others names, and it carries the
// last lines of the log so the operator reads them in the park rather than
// opening the file.
const (
	detailRunnerExit  = "runner_exit"
	detailToolDenied  = "tool_denied"
	detailLeftRunning = "background_left_running"
	detailStreamCut   = "stream_cut"
	detailAuth        = "auth"
	detailOOM         = "oom"
	detailPushHook    = "push_hook"
	detailPushNetwork = "push_network"
	detailUnknown     = "unknown"
)

// tailLines is how many of a log's last lines an unknown detail carries.
const tailLines = 5

// parkDetail is the detail token, the line that is its evidence, and the log
// that line came from.
type parkDetail struct {
	Detail   string
	Evidence string
	Log      string
}

// lostReview reports a review pass that never reached a result about the work:
// its session died, or recorded nothing because it was cut or killed.
// rerun_lost_review answers these with one more pass on the same head. A pass
// that recorded nothing because the runner denied it tools is not here, since
// the same prompt meets the same denial; nor is one that recorded a changes row or
// an audit and then exited non-zero, because that pass reached its result.
func lostReview(v, detail string) bool {
	return v == passDied ||
		v == passSilent && (detail == detailStreamCut || detail == detailOOM || detail == detailRunnerExit)
}

// sessionDetail reads why one launch failed from what it left: the terminal
// event, the exit status, the processes it left behind, and the log.
//
// The order is the evidence's strength. A runner that said it holds no
// credentials is believed before its exit status, which is that refusal's
// consequence; a SIGKILL outranks a stream that also ended mid-event, because
// the kill is what cut it; and a denial is last of the named causes, because
// a session that exited non-zero after one failed on the exit, not the denial.
func sessionDetail(oc runner.Outcome, err error) parkDetail {
	pd := parkDetail{Log: oc.LogPath}
	res := oc.Result
	tail := logTail(oc.LogPath, 20)
	switch {
	case res.Fault == runlog.FaultAuth:
		pd.Detail, pd.Evidence = detailAuth, trim(firstLine(strings.TrimSpace(res.Text)), 300)
	case oc.Exit == 137 || oc.Signal == int(syscall.SIGKILL):
		pd.Detail, pd.Evidence = detailOOM, exitLine(oc)+", which is the kernel or a supervisor killing it, most often for memory"
	case cutLine(res.Text, tail) != "":
		pd.Detail, pd.Evidence = detailStreamCut, cutLine(res.Text, tail)
	case len(oc.LeftRunning) > 0:
		pd.Detail = detailLeftRunning
		pd.Evidence = "still running when the session exited, and killed: " + strings.Join(oc.LeftRunning, "; ")
	case oc.Exit != 0:
		pd.Detail, pd.Evidence = detailRunnerExit, exitLine(oc)
		if oc.Exit == -1 && oc.Signal == 0 && err != nil {
			pd.Evidence = "no session started: " + err.Error()
		}
	case len(res.Denied) > 0:
		pd.Detail, pd.Evidence = detailToolDenied, res.Denials()
	default:
		pd.Detail, pd.Evidence = detailUnknown, lastLines(strings.Join(tail, "\n"), tailLines)
	}
	return pd
}

// exitLine is how the runner process ended, in the terms a person checks
// against a kill log.
func exitLine(oc runner.Outcome) string {
	if oc.Signal != 0 {
		return fmt.Sprintf("the runner was killed by signal %d (%s)", oc.Signal, syscall.Signal(oc.Signal))
	}
	return fmt.Sprintf("the runner exited %d", oc.Exit)
}

// cutSigns are what a runner writes when its stream to the model ended under
// it: agy's "The stream was interrupted", and the same event in other words.
var cutSigns = []string{"stream was interrupted", "stream interrupted", "stream ended unexpectedly", "stream closed"}

// cutLine is the line naming a cut stream, from the terminal event or the
// log's last lines, and empty when neither names one.
func cutLine(text string, tail []string) string {
	for _, line := range append([]string{text}, tail...) {
		l := strings.ToLower(line)
		for _, sign := range cutSigns {
			if strings.Contains(l, sign) {
				return trim(firstLine(strings.TrimSpace(line)), 300)
			}
		}
	}
	return ""
}

// logTail is a log's last n lines, each cut to a width a park can carry. A
// log that will not open has no tail, which is itself what an unknown with an
// empty evidence says.
func logTail(path string, n int) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path) // #nosec G304 -- a log drover itself wrote
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, trim(line, 200))
			if len(lines) > n {
				lines = lines[1:]
			}
		}
	}
	return lines
}

// networkSigns are a push that never reached the remote's answer: ssh or
// HTTPS failing underneath git, such as "Connection to github.com closed by
// remote host", which `git push` reports in those words.
var networkSigns = []string{
	"could not read from remote repository",
	"could not resolve host",
	"could not resolve hostname",
	"closed by remote host",
	"connection refused",
	"connection reset",
	"connection timed out",
	"operation timed out",
	"remote end hung up unexpectedly",
	"unable to access",
	"early eof",
	"kex_exchange_identification",
	"broken pipe",
}

// pushDetail reads why `git push` refused, from its output and the hooks the
// tree declares.
//
// git names no hook when one refuses: a pre-push hook that exits 1 leaves its
// own output and then "error: failed to push some refs", the same line a
// rejected ref leaves. So the hook is found by asking git which one would
// have run, and a refusal is read as the hook's only where one exists and the
// remote said nothing of its own. A remote rejection (a protected branch, a
// server-side hook) is the forge's answer and stays unknown, with its lines.
//
// drover never adds --no-verify to answer either: a hook is the project's
// check on what leaves the machine, and the operator owns the decision to
// skip it.
func pushDetail(root string, err error) parkDetail {
	msg := strings.TrimSpace(err.Error())
	low := strings.ToLower(msg)
	if line := firstMatch(msg, networkSigns); line != "" {
		return parkDetail{Detail: detailPushNetwork, Evidence: trim(line, 300)}
	}
	if !strings.Contains(msg, "[rejected]") && !strings.Contains(msg, "[remote rejected]") &&
		!strings.HasPrefix(low, "no branch ") {
		if hook := prePushHook(root); hook != "" {
			return parkDetail{Detail: detailPushHook, Evidence: hook + ": " + trim(firstLine(msg), 300)}
		}
	}
	return parkDetail{Detail: detailUnknown, Evidence: lastLines(msg, tailLines)}
}

// firstMatch is the first line of text carrying any of signs, which is the
// one nearest the cause: git prints the transport's own line, then its
// generic "Could not read from remote repository".
func firstMatch(text string, signs []string) string {
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(line)
		for _, sign := range signs {
			if strings.Contains(l, sign) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// prePushHook names the pre-push hook git would run in root, empty when none
// would. Config hooks (git 2.54's hook.<name>.event) are named by their
// name; a hook file by its path, which core.hooksPath moves.
func prePushHook(root string) string {
	if out, err := gitcmd.Command(root, "config", "--get-regexp", `^hook\..*\.event$`).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
			if ok && strings.TrimSpace(val) == "pre-push" {
				return "pre-push hook " + strings.TrimSuffix(strings.TrimPrefix(key, "hook."), ".event")
			}
		}
	}
	out, err := gitcmd.Command(root, "rev-parse", "--git-path", "hooks/pre-push").Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if info, err := os.Stat(path); err == nil && info.Mode()&0o111 != 0 {
		return "pre-push hook " + path
	}
	return ""
}

// recordPark fills the detail of a park nothing named one for, from the last
// launch the item made, and appends the twelfth-shape row status and show read
// it back from. A park before any launch has no session to read, and says so.
func (s *session) recordPark(d *DriveItem, last *runner.Outcome, lastErr error) {
	if d.Detail == "" {
		pd := parkDetail{Detail: detailUnknown, Evidence: "no session ran for this item in this drive"}
		if last != nil {
			pd = sessionDetail(*last, lastErr)
		}
		d.setDetail(pd)
	}
	row := ledger.Row{ID: d.ID, Park: d.Park, Detail: d.Detail, Evidence: d.Evidence, Log: d.Log, Date: ledger.Today()}
	if err := s.record(row); err != nil {
		fmt.Fprintf(os.Stderr, "  · %s: the ledger refused the park row (%v); status will read the item as implemented\n", d.ID, err)
	}
}

// announcePark says a park on stderr the moment it happens. driveSummary says
// it again once the queue drains, which under --follow is long after the next
// item's banner has pushed the review result before it out of sight, and a
// park nobody sees is a reviewed item nobody merges.
func (s *session) announcePark(d DriveItem) {
	at := "no head"
	if d.Branch != "" {
		if sha, err := revParse(s.cfg.Root, d.Branch); err == nil {
			at = shortSHA(sha)
		}
	}
	why, _, _ := strings.Cut(d.Reason, "\n")
	fmt.Fprintf(os.Stderr, "  ! %s parked %s at %s: %s\n", d.ID, d.Park, at, why)
}

// blankDetail is a park row's detail for display; a row written by hand may
// carry none.
func blankDetail(d string) string {
	if d == "" {
		return detailUnknown
	}
	return d
}

// setDetail copies a detail onto the item's account.
func (d *DriveItem) setDetail(pd parkDetail) {
	d.Detail, d.Evidence, d.Log = pd.Detail, pd.Evidence, pd.Log
}
