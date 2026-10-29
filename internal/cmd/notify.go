package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/spf13/cobra"
)

// ntfyWait bounds one ntfy send. It is shorter than notifyWait because a POST
// that has not answered in ten seconds is a server that is down, and every
// item waiting for a slot waits with it.
const ntfyWait = 10 * time.Second

// ntfyClock and ntfyTransport are what a send reads the time from and sends
// through, apart so a test can move the deadline and watch the request without
// sleeping or reaching a network.
var (
	ntfyClock     = time.Now
	ntfyTransport = http.DefaultTransport
)

// ntfyResult is what one send came to: the status ntfy answered with, zero
// where it never answered, and the body it returned, trimmed.
type ntfyResult struct {
	Status int
	Body   string
}

func (r ntfyResult) bodyClause() string {
	if r.Body == "" {
		return ""
	}
	return ": " + lastLines(r.Body, 3)
}

// postNtfy sends one notice to the destination's topic. The token is read
// here, at send time, so a rotated token takes effect on the next message, and
// it is cut from everything returned, since a server echoing the request would
// otherwise put it on stderr. click is the URL a tap on the message opens,
// empty for none.
func postNtfy(d config.Destination, nt notice, click string) (ntfyResult, error) {
	token := ""
	if d.TokenFile != "" {
		b, err := os.ReadFile(d.TokenFile)
		if err != nil {
			return ntfyResult{}, fmt.Errorf("token_file would not read: %w", err)
		}
		if token = strings.TrimSpace(string(b)); token == "" {
			return ntfyResult{}, fmt.Errorf("token_file %s is empty", d.TokenFile)
		}
	}
	redact := func(s string) string {
		if token == "" {
			return s
		}
		return strings.ReplaceAll(s, token, "[token]")
	}
	ctx, cancel := context.WithDeadline(context.Background(), ntfyClock().Add(ntfyWait))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, strings.NewReader(nt.body))
	if err != nil {
		return ntfyResult{}, errors.New(redact(err.Error()))
	}
	title := ntfyTitle(nt.event)
	if nt.reminder != "" {
		title += " " + nt.reminder
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", nt.priority)
	req.Header.Set("Tags", "robot")
	if click != "" {
		req.Header.Set("Click", click)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Transport: ntfyTransport}).Do(req)
	if err != nil {
		return ntfyResult{}, errors.New(redact(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	res := ntfyResult{Status: resp.StatusCode, Body: redact(strings.TrimSpace(string(body)))}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return res, fmt.Errorf("HTTP %s", resp.Status)
	}
	return res, nil
}

// ntfyTitle names the project, and the item where the event has one.
func ntfyTitle(e DriveEvent) string {
	if e.ID == "" {
		return "drover: " + e.Project
	}
	return "drover: " + e.Project + " " + e.ID
}

// tokenLoose is the warning for a token file group or others can read, empty
// for one only its owner can, or one that cannot be stat'd, which the send
// itself reports.
func tokenLoose(path string) string {
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o044 == 0 {
		return ""
	}
	return fmt.Sprintf("token_file %s is readable by group or others (%s): chmod 600 %s", path, fi.Mode().Perm(), path)
}

var (
	notifyTest  bool
	notifyEvent string
)

// testReason is what every sample says, so nobody acts on one.
const testReason = "notification test from drover, nothing to do"

// sampleEvent is a synthetic event of one kind, id TEST, carrying the fields
// that kind's message reads, so a sample renders as the real one would.
func sampleEvent(kind, project string) DriveEvent {
	e := DriveEvent{Event: kind, TS: time.Now().UTC().Format(time.RFC3339), Project: project, ID: "TEST", Reason: testReason}
	switch kind {
	case evStarted:
		e.ID, e.Drive, e.Queue, e.Jobs = "", "D0", []string{"TEST"}, 1
	case evQueued:
		e.Position = 1
	case evPreflight:
		e.Action = "deps_lint"
	case evStage:
		e.Stage = stageGate
	case evCheck:
		e.Check, e.Class, e.Evidence = "test", "code", testReason
	case evParked:
		e.Park, e.Detail = "gate_refused", detailUnknown
	case evFiled:
		e.Kind, e.Ref = findingScaffold, "TEST"
	case evNeedsOperator:
		e.Reason, e.Action, e.Policy, e.Run = "", "test", string(polOperator), testReason
	case evNeedsSupervisor:
		e.Reason, e.Action, e.Policy, e.Run = "", "test", string(polSupervisor), testReason
	case evMerged, evMergeQueued:
		e.Reason, e.Title = "", "TEST: "+testReason
	case evReexecWaiting:
		e.ID, e.Waiting = "", []WaitingItem{{ID: "TEST", Stage: "review"}}
	case evConfigChanged:
		e.Tables = []string{"[drive]"}
	case evUsageWarning:
		left := 20
		e.Window, e.Remaining, e.ResetsAt = config.UsageWeekly, &left, time.Now().Add(24*time.Hour).Format(time.RFC3339)
	case evExit, config.EventExitUndone:
		code := 0
		e.Event, e.ID, e.Drive, e.Code = evExit, "", "D0", &code
		e.Summary = &ExitSummary{Merged: 1, Ended: endedDrained}
		if kind == config.EventExitUndone {
			e.Summary.Parked = 1
			e.Summary.Undone = []UndoneItem{{ID: "TEST", Outcome: outcomeParked, Park: "gate_refused", Reason: testReason}}
		}
	}
	return e
}

// notifyAll sends one sample through every destination cfg configures, one
// line each to w. With no kind the sample is needs_operator and goes to every
// destination, whatever its events name, since the point is to test the
// destination; with a kind it goes at each destination's routed priority, to
// those that route it. It says whether any destination was sent to and
// whether every send succeeded.
func notifyAll(w io.Writer, cfg *config.Config, kind string) (sent, ok bool) {
	route := kind
	if kind == "" {
		route = evNeedsOperator
	}
	e := sampleEvent(route, cfg.Project)
	ok = true
	for _, d := range cfg.Notifications().Destinations {
		p, routed := d.Priority(route)
		switch {
		case !routed && kind != "":
			fmt.Fprintf(w, "%-8s %s routes no %s; not sent\n", d.Name, d.Type, kind)
			continue
		case !routed:
			p = "high"
		}
		sent = true
		nt := notice{event: e, priority: p, body: noticeBody(e)}
		var err error
		switch d.Type {
		case config.DestNtfy:
			if loose := tokenLoose(d.TokenFile); loose != "" {
				fmt.Fprintf(w, "warning: %s\n", loose)
			}
			var res ntfyResult
			res, err = postNtfy(d, nt, "")
			switch {
			case err == nil:
				fmt.Fprintf(w, "%-8s ntfy %s at %s: HTTP %d\n", d.Name, d.URL, p, res.Status)
			case res.Status != 0:
				fmt.Fprintf(w, "%-8s ntfy %s at %s: HTTP %d%s\n", d.Name, d.URL, p, res.Status, res.bodyClause())
			default:
				fmt.Fprintf(w, "%-8s ntfy %s at %s: failed (%v)\n", d.Name, d.URL, p, err)
			}
		case config.DestCommand:
			var out string
			out, err = runArgvNotifier(cfg.Root, d.Argv, nt.payload())
			argv := strings.Join(d.Argv, " ")
			code := 0
			if err != nil {
				code = -1
				if xe, isExit := errors.AsType[*exec.ExitError](err); isExit {
					code = xe.ExitCode()
				}
			}
			switch {
			case err == nil:
				fmt.Fprintf(w, "%-8s command %s at %s: exit 0\n", d.Name, argv, p)
				for th := range threadLines(out) {
					fmt.Fprintf(w, "%-8s opened T3 Code thread %s for %s\n", "", th.URL, th.ID)
				}
			case code >= 0:
				fmt.Fprintf(w, "%-8s command %s at %s: exit %d%s\n", d.Name, argv, p, code, outClause(out))
			default:
				fmt.Fprintf(w, "%-8s command %s at %s: failed (%v)%s\n", d.Name, argv, p, err, outClause(out))
			}
		}
		ok = ok && err == nil
	}
	return sent, ok
}

func outClause(out string) string {
	if out == "" {
		return ""
	}
	return ": " + lastLines(out, 3)
}

var notifyCmd = &cobra.Command{
	Use:   "notify",
	Short: "Send a test event through every configured notification destination.",
	Long: `A drive reaches a person through the destinations under [notify]: an ntfy
topic each routed event is POSTed to, or a command run with each routed event
on stdin. Neither is exercised until a drive emits an event it routes, which
is the worst moment to learn the token expired.

--test sends one synthetic event, id TEST, through every destination
configured, and prints what each came to: the HTTP status for ntfy, the exit
status for a command, and the T3 Code thread a command destination running
drover t3 thread --from-event opened, so a wrong [t3] project or an expired
token shows before a drive parks. It needs no running drive. With no --event the sample is
needs_operator, sent to every destination whatever its events name, at its
routed priority or high where it routes none, since the point is to test the
destination. --event <name> sends a sample of that kind instead (any event a
drive emits, or exit_undone), rendered as the real one would be, at each
destination's routed priority, and skips a destination that does not route it.

Exit 0 when every destination sent to succeeded, 1 when one failed, and 2
when none is configured or none routes the --event named.`,
	Example: `  drover notify --test
    command  command notify-send drover at default: exit 0
    phone    ntfy https://ntfy.example.com/drover at high: HTTP 200

  drover notify --test --event exit_undone
    command  command routes no exit_undone; not sent
    phone    ntfy https://ntfy.example.com/drover at high: HTTP 200`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		if !notifyTest {
			if notifyEvent != "" {
				return errors.New("--event names the sample --test sends; pass --test with it")
			}
			return c.Help()
		}
		if notifyEvent != "" && !slices.Contains(config.NotifyEvents(), notifyEvent) {
			return fmt.Errorf("--event %q is no event a destination can route; one of %s", notifyEvent, strings.Join(config.NotifyEvents(), ", "))
		}
		s, err := open()
		if err != nil {
			return err
		}
		sent, ok := notifyAll(os.Stdout, s.cfg, notifyEvent)
		switch {
		case !sent && len(s.cfg.Notifications().Destinations) == 0:
			return Findings{errors.New("no notification destination is configured: add a [notify.destinations.<name>] table")}
		case !sent:
			return Findings{fmt.Errorf("no destination routes %s: add it to a destination's events", notifyEvent)}
		case !ok:
			return errors.New("a notification destination failed; its line above says how")
		}
		return nil
	},
}

func init() {
	notifyCmd.Flags().BoolVar(&notifyTest, "test", false, "send one test event through every configured destination")
	notifyCmd.Flags().StringVar(&notifyEvent, "event", "", "with --test, send a sample of this event kind at its routed priority")
	root.AddCommand(notifyCmd)
}
