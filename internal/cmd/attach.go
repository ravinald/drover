package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/spf13/cobra"
)

// driveEventNames is every event a drive emits, which is what watch --until
// may name.
var driveEventNames = []string{
	evQueued, evDispatched, evPreflight, evStage, evCheck, evParked, evFiled,
	evNeedsOperator, evMerged, evPaused, evResumed, evExit,
}

// driveEventsPath is where a drive keeps its events, one object per line,
// whether or not it streams them under --events. Per-user state rather than
// the project: a process that did not launch the drive has to find it, and
// nothing the project tracks may name one machine's drive.
func driveEventsPath(project, id string) (string, error) {
	dir, err := driveFilesDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".jsonl"), nil
}

// driveFilesDir holds a project's drive event files and detached drive logs.
func driveFilesDir(project string) (string, error) {
	dir, err := stateDir("drives")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, project), nil
}

// followed is one drive attach or watch reads: its id, its event file and the
// lease of the checkout it runs in, which says whether it still runs.
type followed struct {
	id     string
	events string
	lease  string
}

// errDriveGone is a drive that stopped running with no exit event in its file:
// killed, or its event file stopped taking writes.
var errDriveGone = errors.New("ended without an exit event")

// followPoll is how often attach and watch look for a line the drive appended.
var followPoll = 200 * time.Millisecond

// resolveFollowed names the drive args points at: the id given, else the drive
// running in this checkout, else the project's latest. An id that neither the
// ledger nor the lease names is no drive at all.
func resolveFollowed(args []string) (followed, error) {
	wd, err := os.Getwd()
	if err != nil {
		return followed{}, err
	}
	cfg, err := config.Load(wd)
	if err != nil {
		return followed{}, err
	}
	lp, err := (&session{cfg: cfg}).leasePath()
	if err != nil {
		return followed{}, err
	}
	led, err := ledger.Load(cfg.Abs(cfg.Paths.Ledger))
	if err != nil {
		return followed{}, err
	}
	id := ""
	if len(args) == 1 {
		id = strings.ToUpper(args[0])
	}
	l, busy, _ := lease.Read(lp)
	running := busy && l.Command == "drive" && l.Drive != ""
	ds := led.Drives()
	switch {
	case id != "":
	case running:
		id = l.Drive
	case len(ds) > 0:
		id = ds[0].ID
	default:
		return followed{}, errors.New("no drive has run in this project, so there is none to follow")
	}
	if (!running || l.Drive != id) && !slices.ContainsFunc(ds, func(d ledger.Drive) bool { return d.ID == id }) {
		return followed{}, fmt.Errorf("no drive %s in this project; drover drives lists them", id)
	}
	path, err := driveEventsPath(cfg.Project, id)
	if err != nil {
		return followed{}, err
	}
	return followed{id: id, events: path, lease: lp}, nil
}

// running reports whether the checkout's lease still names this drive. A drive
// holding the lease that has not named its id yet counts, since it may be this
// one about to.
func (f followed) running() bool {
	l, busy, err := lease.Read(f.lease)
	return err == nil && busy && l.Command == "drive" && (l.Drive == f.id || l.Drive == "")
}

// follow hands fn each event in the drive's file from the start, with its line
// number from 1 (the seq the drive stamped on it) and the offset just past the
// line, and waits for more until fn returns true or the
// exit event is read. Liveness is read before the file each time, so a line the
// drive wrote just before it let the lease go is still read.
func (f followed) follow(ctx context.Context, fn func(line []byte, n int, end int64, e DriveEvent) bool) error {
	var off, consumed int64
	n := 0
	var buf []byte
	seen := false
	for {
		alive := f.running()
		chunk, exists, err := readFrom(f.events, off)
		if err != nil {
			return err
		}
		seen = seen || exists
		off += int64(len(chunk))
		buf = append(buf, chunk...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[:i]
			buf = buf[i+1:]
			consumed += int64(i) + 1
			n++
			var e DriveEvent
			if json.Unmarshal(line, &e) != nil || e.Event == "" {
				continue
			}
			if fn(line, n, consumed, e) || e.Event == evExit {
				return nil
			}
		}
		if !alive {
			if !seen {
				return fmt.Errorf("drive %s is not running and has no event file at %s: it ran before drives kept one, or the file would not write", f.id, f.events)
			}
			return fmt.Errorf("drive %s %w at %s: it was killed, or its event file stopped taking writes", f.id, errDriveGone, f.events)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(followPoll):
		}
	}
}

// readFrom is path's bytes past off. A file not there yet is empty: a drive
// opens it with its id, and attach may get there first.
func readFrom(path string, off int64) ([]byte, bool, error) {
	f, err := os.Open(path) // #nosec G304 -- under the per-user state directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, true, err
	}
	b, err := io.ReadAll(f)
	return b, true, err
}

// before reports whether an event stamped ts was emitted before since. An
// unreadable stamp is never skipped.
func before(ts string, since time.Time) bool {
	at, err := time.Parse(time.RFC3339, ts)
	return err == nil && at.Before(since)
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since takes an event's ts, RFC 3339 (2026-10-18T09:00:00Z); got %q", s)
	}
	return at, nil
}

// eventLine is one event as attach prints it without --json: the stamp, the
// event, the item, then whichever fields it carries.
func eventLine(e DriveEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-14s", e.TS, e.Event)
	if e.ID != "" {
		b.WriteString(" " + e.ID)
	}
	add := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, " %s=%s", k, v)
		}
	}
	num := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	add("stage", e.Stage)
	add("park", e.Park)
	add("detail", e.Detail)
	add("action", e.Action)
	add("policy", e.Policy)
	add("pr", num(e.PR))
	add("slot", num(e.Slot))
	add("position", num(e.Position))
	add("check", e.Check)
	add("class", e.Class)
	add("head", e.Head)
	if e.Code != nil {
		add("code", strconv.Itoa(*e.Code))
	}
	add("changed", strings.Join(e.Changed, ","))
	add("seq", num(e.Seq))
	for _, kv := range [][2]string{{"reason", e.Reason}, {"run", e.Run}, {"evidence", e.Evidence}} {
		if kv[1] != "" {
			add(kv[0], strconv.Quote(kv[1]))
		}
	}
	return b.String()
}

var (
	attachSince string
	attachJSON  bool
)

var attachCmd = &cobra.Command{
	Use:   "attach [drive id]",
	Short: "Print a drive's events from the start and follow them until it exits.",
	Long: `Reads the drive's event file, which every drive writes whether or not it ran
under --events or --detach, prints it from the first line, then follows it
until the drive's exit event and exits 0. With no id it follows the drive
running in this checkout, else the project's latest.

An interrupt stops following and leaves the drive running: attach never
signals the drive or writes anything it reads. A drive that stops running with
no exit event in its file (killed, or its event file stopped taking writes)
exits 1, and so does an id this project has no drive under.

--since skips events stamped before it. --json prints each line as the drive
wrote it, the objects documented under drive --events; without it each event
is one readable line.`,
	Example: `  drover attach                     # the drive running here, or the latest
  drover attach D3 --json           # its events as JSON, one per line
  drover attach --since 2026-10-18T09:00:00Z

  2026-10-18T09:12:03Z  dispatched     4
  2026-10-18T09:12:03Z  stage          4 stage=implement
  2026-10-18T09:40:51Z  parked         4 park=silent detail=unknown reason="the review recorded nothing"
  2026-10-18T09:40:52Z  exit           code=0

  under --json, each line as the drive wrote it:
  {"event":"exit","ts":"2026-10-18T09:40:52Z","drive":"D3","code":0}`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(c *cobra.Command, args []string) error {
		since, err := parseSince(attachSince)
		if err != nil {
			return err
		}
		f, err := resolveFollowed(args)
		if err != nil {
			return err
		}
		err = f.follow(c.Context(), func(line []byte, _ int, _ int64, e DriveEvent) bool {
			if !since.IsZero() && before(e.TS, since) {
				return false
			}
			if attachJSON {
				fmt.Fprintf(os.Stdout, "%s\n", line)
			} else {
				fmt.Fprintln(os.Stdout, eventLine(e))
			}
			return false
		})
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "· stopped following drive %s; it is still running\n", f.id)
			return nil
		}
		return err
	},
}

func init() {
	attachCmd.Flags().StringVar(&attachSince, "since", "", "skip events stamped before this RFC 3339 time")
	attachCmd.Flags().BoolVar(&attachJSON, "json", false, "print each event as the JSON line the drive wrote")
	root.AddCommand(attachCmd)
}
