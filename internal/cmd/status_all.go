package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/projects"
)

// ProjectStatus is one registered project under `status --all-projects`.
type ProjectStatus struct {
	Project string `json:"project"`
	Root    string `json:"root"`
	// Drive and Label name the drive holding the project's checkout, Running
	// the items it has in a stage now, Queue how many wait behind them and
	// Paused whether drive --pause holds it. All five are absent when no drive
	// runs there.
	Drive   string   `json:"drive,omitempty"`
	Label   string   `json:"label,omitempty"`
	Running []string `json:"running,omitempty"`
	Queue   *int     `json:"queue,omitempty"`
	Paused  bool     `json:"paused,omitempty"`
	// Attention is how many entries the project's attention list holds, the
	// status template's "needs you".
	Attention *int `json:"attention,omitempty"`
	// Finished is the trends' finished total over the window, null when the
	// window holds no timed row, and LastFinished the newest finish ever.
	Finished     *TrendCount `json:"finished,omitempty"`
	LastFinished string      `json:"last_finished,omitempty"`
	// Renders is every [[status.render]] pair the project declares, so an index
	// can link each project's own dashboard.
	Renders []ProjectRender `json:"renders,omitempty"`
	// Worktrees is the root of every registered project that is a git
	// worktree of this one. Each is shown here and never as a project.
	Worktrees []string `json:"worktrees,omitempty"`
	// LeaseError is why the checkout's holder could not be read.
	LeaseError string `json:"lease_error,omitempty"`
	// Error is why the project could not be read at all, present only then;
	// every field above but the name and root is then absent.
	Error string `json:"error,omitempty"`
}

// ProjectRender is one [[status.render]] pair of a registered project.
type ProjectRender struct {
	Template string `json:"template"`
	// Path is the rendered file, absolute. Href is the same file relative to
	// the directory -o writes into, and Path again without -o.
	Path   string `json:"path"`
	Href   string `json:"href"`
	Format string `json:"format"`
}

// AllProjects is what `status --all-projects --json` emits and its template
// reads.
type AllProjects struct {
	Projects []ProjectStatus `json:"projects"`
	Now      string          `json:"now"`
	// Skipped counts the registered roots left out because they are gone or
	// hold no config: a record of where drover was pointed, not a project.
	Skipped int `json:"skipped,omitempty"`
	// Scratch counts the projects left out because they sit under TempDir,
	// the OS temp directory: real projects, but reproductions rather than
	// anything this host drives. TempDir is present only beside a count.
	Scratch int    `json:"scratch,omitempty"`
	TempDir string `json:"temp_dir,omitempty"`
}

// statusEverywhere is `status --all-projects`: one row per project in the
// per-user registry. A project that will not read is a row carrying its error,
// because a project left out reads as one that does not exist. A root that is
// gone or holds no config is not a project, and a project under the temp
// directory is a reproduction, so both are counted rather than shown. A
// worktree of another registered project is shown on that project's row.
func statusEverywhere(w io.Writer) error {
	loc := time.UTC
	if statusTZ != "" {
		var err error
		if loc, err = config.ParseZone(statusTZ); err != nil {
			return fmt.Errorf("--tz %q: %v (known: UTC, local, or an IANA name such as America/Los_Angeles)", statusTZ, err)
		}
	}
	now := time.Now().In(loc)
	path := projects.Path(config.GlobalPath())
	rows, err := projects.Read(path)
	if err != nil {
		return err
	}
	outDir := ""
	if statusOut != "" {
		if outDir, err = filepath.Abs(filepath.Dir(expandHome(statusOut))); err != nil {
			return err
		}
	}
	all := AllProjects{Projects: []ProjectStatus{}, Now: now.Format(time.RFC3339)}
	live, parents, gone := hostProjects(rows)
	all.Skipped = gone
	children := map[string][]string{}
	for _, r := range live {
		if p, ok := parents[r.Root]; ok {
			children[p] = append(children[p], r.Root)
		}
	}
	for _, r := range live {
		if _, ok := parents[r.Root]; ok {
			continue
		}
		if underTemp(r.Root) {
			if _, err := config.LoadAt(r.Root); err == nil {
				all.Scratch++
				continue
			}
		}
		p := projectStatus(r, now, loc, outDir)
		p.Worktrees = children[r.Root]
		all.Projects = append(all.Projects, p)
	}
	if all.Scratch > 0 {
		all.TempDir = filepath.Clean(os.TempDir())
	}

	tmpl := statusTemplate
	if statusHTML {
		tmpl = "builtin:html"
	}
	var out []byte
	switch {
	case tmpl != "":
		s := &session{cfg: &config.Config{}, offline: true, zone: loc}
		raw, err := json.Marshal(all)
		if err != nil {
			return err
		}
		out, err = s.execTemplate(tmpl, raw, func(map[string]any) {}, now)
		if err != nil {
			return err
		}
	case statusJSON:
		var b bytes.Buffer
		if err := encodeIndented(&b, all); err != nil {
			return err
		}
		out = b.Bytes()
	default:
		if all.Skipped+all.Scratch == 0 || len(all.Projects) > 0 {
			printProjects(w, all.Projects, path)
		}
		if all.Skipped+all.Scratch > 0 {
			fmt.Fprintln(w, skippedLine(all.Skipped, all.Scratch))
		}
		return nil
	}
	// A rendered page or --json is a file somebody reads elsewhere, so the count
	// goes to the operator on stderr rather than into the output.
	if all.Skipped+all.Scratch > 0 {
		fmt.Fprintln(errOut, skippedLine(all.Skipped, all.Scratch))
	}
	if statusOut != "" {
		return writeOutput(expandHome(statusOut), out, statusForce)
	}
	_, err = w.Write(out)
	return err
}

// projectStatus reads one registered project as status would from inside it,
// at the registered root exactly: config.Load would honor a DROVER_ROOT this
// process inherited and read that one project under every name.
// The forge is asked only for the register the attention list reads, and not
// at all under --no-tracker.
func projectStatus(r projects.Row, now time.Time, loc *time.Location, outDir string) ProjectStatus {
	p := ProjectStatus{Project: r.Project, Root: r.Root}
	cfg, err := config.LoadAt(r.Root)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	s, err := openFrom(cfg)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	s.offline, s.zone, s.window = statusNoTracker, loc, statusWindow
	report := s.statusReport(false, false, now)
	p.LeaseError = report.LeaseError
	if l := report.Lease; l != nil && l.Command == "drive" {
		p.Drive, p.Label, p.Running, p.Paused = l.Drive, l.Label, l.Running, l.Paused
		q := len(l.Queue)
		p.Queue = &q
	}
	n := len(s.attention(report.all))
	p.Attention = &n
	p.Finished, p.LastFinished = report.Trends.Total.Finished, report.Trends.last
	for _, t := range cfg.Status.Render {
		abs := cfg.Abs(t.Path)
		pr := ProjectRender{Template: t.Template, Path: abs, Href: abs, Format: strings.TrimPrefix(filepath.Ext(abs), ".")}
		if outDir != "" {
			if rel, err := filepath.Rel(outDir, abs); err == nil {
				pr.Href = filepath.ToSlash(rel)
			}
		}
		p.Renders = append(p.Renders, pr)
	}
	return p
}

func printProjects(w io.Writer, ps []ProjectStatus, registry string) {
	if len(ps) == 0 {
		fmt.Fprintf(w, "no projects registered (%s)\n", registry)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tDRIVE\tITEM\tQUEUE\tNEEDS YOU\tFINISHED\tLAST FINISHED")
	dash := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	num := func(n *int) string {
		if n == nil {
			return "-"
		}
		return strconv.Itoa(*n)
	}
	var unreadable []ProjectStatus
	for _, p := range ps {
		if p.Error != "" {
			unreadable = append(unreadable, p)
			continue
		}
		holder := p.Drive
		if holder != "" && p.Label != "" {
			holder += fmt.Sprintf(" %q", p.Label)
		}
		if p.Paused {
			holder += " paused"
		}
		landed := "-"
		if p.Finished != nil {
			landed = strconv.Itoa(p.Finished.Value)
		}
		last := p.LastFinished
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			last = t.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Project, dash(holder), dash(strings.Join(p.Running, ",")),
			num(p.Queue), num(p.Attention), landed, dash(last))
		// Every cell present, so the line extends each column rather than
		// ending the alignment of the rows below it.
		for _, wt := range p.Worktrees {
			fmt.Fprintf(tw, "  worktree %s\t\t\t\t\t\t\n", wt)
		}
	}
	_ = tw.Flush()
	// Below the table rather than in it: a row of one long cell would end the
	// column alignment for every row after it.
	for _, p := range unreadable {
		fmt.Fprintf(w, "%s  unreadable: %s\n", p.Project, oneLine(p.Error))
		for _, wt := range p.Worktrees {
			fmt.Fprintf(w, "  worktree %s\n", wt)
		}
	}
}

// skippedLine is the footer naming how many registered roots were left out,
// the gone and the scratch apart, and the two commands that list and forget
// them.
func skippedLine(gone, scratch int) string {
	var parts []string
	if gone > 0 {
		parts = append(parts, fmt.Sprintf("%d registered %s left out: gone or no %s",
			gone, plural(gone, "root", "roots"), config.FileName))
	}
	if scratch > 0 {
		parts = append(parts, fmt.Sprintf("%d scratch %s under %s left out",
			scratch, plural(scratch, "project", "projects"), filepath.Clean(os.TempDir())))
	}
	return strings.Join(parts, "; ") + " (drover projects lists them, drover projects --forget <root> forgets one)"
}

// oneLine keeps a multi-line error on its row of the table.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// statusAllProjectsRun is the --all-projects branch of status's RunE, apart so
// the refusals it shares with nothing else sit beside it.
func statusAllProjectsRun() error {
	if statusAll || len(statusSev) > 0 || statusBySev {
		return Refusal{fmt.Errorf("--all-projects prints one row per project, and --all, --sev and --by-sev choose items: drop them")}
	}
	return statusEverywhere(os.Stdout)
}
