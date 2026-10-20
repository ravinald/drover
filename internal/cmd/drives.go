package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/lease"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/projects"
	"github.com/spf13/cobra"
)

// runningDrive is the drive this process is running: its id, whether its
// start row is written, and what each item it ran came to, for the end row.
// register is the last register its finding watch read, nil before the first
// read that answered; the drive's render judges that one rather than asking
// the forge on every row.
type runningDrive struct {
	id       string
	mu       sync.Mutex
	started  bool
	outcomes []ledger.Outcome
	register *gh.Index
}

// activeDrive is the running drive, nil before its id is allocated and in every
// process that is not a drive. Package state for the reason held is: every
// session a drive forks for its items writes rows, and each has to name it.
var activeDrive atomic.Pointer[runningDrive]

func (d *runningDrive) note(item DriveItem) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.outcomes = append(d.outcomes, ledger.Outcome{ID: item.ID, Outcome: item.Outcome})
}

// revise replaces the outcome noted for item's id with the one it has now,
// for an item the drive picked up again after noting it.
func (d *runningDrive) revise(item DriveItem) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.outcomes) - 1; i >= 0; i-- {
		if d.outcomes[i].ID == item.ID {
			d.outcomes[i].Outcome = item.Outcome
			return
		}
	}
	d.outcomes = append(d.outcomes, ledger.Outcome{ID: item.ID, Outcome: item.Outcome})
}

func (d *runningDrive) sawRegister(index gh.Index) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.register = &index
}

// lastRegister is the register as this drive last read it, false before it has.
func (d *runningDrive) lastRegister() (gh.Index, bool) {
	if d == nil {
		return gh.Index{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.register == nil {
		return gh.Index{}, false
	}
	return *d.register, true
}

// driveStamp names the running drive on a row it appends.
func driveStamp(r ledger.Row) ledger.Row {
	if d := activeDrive.Load(); d != nil && r.Drive == "" {
		r.Drive = d.id
	}
	return r
}

// allocDrive allocates the drive's id and names it in the lease, the moment
// the drive holds that lease. Reconciliation runs finish and appends rows before
// the queue is read, and an --enqueue taken into an empty queue emits events,
// so an id taken any later leaves rows and events of this drive naming none.
func (s *session) allocDrive() error {
	id, err := ledger.NextDrive(s.cfg.Abs(s.cfg.Paths.Ledger))
	if err != nil {
		return err
	}
	activeDrive.Store(&runningDrive{id: id})
	events.openFile(id)
	if held != nil {
		_ = held.Update(func(l *lease.Lease) { l.Drive, l.Label = id, driveLabel })
	}
	return nil
}

// startDrive writes the start row once the queue the drive launches with is
// known, under the id allocDrive took.
func (s *session) startDrive(items []backlog.Item) error {
	d := activeDrive.Load()
	if d == nil {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID.Bare())
	}
	if err := s.record(ledger.Row{ID: ledger.DriveRowID, Label: driveLabel, Queued: ids, Jobs: driveJobs}); err != nil {
		return err
	}
	d.mu.Lock()
	d.started = true
	d.mu.Unlock()
	return nil
}

// endDrive appends the end row: the exit code err maps to and each item's
// outcome. A drive that exits before its start row went in (an empty queue, a
// reconciliation that failed) writes one first with nothing queued, because
// Drives reads an end row with no start row as a drive that never started.
// One refused before then writes neither while no row names it: exit 3
// promises the ledger untouched, and its id stays free for the next drive. A
// row reconciliation wrote before the refusal does name it, and then the pair
// goes in so drives lists the drive that row points at. A row that will not write is
// reported and changes no exit: the drive's own report already said what
// happened.
func (s *session) endDrive(err error) {
	d := activeDrive.Load()
	if d == nil {
		return
	}
	code := Code(err)
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if !started {
		if code == ExitRefused && !s.ledgerNames(d.id) {
			return
		}
		if werr := s.startDrive(nil); werr != nil {
			fmt.Fprintf(os.Stderr, "· drive %s: the start row would not write: %v\n", d.id, werr)
		}
	}
	d.mu.Lock()
	out := slices.Clone(d.outcomes)
	d.mu.Unlock()
	if werr := s.record(ledger.Row{ID: ledger.DriveRowID, Exit: &code, Outcomes: out}); werr != nil {
		fmt.Fprintf(os.Stderr, "· drive %s: the end row would not write: %v\n", d.id, werr)
	}
}

// ledgerNames reports whether any row on disk carries drive id. On disk, not
// the loaded copy: reconciliation's finish runs on a session of its own. A ledger
// that will not read answers yes, so the drive's rows are attempted rather than
// silently dropped.
func (s *session) ledgerNames(id string) bool {
	l, err := ledger.Load(s.cfg.Abs(s.cfg.Paths.Ledger))
	if err != nil {
		return true
	}
	return slices.ContainsFunc(l.Rows, func(r ledger.Row) bool { return r.Drive == id })
}

// drivesRunning marks the drive the lease names as running, and as paused
// when the lease reads paused.
func drivesRunning(ds []ledger.Drive, l lease.Lease, busy bool) {
	for i := range ds {
		ds[i].Running = busy && l.Drive != "" && ds[i].ID == l.Drive
		ds[i].Paused = ds[i].Running && l.Paused
	}
}

// ProjectDrives is one registered project under `drives --all --json`.
type ProjectDrives struct {
	Project string         `json:"project"`
	Root    string         `json:"root"`
	Drives  []ledger.Drive `json:"drives"`
	// Error is why this project's drives could not be read, present only then.
	// The rest of the registry still answers.
	Error string `json:"error,omitempty"`
}

var (
	drivesJSON bool
	drivesAll  bool
)

var drivesCmd = &cobra.Command{
	Use:   "drives",
	Short: "Every drive this project has run, newest first, and what each item came to.",
	Long: `Reads the drive rows in the ledger. Each drive that launches takes the next id in
its project, D1, D2 and so on, with the label --label gave it, and every row it
appends carries that id. A drive with no end row is running when this checkout's
lease names it, and interrupted otherwise: it was killed before it could write
one.

--all reads every project in the per-user registry instead, and needs no
project, so it answers from anywhere. A project whose config or ledger will not
read says why and the rest still answer.`,
	Example: `  drover drives                     # this project's drives, newest first
  drover drives --all               # every registered project's
  drover drives --json              # an array, one object per drive:
    [{"id":"D2","label":"night run","started":"2026-10-17T01:00:00-07:00",
      "queued":["4","5"],"jobs":2,"items":[{"id":"4","outcome":""},{"id":"5","outcome":""}],
      "running":true,"paused":true},
     {"id":"D1","started":"2026-10-16T22:00:00-07:00","queued":["1","2"],
      "ended":"2026-10-16T23:10:00-07:00","exit":0,
      "items":[{"id":"1","outcome":"ready_to_merge"},{"id":"2","outcome":"parked"}]}]
  drover drives --all --json        # an array, one object per project:
    [{"project":"webapp","root":"/home/me/src/webapp","drives":[...]}]

  ended and exit are absent until the drive wrote its end row. items is each
  item's outcome from that row, and the ids queued at launch with an empty
  outcome until then. running is true while this checkout's lease names the
  drive, and paused while that lease reads paused (drive --pause).`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if drivesAll {
			return drivesEverywhere(os.Stdout)
		}
		s, err := open()
		if err != nil {
			return err
		}
		ds := s.drives()
		if drivesJSON {
			return encodeIndented(os.Stdout, ds)
		}
		printDrives(os.Stdout, ds, "")
		return nil
	},
}

// drives is the project's drives, with the running one marked. A lease that
// will not read marks none running, which the table prints as interrupted:
// drives answers from the ledger, and the lease only adds the one word.
func (s *session) drives() []ledger.Drive {
	ds := s.ledger.Drives()
	l, busy, _ := s.leaseHolder()
	drivesRunning(ds, l, busy)
	return ds
}

func drivesEverywhere(w io.Writer) error {
	path := projects.Path(config.GlobalPath())
	rows, err := projects.Read(path)
	if err != nil {
		return err
	}
	out := []ProjectDrives{}
	live, parents, _ := hostProjects(rows)
	for _, r := range live {
		// A worktree's ledger is a copy of its parent's, or none at all, so
		// the parent's entry is the record of its drives.
		if _, child := parents[r.Root]; child {
			continue
		}
		p := ProjectDrives{Project: r.Project, Root: r.Root, Drives: []ledger.Drive{}}
		if cfg, err := config.LoadAt(r.Root); err != nil {
			p.Error = err.Error()
		} else if l, err := ledger.Load(cfg.Abs(cfg.Paths.Ledger)); err != nil {
			p.Error = err.Error()
		} else {
			s := &session{cfg: cfg, ledger: l}
			p.Drives = s.drives()
		}
		out = append(out, p)
	}
	if drivesJSON {
		return encodeIndented(w, out)
	}
	if len(out) == 0 {
		fmt.Fprintf(w, "no projects registered (%s)\n", path)
		return nil
	}
	for i, p := range out {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s  %s\n", p.Project, p.Root)
		if p.Error != "" {
			fmt.Fprintf(w, "  unreadable: %s\n", p.Error)
			continue
		}
		printDrives(w, p.Drives, "  ")
	}
	return nil
}

func encodeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printDrives(w io.Writer, ds []ledger.Drive, indent string) {
	if len(ds) == 0 {
		fmt.Fprintf(w, "%sno drives recorded\n", indent)
		return
	}
	for _, d := range ds {
		state := "interrupted"
		switch {
		case d.Exit != nil:
			state = fmt.Sprintf("ended %s  exit %d", d.Ended, *d.Exit)
		case d.Paused:
			state = "running, paused"
		case d.Running:
			state = "running"
		}
		label := ""
		if d.Label != "" {
			label = fmt.Sprintf("  %q", d.Label)
		}
		fmt.Fprintf(w, "%s%-4s started %s  %s%s\n", indent, d.ID, d.Started, state, label)
		items := make([]string, 0, len(d.Items))
		for _, it := range d.Items {
			if it.Outcome == "" {
				items = append(items, it.ID)
				continue
			}
			items = append(items, it.ID+" "+it.Outcome)
		}
		if len(items) > 0 {
			fmt.Fprintf(w, "%s     %s\n", indent, strings.Join(items, ", "))
		}
	}
}

func init() {
	drivesCmd.Flags().BoolVar(&drivesJSON, "json", false, "emit the drives as JSON")
	drivesCmd.Flags().BoolVar(&drivesAll, "all", false, "every project in the per-user registry, not just this one")
	root.AddCommand(drivesCmd)
}
