package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ravinald/drover/internal/config"
	"github.com/spf13/cobra"
)

var configDiff, configStamp bool

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Print the .drover.toml this binary would write for this project today.",
	Long: `A .drover.toml is written once, by init, against whatever this build
understood that day. Nothing afterwards tells a project the binary has moved: a
key added since gets its zero value, a table added since is absent, and both
read exactly like a project that declared them and chose the default. This is
the command that says which.

--diff is the form to use. It prints only what differs — the keys this build
reads that no config declares, the keys a config declares that nothing here
reads, and the schema_version gap — one paste-ready line each. Without it the
whole file is printed, built from the same generator ` + "`drover init`" + ` uses and
carrying this project's own values, so what you read back is today's layout with
your decisions in it rather than what a fresh project gets.

--stamp records that you have reviewed the config against this release, by
writing drover_version and nothing else. Every project command refuses while
that stamp is missing or names an older minor release, because a key added
since reads as its zero value and nothing else says it was never decided. Run
--diff first; the stamp is the claim that you read what it printed.

It never rewrites the file, and there is no --write. A .drover.toml carries hand-written
comments recording why each value is what it is, and no TOML round-trip
preserves them: a regenerating writer would flatten the more valuable half of
the file. The generated block in AGENTS.md has no such problem because markers
fence it off and nobody writes prose inside them, and a config has no markers to
fence. So this prints, and a human pastes.

It judges nothing either. A key reported as undeclared is not a key this project
ought to set — most projects are right to leave most of them alone, and a
default drover picked is the default it will keep picking. The report says what
you have not decided, not what you decided wrongly.

It reads the config without validating it, which is the whole point: an older
schema_version and every key a rename left behind are errors at load, so a
command that went through the ordinary door would refuse exactly the configs
worth asking about. Nothing here checks a root marker, a severity rank or a
register backend; ` + "`drover check`" + ` does that.`,
	Example: `  drover config --diff   # what this binary would write differently
  drover config --stamp  # after reading --diff: drover_version = this release
  drover config          # the whole file, with this project's values, then a
                         # commented notice per key this build no longer reads,
                         # then every supervised action's effective policy

  Paste each reported line under the table named above it. TOML refuses a
  second [table] header, so piping the whole report onto a config that already
  declares that table writes a file drover cannot load.

  --diff exits 0 either way: it reports, and reporting nothing is an answer.
  drover check reports the same gap as STALE CONFIG and exits 2, which is the
  form for CI.

  A key the per-user config supplies is never reported as missing. Repeating it
  in a project file shadows the global value rather than inheriting it, and the
  next edit to that file would then silently miss this project.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		in, err := config.Inspect(wd)
		if err != nil {
			return err
		}
		if configStamp {
			stamp, ok := releaseStamp()
			if !ok {
				return Refusal{fmt.Errorf("this build is %q and carries no release version to stamp: "+
					"build it with make install, or install a release", version)}
			}
			if err := stampConfig(in.Path, stamp); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "stamped %s: drover_version = %q\n", rel(in.Root, in.Path), stamp)
			return nil
		}
		if configDiff {
			renderConfigDiff(os.Stdout, in)
			fmt.Print(addendumNote(in.Config))
			return nil
		}
		global, err := globalKeys()
		if err != nil {
			return err
		}
		body := renderConfig(forReport, in.Root, global, in.Config.Verify["default"], written(in.Written()), in.Bare)
		fmt.Print(body + notices(body, in) + addendumNote(in.Config) + configPolicies(in.Config))
		return nil
	},
}

// renderConfigDiff writes the ways a file and this build can disagree.
// Separate from RunE so a test drives the real output rather than a second copy
// of this format.
//
// The undeclared keys come in two sections because they are two different
// standards, and one list under one caveat would state both wrongly. What
// `drover init` writes today is what a config written today would carry, and
// STALE CONFIG counts exactly that set. Everything else this build reads is a
// default nobody has to decide, and reporting it as a gap would contradict the
// caveat printed over it.
func renderConfigDiff(w io.Writer, in *config.Inspection) {
	generated, err := undeclared(in.Root, in.Config.Verify["default"], in.Declared)
	if err != nil {
		fmt.Fprintf(w, "# drover could not render this project's layout: %v\n", err)
	}
	// schema_version reaches these lists only when no file declares it, which
	// is exactly when the stanza below prints it. Reported in both, one key
	// reads as two gaps and the reader pastes the line twice.
	version := []config.Key{{Dotted: "schema_version"}}
	generated = except(generated, version)
	rest := except(except(in.Missing(), generated), version)

	if !in.Stale() && len(generated) == 0 && len(rest) == 0 && len(in.Retired) == 0 {
		fmt.Fprintf(w, "ok — %s declares every key this build reads, and nothing it does not\n",
			rel(in.Root, in.Path))
		return
	}

	if in.Stale() {
		declares := fmt.Sprintf("the file declares %d", in.Version)
		if !in.VersionDeclared() {
			declares = "the file declares none"
			// Absence is the project's to fix whatever the per-user file says,
			// and an inherited mismatch is named where it was written.
			if src, v := in.VersionSource(); src != in.Path {
				declares += fmt.Sprintf(", and %s declares %d", src, v)
			}
		}
		fmt.Fprintf(w, "schema_version = %d   # %s\n\n",
			config.SchemaVersion, declares)
		fmt.Fprintf(w, "# Bumping the line is not the migration. Load names the renames that made\n")
		fmt.Fprintf(w, "# this version old, and the retired keys below are what they left behind.\n\n")
	}

	if len(generated) > 0 {
		fmt.Fprintf(w, "# %d %s `drover init` writes today that this config does not declare.\n",
			len(generated), plural(len(generated), "key", "keys"))
		fmt.Fprintf(w, "# This is what `drover check` reports as STALE CONFIG.\n")
		pasteNote(w)
		writeKeys(w, generated)
	}

	if len(rest) > 0 {
		fmt.Fprintf(w, "# %d further %s this build reads, at the value drover resolves each to.\n",
			len(rest), plural(len(rest), "key", "keys"))
		fmt.Fprintf(w, "# Nothing reports these: a default left alone is not a gap. They are here\n")
		fmt.Fprintf(w, "# because a key you do not know about is one you cannot go looking for.\n")
		pasteNote(w)
		writeKeys(w, rest)
	}

	if len(in.Retired) > 0 {
		fmt.Fprintf(w, "# %d %s nothing in this build reads. Whoever wrote the file named beside\n",
			len(in.Retired), plural(len(in.Retired), "key", "keys"))
		fmt.Fprintf(w, "# each chose that value, so it is named rather than dropped; remove it from\n")
		fmt.Fprintf(w, "# that file. Commented, because pasting this block must not put it back.\n")
		for _, r := range in.Retired {
			fmt.Fprintf(w, "# %s   (%s)\n", r.Key, rel(in.Root, r.Path))
		}
	}
}

// addendumNote says where [agents] addendum resolves and whether it reads,
// which the printed value cannot: ~/ and a relative path both name a file
// somewhere the reader has to work out. Commented, so the report still pastes.
func addendumNote(cfg *config.Config) string {
	path := cfg.AddendumPath()
	if path == "" {
		return ""
	}
	if _, err := (&session{cfg: cfg}).readAddendum(); err != nil {
		return fmt.Sprintf("\n# [agents] addendum resolves to %s, which cannot be read: %v\n", path, errors.Unwrap(err))
	}
	return fmt.Sprintf("\n# [agents] addendum resolves to %s, readable\n", path)
}

// pasteNote says how a line gets into the file. TOML refuses a second [table]
// header, so the stanzas below are a destination rather than a block to append:
// a reader who pipes the whole report onto a config that already declares
// [paths] gets a file that will not parse.
func pasteNote(w io.Writer) {
	fmt.Fprintf(w, "#\n")
	fmt.Fprintf(w, "# Each line goes under the table named above it. TOML refuses a second\n")
	fmt.Fprintf(w, "# [table] header, so a whole stanza appends only where the file has no\n")
	fmt.Fprintf(w, "# such table yet.\n")
}

// writeKeys groups one section by table. Top-level keys arrive first from
// Missing and undeclared alike, because a bare key written after a [table]
// header belongs to that header.
func writeKeys(w io.Writer, keys []config.Key) {
	table := "\x00" // No table can be named this, so the first key always writes a header.
	for _, k := range keys {
		if k.Table != table {
			table = k.Table
			if table == "" {
				fmt.Fprintln(w)
			} else {
				fmt.Fprintf(w, "\n[%s]\n", table)
			}
		}
		fmt.Fprintf(w, "%s = %s\n", k.Name, k.Default)
	}
	fmt.Fprintln(w)
}

// except drops from keys everything the other list already reported, so no key
// appears in two sections under two different standards.
func except(keys, reported []config.Key) []config.Key {
	seen := make(map[string]bool, len(reported))
	for _, k := range reported {
		seen[k.Dotted] = true
	}
	var out []config.Key
	for _, k := range keys {
		if !seen[k.Dotted] {
			out = append(out, k)
		}
	}
	return out
}

func init() {
	configCmd.Flags().BoolVar(&configDiff, "diff", false,
		"print only what differs, one paste-ready line per key")
	configCmd.Flags().BoolVar(&configStamp, "stamp", false,
		"write drover_version = this release into .drover.toml, changing no other line")
	root.AddCommand(configCmd)
}
