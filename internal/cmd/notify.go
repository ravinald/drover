package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
	ntfyClock                       = time.Now
	ntfyTransport http.RoundTripper = http.DefaultTransport
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

// postNtfy sends one event to the topic. The token is read here, at send
// time, so a rotated token takes effect on the next event, and it is cut from
// everything returned, since a server echoing the request would otherwise put
// it on stderr.
func postNtfy(n config.Ntfy, e DriveEvent) (ntfyResult, error) {
	token := ""
	if n.TokenFile != "" {
		b, err := os.ReadFile(n.TokenFile)
		if err != nil {
			return ntfyResult{}, fmt.Errorf("token_file would not read: %w", err)
		}
		if token = strings.TrimSpace(string(b)); token == "" {
			return ntfyResult{}, fmt.Errorf("token_file %s is empty", n.TokenFile)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(ntfyMessage(e)))
	if err != nil {
		return ntfyResult{}, errors.New(redact(err.Error()))
	}
	req.Header.Set("Title", ntfyTitle(e))
	req.Header.Set("Priority", n.Priority)
	req.Header.Set("Tags", "robot")
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

// ntfyMessage is what the operator reads on the phone: what to do where the
// event says, else why it was sent, else which event it is.
func ntfyMessage(e DriveEvent) string {
	switch {
	case e.Run != "" && e.Action != "":
		return e.Action + ": " + e.Run
	case e.Run != "":
		return e.Run
	case e.Reason != "":
		return e.Reason
	}
	return e.Event
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

var notifyTest bool

// notifyTestEvent is the event --test sends: needs_operator, because that is
// the event every destination is configured for by default.
func notifyTestEvent(project string) DriveEvent {
	return DriveEvent{Event: evNeedsOperator, TS: time.Now().UTC().Format(time.RFC3339), Project: project,
		ID: "TEST", Action: "test", Policy: string(polOperator), Run: "notification test from drover, nothing to do"}
}

// notifyAll sends the test event through every destination cfg configures,
// one line each to w, and says whether any was configured and whether all
// succeeded.
func notifyAll(w io.Writer, cfg *config.Config) (configured, ok bool) {
	e := notifyTestEvent(cfg.Project)
	ok = true
	if argv := cfg.Notify(); len(argv) > 0 {
		configured = true
		out, err := runArgvNotifier(cfg.Root, argv, e)
		code := 0
		if err != nil {
			code = -1
			if xe, isExit := errors.AsType[*exec.ExitError](err); isExit {
				code = xe.ExitCode()
			}
		}
		switch {
		case err == nil:
			fmt.Fprintf(w, "notify  %s: exit 0\n", strings.Join(argv, " "))
		case code >= 0:
			fmt.Fprintf(w, "notify  %s: exit %d%s\n", strings.Join(argv, " "), code, outClause(out))
		default:
			fmt.Fprintf(w, "notify  %s: failed (%v)%s\n", strings.Join(argv, " "), err, outClause(out))
		}
		ok = ok && err == nil
	}
	if n, on := cfg.Ntfy(); on {
		configured = true
		if loose := tokenLoose(n.TokenFile); loose != "" {
			fmt.Fprintf(w, "warning: %s\n", loose)
		}
		res, err := postNtfy(n, e)
		switch {
		case err == nil:
			fmt.Fprintf(w, "ntfy    %s: HTTP %d\n", n.URL, res.Status)
		case res.Status != 0:
			fmt.Fprintf(w, "ntfy    %s: HTTP %d%s\n", n.URL, res.Status, res.bodyClause())
		default:
			fmt.Fprintf(w, "ntfy    %s: failed (%v)\n", n.URL, err)
		}
		ok = ok && err == nil
	}
	return configured, ok
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
	Long: `A drive reaches a person through [supervise] notify, an argv run with each
needs_operator event on stdin, and [supervise.ntfy], a topic each event it names
is POSTed to. Neither is exercised until a drive stops for somebody, which is
the worst moment to learn the token expired.

--test sends one synthetic needs_operator event, id TEST and action test,
through every destination configured, and prints what each came to: the exit
status for the argv, the HTTP status for ntfy. It needs no running drive. The
event goes to ntfy whatever its events list names, since the point is to test
the destination.

Exit 0 when every configured destination succeeded, 1 when one failed, and 2
when none is configured.`,
	Example: `  drover notify --test
    notify  osascript -e display notification "drover": exit 0
    ntfy    https://ntfy.example.com/drover: HTTP 200`,
	Args: cobra.NoArgs,
	RunE: func(c *cobra.Command, _ []string) error {
		if !notifyTest {
			return c.Help()
		}
		s, err := open()
		if err != nil {
			return err
		}
		configured, ok := notifyAll(os.Stdout, s.cfg)
		switch {
		case !configured:
			return Findings{errors.New("no notification destination is configured: set [supervise] notify, [supervise.ntfy], or both")}
		case !ok:
			return errors.New("a notification destination failed; its line above says how")
		}
		return nil
	},
}

func init() {
	notifyCmd.Flags().BoolVar(&notifyTest, "test", false, "send one test event through every configured destination")
	root.AddCommand(notifyCmd)
}
