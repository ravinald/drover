package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	skillsFrom  string
	skillsPrune bool
	skillsJSON  bool
)

// SkillUse is one guide and the items that reference it. State is the same word
// the table prints, so a wrapper and a reader agree on what is wrong: vendored,
// missing, or unreferenced.
type SkillUse struct {
	Name     string   `json:"name"`
	Refs     []string `json:"refs"`
	Vendored bool     `json:"vendored"`
	State    string   `json:"state"`
}

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Which guides the backlog references, and which are vendored.",
	Long: `Vendoring exists because a sandboxed run cannot reach a guide outside the repo.
That makes drift expensive in one direction and merely wasteful in the other: a
referenced guide that is missing degrades to a suggestion an unattended run
cannot act on, while an unreferenced one is dead weight nobody notices.

--from <dir> copies every referenced guide out of a source tree, which turns
vendoring into something reproducible rather than a one-time copy.`,
	Example: `  drover skills                     # what the backlog references, and what is vendored
  drover skills --from ~/.claude/skills   # copy every referenced guide into {skills}
  drover skills --prune             # delete vendored guides no item references
  drover skills --json              # an array, one object per guide:
    [{"name":"golang-testing","refs":["3","B1"],"vendored":true,"state":"vendored"}]

  state is the same word the table prints: vendored, missing, or unreferenced.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		// An unset [paths] skills is degraded rather than fatal, which is what
		// every other optional path does: what the backlog references is still
		// answerable, and the answer is that none of it can be vendored. Exiting
		// 1 here made the one command that reports a gap unusable on the project
		// most likely to have one.
		dir := s.cfg.Abs(s.cfg.Paths.Skills)
		uses := s.skillUses(dir)

		if skillsFrom != "" {
			if dir == "" {
				return Refusal{fmt.Errorf(
					"--from has nowhere to copy to: this project declares no [paths] skills")}
			}
			if err := syncSkills(uses, skillsFrom, dir); err != nil {
				return err
			}
			uses = s.skillUses(dir)
		}

		var missing, unreferenced []string
		for i := range uses {
			switch {
			case len(uses[i].Refs) == 0:
				uses[i].State = "unreferenced"
				unreferenced = append(unreferenced, uses[i].Name)
			case !uses[i].Vendored:
				uses[i].State = "missing"
				missing = append(missing, uses[i].Name)
			default:
				uses[i].State = "vendored"
			}
		}

		// Pruning happens before either output path, so --json and the table
		// delete the same guides. Ordering the removal after the JSON return
		// made --json --prune a silent no-op while --json --from still copied,
		// which is one flag pair behaving two ways with nothing documenting it.
		//
		// One stderr line carries the whole report, what was eligible and that
		// it went, because --json promises stdout is the array alone. Split
		// across the streams, a captured stdout recorded the plan and read
		// identically to a run that never reached the removal.
		pruned := skillsPrune && dir != "" && len(unreferenced) > 0
		if pruned {
			for _, name := range unreferenced {
				if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
					return err
				}
			}
			fmt.Fprintf(errOut, "pruned %d vendored but unreferenced: %s\n",
				len(unreferenced), strings.Join(unreferenced, " "))
		}

		if skillsJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(uses)
		}

		fmt.Printf("%-32s %5s  %s\n", "guide", "refs", "state")
		for _, u := range uses {
			state := u.State
			if state == "missing" {
				state = "MISSING — an unattended run cannot read it"
			}
			if state == "unreferenced" {
				state = "UNREFERENCED"
			}
			fmt.Printf("%-32s %5d  %s\n", u.Name, len(u.Refs), state)
		}

		fmt.Println()
		if len(missing) > 0 {
			fmt.Printf("%d referenced but not vendored: %s\n", len(missing), strings.Join(missing, " "))
		}
		// Drift the operator has not acted on is part of the table, so it stays
		// on stdout; once --prune acts, the same names are a side-effect report
		// and stderr above already carries them.
		if len(unreferenced) > 0 && !pruned {
			fmt.Printf("%d vendored but unreferenced: %s\n", len(unreferenced), strings.Join(unreferenced, " "))
			fmt.Println("remove them with --prune")
		}
		if dir == "" {
			fmt.Println("no [paths] skills, so nothing is vendored and nothing can be")
		} else if len(missing) == 0 && len(unreferenced) == 0 {
			fmt.Println("backlog and vendored guides agree")
		}
		return nil
	},
}

// skillUses joins what the backlog references against what is on disk.
func (s *session) skillUses(dir string) []SkillUse {
	byName := map[string]*SkillUse{}
	for _, it := range s.backlog.Items {
		for _, name := range strings.Fields(it.Get("skills")) {
			u, ok := byName[name]
			if !ok {
				u = &SkillUse{Name: name}
				byName[name] = u
			}
			u.Refs = append(u.Refs, it.ID.Bare())
		}
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, ok := byName[e.Name()]; !ok {
				byName[e.Name()] = &SkillUse{Name: e.Name()}
			}
		}
	}
	for name, u := range byName {
		if st, err := os.Stat(filepath.Join(dir, name, "SKILL.md")); err == nil && !st.IsDir() {
			u.Vendored = true
		}
	}

	out := make([]SkillUse, 0, len(byName))
	for _, u := range byName {
		if u.Refs == nil {
			u.Refs = []string{}
		}
		out = append(out, *u)
	}
	// Most-referenced first: it is the order that answers "which of these
	// actually carry the backlog".
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Refs) != len(out[j].Refs) {
			return len(out[i].Refs) > len(out[j].Refs)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// syncSkills copies every referenced guide from src into dst.
func syncSkills(uses []SkillUse, src, dst string) error {
	var copied, absent []string
	for _, u := range uses {
		if len(u.Refs) == 0 {
			continue
		}
		from := filepath.Join(src, u.Name)
		if st, err := os.Stat(filepath.Join(from, "SKILL.md")); err != nil || st.IsDir() {
			absent = append(absent, u.Name)
			continue
		}
		if err := copyTree(from, filepath.Join(dst, u.Name)); err != nil {
			return fmt.Errorf("vendor %s: %w", u.Name, err)
		}
		copied = append(copied, u.Name)
	}
	// stderr, not stdout: --json promises the stream is an array, and an
	// operator wants to know what was vendored in both moods.
	fmt.Fprintf(errOut, "vendored %d %s from %s\n", len(copied), plural(len(copied), "guide", "guides"), src)
	if len(absent) > 0 {
		// Named rather than counted: a guide the source does not have is the one
		// a run will silently do without.
		fmt.Fprintf(errOut, "not found in the source: %s\n", strings.Join(absent, " "))
	}
	fmt.Fprintln(errOut)
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- path comes from a walk of an operator-named directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func init() {
	skillsCmd.Flags().StringVar(&skillsFrom, "from", "", "copy referenced guides out of this directory before reporting")
	skillsCmd.Flags().BoolVar(&skillsPrune, "prune", false, "delete vendored guides no item references")
	skillsCmd.Flags().BoolVar(&skillsJSON, "json", false, "emit rows as JSON")
	root.AddCommand(skillsCmd)
}
