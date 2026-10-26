package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/projects"
	"github.com/ravinald/drover/internal/runner"
	"github.com/spf13/cobra"
)

var (
	projectsJSON, projectsAll bool
	projectsForget            string
)

// enroll appends a row for the project cfg names unless the registry already
// holds one for its root. Every project command passes through here, so a tree
// initialized before the registry existed is recorded the first time anything
// runs in it, and a repeat command appends nothing. A session drover launched is
// running in a project the operator's own command already recorded, usually from
// a drive worktree, so it never registers. A tree under the temp directory does:
// the host-wide views leave it out when they read, which is where the decision
// that it is scratch belongs. A failure is a lost line in an audit trail and
// never a reason to refuse the command.
func enroll(cfg *config.Config) {
	if runner.Launched() {
		return
	}
	reg := projects.Path(config.GlobalPath())
	rows, err := projects.Read(reg)
	if err == nil && projects.Registered(rows, cfg.Root) {
		return
	}
	if err == nil {
		row := projects.Row{Project: cfg.Project, Root: cfg.Root, Repo: cfg.GitHub.Repo}
		if p, ok := gitPlaceOf(cfg.Root); ok && p.worktree() {
			row.Parent = worktreeParents(append(projects.Latest(rows), row))[cfg.Root]
		}
		err = projects.Append(reg, row)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not record this project in %s: %v\n", reg, err)
	}
}

// gitPlace is where git keeps one tree: the tree's own git dir, the common dir
// every worktree of its repository shares, and the tree's path inside the
// repository.
type gitPlace struct{ dir, common, prefix string }

// worktree is a linked worktree, whose own git dir sits inside another
// checkout's.
func (p gitPlace) worktree() bool { return p.dir != p.common }

func gitPlaceOf(root string) (gitPlace, bool) {
	out, err := gitcmd.Command(root, "rev-parse", "--path-format=absolute",
		"--git-dir", "--git-common-dir", "--show-prefix").Output()
	if err != nil {
		return gitPlace{}, false
	}
	f := strings.SplitN(string(out), "\n", 4)
	if len(f) < 3 || f[0] == "" || f[1] == "" {
		return gitPlace{}, false
	}
	return gitPlace{dir: resolved(f[0]), common: resolved(f[1]), prefix: f[2]}, true
}

// worktreeParents maps each root in rows that is a linked worktree of another
// root in rows to that root. git answers for every root it can read, so a
// worktree registered before its parent, or before rows carried a parent,
// folds the same way, and a path that stopped being a worktree stops folding.
// A recorded parent answers only where git cannot.
//
// The parent is the main checkout of the same repository at the same path
// inside it. A second project in another directory of one repository shares
// its git dir without being a worktree of anything, so the git dir alone would
// fold one sibling into the other.
func worktreeParents(rows []projects.Row) map[string]string {
	in := map[string]bool{}
	for _, r := range rows {
		in[r.Root] = true
	}
	out := map[string]string{}
	var asked []projects.Row
	places := map[string]gitPlace{}
	for _, r := range rows {
		if p, ok := gitPlaceOf(r.Root); ok {
			places[r.Root] = p
			asked = append(asked, r)
		} else if r.Parent != "" && r.Parent != r.Root && in[r.Parent] {
			out[r.Root] = r.Parent
		}
	}
	for _, c := range asked {
		cp := places[c.Root]
		if !cp.worktree() {
			continue
		}
		for _, m := range asked {
			mp := places[m.Root]
			if m.Root != c.Root && !mp.worktree() && mp.dir == cp.common && mp.prefix == cp.prefix {
				out[c.Root] = m.Root
				break
			}
		}
	}
	return out
}

// hostProjects is the registry as every host-wide view reads it: the newest row
// of each root nobody forgot, less the roots that are gone or hold no config,
// which it counts. parents maps each worktree among them to the project it is a
// worktree of; a view shows a child through its parent, never as a project.
func hostProjects(rows []projects.Row) (live []projects.Row, parents map[string]string, gone int) {
	for _, r := range projects.Latest(rows) {
		if stale(r.Root) {
			gone++
			continue
		}
		live = append(live, r)
	}
	return live, worktreeParents(live), gone
}

// underTemp is a root inside os.TempDir. Both sides are compared resolved as
// well as literal, because macOS hands out /var/folders and /tmp as symlinks
// into /private.
func underTemp(root string) bool {
	tmp := os.TempDir()
	for _, dir := range []string{filepath.Clean(tmp), resolved(tmp)} {
		for _, r := range []string{root, resolved(root)} {
			if rel, err := filepath.Rel(dir, r); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				return true
			}
		}
	}
	return false
}

// stale is a registered root that no longer holds a project: gone, or a tree
// that never had a config. A root whose config exists and will not load is not
// stale; it is a broken project and every host-wide view still shows it.
func stale(root string) bool {
	_, err := os.Stat(filepath.Join(root, config.FileName))
	return os.IsNotExist(err)
}

// forget appends a tombstone for root. The rows naming it stay, so the record
// that drover was pointed there survives.
func forget(path, root string) error {
	rows, err := projects.Read(path)
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(expandHome(root)); err == nil {
		root = abs
	}
	// The registry holds roots as config resolved them, so a path typed through
	// a symlink matches its target too.
	names := map[string]bool{root: true}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		names[r] = true
	}
	var last *projects.Row
	for i := range rows {
		if names[rows[i].Root] {
			last = &rows[i]
		}
	}
	if last == nil {
		return Refusal{fmt.Errorf("the registry (%s) never named %s: drover projects --all lists every root it holds", path, root)}
	}
	if last.Forgotten {
		fmt.Printf("%s is already forgotten\n", root)
		return nil
	}
	if err := projects.Append(path, projects.Row{
		Project: last.Project, Root: last.Root, Repo: last.Repo,
		Registered: time.Now().Format("2006-01-02"), Forgotten: true,
	}); err != nil {
		return err
	}
	fmt.Printf("forgot %s (%s): its rows stay in %s, and the next drover command run there registers it again\n",
		last.Project, last.Root, path)
	return nil
}

// ProjectRow is one registered project as `projects` reports it.
//
// Present is what the registry cannot know: it records where a project was, and
// only a filesystem read says whether the tree is still there. A row whose root
// is gone stays in the file — deleting it would erase the record that drover was
// pointed at that tree, which is the one thing an audit trail is for.
type ProjectRow struct {
	Project    string `json:"project"`
	Root       string `json:"root"`
	Repo       string `json:"repo,omitempty"`
	Registered string `json:"registered"`
	Present    bool   `json:"present"`
	// Forgotten appears only under --all, on a root whose newest row is a
	// tombstone.
	Forgotten bool `json:"forgotten,omitempty"`
}

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Every project drover has been pointed at on this machine.",
	Long: `An append-only record of the projects this host drives. Any project command
registers the project it runs in the first time it runs there, so a project
initialized before the registry existed appears after its next status, check or
drive; a repeat command appends nothing. drover init registers too. A command a
session drover launched runs (DROVER_ACTOR set) never registers: its root is a
drive worktree, and the operator's own command already recorded the project. A
root under the OS temp directory registers like any other and is listed here;
status --all-projects counts it as scratch rather than giving it a row. A git
worktree of a registered project records that project as its parent, and the
host-wide views show it through the parent.

Nothing behaves differently inside a project because of what is in it: it answers
"what is drover driving here" without depending on anybody remembering, gives a
vocabulary change a list of trees to migrate, and is the list status
--all-projects, drives --all and drive --pause --all read.

A root that no longer exists is marked and kept. Erasing it would erase the
record that drover was pointed at that tree, which is the fact the file holds.

--forget <root> takes a root off every list by appending a tombstone row
(forgotten: true and today's date). It appends rather than deletes: the earlier
rows stay, --all still shows the root marked forgotten, and the next command run
in a forgotten project that still exists registers it again. --forget with a root
the registry never named exits 3.

It needs no project, so it answers from anywhere.`,
	Example: `  drover projects                   # what this machine drives, newest registration per root
  drover projects --all             # forgotten roots too, marked
  drover projects --forget /tmp/repro   # appends a tombstone; the rows stay
  drover projects --json            # an array, one object per project:
    [{"project":"webapp","root":"/home/me/src/webapp",
      "repo":"me/webapp","registered":"2026-08-28","present":true}]

  present is false when the root is gone. The row stays: the registry records
  where drover was pointed, and a tree that moved still was. Under --all a
  forgotten root carries "forgotten": true.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		path := projects.Path(config.GlobalPath())
		if projectsForget != "" {
			if path == "" {
				return Refusal{fmt.Errorf("no registry: this machine has no home directory to keep one in")}
			}
			return forget(path, projectsForget)
		}
		rows, err := projects.Read(path)
		if err != nil {
			return err
		}
		latest := projects.Latest(rows)
		if projectsAll {
			latest = projects.Fold(rows)
		}

		out := make([]ProjectRow, 0, len(latest))
		for _, r := range latest {
			_, statErr := os.Stat(r.Root)
			out = append(out, ProjectRow{
				Project: r.Project, Root: r.Root, Repo: r.Repo,
				Registered: r.Registered, Present: statErr == nil, Forgotten: r.Forgotten,
			})
		}

		if projectsJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		}

		if len(out) == 0 {
			if path == "" {
				fmt.Println("no registry: this machine has no home directory to keep one in")
				return nil
			}
			fmt.Printf("no projects registered yet (%s)\n", path)
			fmt.Println("any drover command run in a project registers it")
			return nil
		}

		width := 0
		for _, r := range out {
			if n := len(r.Project); n > width {
				width = n
			}
		}
		var missing, forgotten int
		for _, r := range out {
			mark := " "
			switch {
			case r.Forgotten:
				mark = "-"
				forgotten++
			case !r.Present:
				mark = "!"
				missing++
			}
			repo := r.Repo
			if repo == "" {
				repo = "-"
			}
			fmt.Printf("%s %-*s  %-22s %s  %s\n", mark, width, r.Project, repo, r.Registered, r.Root)
		}
		fmt.Println()
		if forgotten > 0 {
			fmt.Printf("%d forgotten, marked -\n", forgotten)
		}
		if missing > 0 {
			// The noun agrees with the total and the verb with the count that
			// is missing, so "1 of 2 roots ... no longer exists" reads right and
			// so does "2 of 2 roots ... no longer exist".
			fmt.Printf("%d of %d %s registered here no longer %s\n",
				missing, len(out), plural(len(out), "root", "roots"),
				plural(missing, "exists", "exist"))
			return nil
		}
		fmt.Printf("%d %s\n", len(out), plural(len(out), "project", "projects"))
		return nil
	},
}

func init() {
	projectsCmd.Flags().BoolVar(&projectsJSON, "json", false, "emit rows as JSON")
	projectsCmd.Flags().BoolVar(&projectsAll, "all", false, "include forgotten roots, marked")
	projectsCmd.Flags().StringVar(&projectsForget, "forget", "", "append a tombstone for this `root`, taking it off every list")
	projectsCmd.MarkFlagsMutuallyExclusive("forget", "all")
	projectsCmd.MarkFlagsMutuallyExclusive("forget", "json")
	root.AddCommand(projectsCmd)
}
