package cmd

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ravinald/drover/internal/config"
)

// mergeFlagKeys are the two merge actions --permissive-merge and
// --restrictive-merge set together.
var mergeFlagKeys = []string{"merge_drive_pr", "merge_protected_paths"}

// mergeFlag applies --permissive-merge or --restrictive-merge to the session's
// config, so the block renders under the policy it is about to record, and
// returns the write that records it in .drover.toml, or nil when the file
// already says it. The write is returned rather than made so that a widening
// refused afterwards leaves the file as it was.
func (s *session) mergeFlag(permissive, restrictive bool, target string) (func() error, error) {
	if !permissive && !restrictive {
		return nil, nil
	}
	if permissive && restrictive {
		return nil, Refusal{fmt.Errorf("--permissive-merge and --restrictive-merge set the same two keys opposite ways: pass one")}
	}
	if target == "" {
		return nil, Refusal{fmt.Errorf(
			"--permissive-merge and --restrictive-merge record the policy in %s and render it into a file: name the file with --write",
			config.FileName)}
	}
	if err := operatorStep("agents --write with a merge flag",
		"it decides what a supervising session may merge, so a session it would bound cannot be the one choosing"); err != nil {
		return nil, err
	}
	want, yes := polOperator, ""
	if permissive {
		want, yes = polSupervisor, " --yes"
	}
	path := filepath.Join(s.cfg.Root, config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the project file Load resolved
	if err != nil {
		return nil, err
	}
	declared, err := config.KeysIn(string(raw))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	exists := slices.ContainsFunc(declared, func(k string) bool { return k == "supervise" || strings.HasPrefix(k, "supervise.") })
	if exists {
		in, err := config.Inspect(s.cfg.Root)
		if err != nil {
			return nil, err
		}
		pinned := true
		var lines strings.Builder
		for _, k := range mergeFlagKeys {
			if p, ok := in.Policy(k); !ok || p != want {
				pinned = false
			}
			fmt.Fprintf(&lines, "%-16s = %q\n", k, string(want))
		}
		if !pinned {
			fmt.Printf("%s already has a [supervise] table, and drover does not rewrite it.\n"+
				"Set these two lines in it:\n\n%s", config.FileName, lines.String())
			return nil, Refusal{fmt.Errorf(
				"%s was not written, so its block cannot claim a policy the config does not hold.\n"+
					"Set the lines above, then run: drover agents --write %s%s", target, target, yes)}
		}
	}
	if s.cfg.Supervise == nil {
		s.cfg.Supervise = map[string]any{}
	}
	chosen := map[string]config.Policy{}
	for _, k := range mergeFlagKeys {
		s.cfg.Supervise[k] = string(want)
		chosen[k] = want
	}
	if exists {
		return nil, nil
	}
	// The prior pins each key, so a restrictive operator is written live: left
	// commented as the default, a per-user supervisor would still reach it.
	table := superviseTable(chosen, superviseState{policies: chosen})
	return func() error { return appendTable(path, raw, table, os.Stderr) }, nil
}

// confirmPolicy compares the policy the block in path records with the one
// about to be rendered, prints each action that moves, and refuses a widening
// nobody confirmed. A file with no block has told no session anything, so it
// has nothing to widen.
func (s *session) confirmPolicy(path string, yes bool) error {
	existing, err := os.ReadFile(s.cfg.Abs(path)) // #nosec G304 -- operator-named path
	if err != nil {
		return nil
	}
	span := findBlock(string(existing))
	if !span.Found || span.Malformed {
		return nil
	}
	changes := policyChanges(recordedPolicy(span.between(string(existing))), effectivePolicy(s.cfg))
	if len(changes) == 0 {
		return nil
	}
	fmt.Fprintf(errOut, "the block in %s changes policy:\n", path)
	widens := 0
	for _, c := range changes {
		fmt.Fprintf(errOut, "  %s\n", c)
		if c.widens() {
			widens++
		}
	}
	if widens == 0 || yes {
		return nil
	}
	if interactive() && confirm(newPrompter(os.Stdin, os.Stderr), fmt.Sprintf("Widen %d action(s) in %s?", widens, path)) {
		return nil
	}
	return Refusal{fmt.Errorf(
		"%d action(s) would widen what the sessions reading %s are told they may do, and nobody confirmed it; nothing was written.\n"+
			"Read the changes above, then run it again with --yes", widens, path)}
}

// confirm asks a yes-or-no question whose default is no.
func confirm(p *prompter, question string) bool {
	switch strings.ToLower(p.ask(question+" (y/N)", "n")) {
	case "y", "yes":
		return true
	}
	return false
}

// policyLinePrefix opens the line the block records its policy on. The line is
// what a later --write and the stale check compare, so a wording change to the
// template moves the block's text without moving its policy.
const policyLinePrefix = "<!-- drover policy: "

// policyLine is every catalog action at cfg's effective policy, sorted by key.
func policyLine(cfg *config.Config) string {
	pol := effectivePolicy(cfg)
	keys := slices.Sorted(maps.Keys(pol))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+string(pol[k]))
	}
	return policyLinePrefix + strings.Join(parts, " ") + " -->"
}

// effectivePolicy is every catalog action at cfg's effective policy.
func effectivePolicy(cfg *config.Config) map[string]config.Policy {
	out := make(map[string]config.Policy, len(catalog))
	for _, a := range catalog {
		out[a.key], _ = cfg.Policy(a.key)
	}
	return out
}

// recordedPolicy reads the policy a block was rendered under. A block with no
// policy line reads as the shipped defaults, as does any action the line does
// not name: a block that never named an action told nobody they could take it.
func recordedPolicy(block string) map[string]config.Policy {
	out := make(map[string]config.Policy, len(catalog))
	for _, a := range catalog {
		out[a.key] = a.def
	}
	for line := range strings.SplitSeq(block, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), policyLinePrefix)
		if !ok {
			continue
		}
		for field := range strings.FieldsSeq(strings.TrimSuffix(rest, "-->")) {
			k, v, ok := strings.Cut(field, "=")
			if _, known := out[k]; ok && known {
				out[k] = config.Policy(v)
			}
		}
		break
	}
	return out
}

// policyChange is one action whose policy moved between two renders.
type policyChange struct {
	Key      string
	Old, New config.Policy
}

// widens reports a move toward auto: further from the operator.
func (c policyChange) widens() bool { return c.New.Above(c.Old) }

func (c policyChange) String() string {
	way := "narrows"
	if c.widens() {
		way = "widens"
	}
	return fmt.Sprintf("%s %s: %s > %s", c.Key, way, c.Old, c.New)
}

// policyChanges is every action whose policy differs between from and to,
// sorted by key.
func policyChanges(from, to map[string]config.Policy) []policyChange {
	var out []policyChange
	for _, k := range slices.Sorted(maps.Keys(to)) {
		if from[k] != to[k] {
			out = append(out, policyChange{Key: k, Old: from[k], New: to[k]})
		}
	}
	return out
}

// stalePolicy is the STALE POLICY finding for one host block, or false when
// the block records the policy cfg holds. It is reported apart from a stale
// block because the sessions reading that file are being told permissions
// drover no longer enforces, which a wording change never does.
func stalePolicy(cfg *config.Config, name, block string) (staleFile, bool) {
	changes := policyChanges(recordedPolicy(block), effectivePolicy(cfg))
	if len(changes) == 0 {
		return staleFile{}, false
	}
	parts := make([]string, len(changes))
	fix := "drover agents --write " + name
	for i, c := range changes {
		parts[i] = c.String()
		if c.widens() {
			fix = "drover agents --write " + name + " --yes"
		}
	}
	return staleFile{Name: name, Kind: "STALE POLICY", Fix: fix,
		Why: "it tells the sessions reading it a policy the config no longer holds: " + strings.Join(parts, "; ")}, true
}
