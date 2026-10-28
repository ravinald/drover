// This file is the map of where a review finding surfaces, driven end to end.
//
// Seventeen channels can carry one, and nothing else in the tree drives a
// writer and a template together: every request-changes test reaches a helper
// or a ledger row, and sample() in prompts_test.go renders a body with no
// sent-back section in it. So a channel could be complete, unit-tested and
// restated across eight surfaces while arriving nowhere near the agent that had
// to act on it, with the verify command green throughout.
//
// Each row names one channel, the surface it must reach and the surface it must
// not, drives drover's own writer for it, and renders the built-in template
// through the command an operator runs. Assertions are on the payload — a file
// and line, a reproducing command, a level — because a heading renders whether
// or not anything arrived beneath it.
//
// A row with an absent column is the other half of the map: a channel
// deliberately not carried there. Carrying one later then costs a decision
// rather than happening as a side effect.
package prompts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/cmd"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/ravinald/drover/internal/runner"
)

// surface is a place a finding has to arrive, named by what renders it.
type surface string

const (
	runBrief    surface = "drover run B2 --dry-run"
	reviewBrief surface = "drover review B2"
	// runProse is the run brief minus the item body it quotes: what drover says
	// in its own voice. A pointer to a section of the body has to land here,
	// because the heading is inside the quoted body either way and an agent told
	// nothing about it is left inferring that a section it has never been named
	// is the one carrying the findings.
	runProse surface = "drover run B2 --dry-run, outside the quoted body"
)

// B2 is the item under test and B1 its one dependency, recorded merged. Two ids
// are the minimum that can tell a row this item owns from a row it inherited.
const (
	item = "B2"
	dep  = "B1"
)

// The register stanzas a row picks between. A project declaring none is not a
// project whose register failed, and the two must not render alike.
const (
	ghRegister = `
[github]
repo         = "owner/example"
follow_up_label   = "follow-up"
severity_prefix   = "severity:"
`
	fileRegister = `
[register]
backend = "file"
`
	noRegister = `
[register]
backend = "none"
`
)

// ghPage is what the stub answers `gh issue list` with: one row routed to the
// item, one routed to its dependency, each carrying the report that filed it.
const ghPage = `[` +
	`{"number":81,"title":"the walk skips a nil manifest","body":"Reproduced at internal/walk/walk.go:348 with ` + "`drover check --json`" + `: it answers 0 findings where 3 are open.\nOwner: B2","state":"OPEN",` +
	`"labels":[{"name":"follow-up"},{"name":"severity:high"}]},` +
	`{"number":90,"title":"the dependency left a gap","body":"Nobody on B2 should act on this evidence.\nOwner: B1","state":"OPEN",` +
	`"labels":[{"name":"follow-up"},{"name":"severity:medium"}]}]`

// changesEvidence is what a review writes back, in the shape the brief asks for:
// the file and line, the command that reproduces it, and the output it gave.
// Numbered, because that is how findings get written and the one shape the body
// cannot carry — request-changes demotes it, and the payload has to survive that.
const changesEvidence = `1. **The reason never reaches the prompt.** internal/cmd/run.go:210 has no else
   branch, so a register drover could not read renders as one with nothing in it.

   Reproduce: drover run B2 --dry-run | grep -c 'did not answer' answers 0.
`

// guarantee is what a pass establishes about the code rather than about one
// case: the shape that has to survive the next fix, and the one an item body
// had nowhere to keep before `drover invariant`.
const guarantee = `- Replacing an object preserves whatever mode an operator set on it.
`

// closedRound is a finding an earlier pass sent back and a later one closed.
// Its headline has to survive `request-changes` replacing the section; its body must not,
// because a closed finding rendered whole reads as an open one.
const closedRound = `1. **The cache hit is recorded against the configured upstream.** cache.go:41.

   Nobody may act on this round again: it closed two passes ago.
`

const auditNote = "Traced R1 to the writer; the template renders it nowhere."

// gitBin resolves before any test narrows PATH. stand() leaves only what a row
// put in the fixture's own bin, so that no row can reach a live tracker, and the
// one row that needs a real binary has to carry it in deliberately.
var gitBin, _ = exec.LookPath("git")

func TestAFindingReachesTheSurfaceThatMustActOnIt(t *testing.T) {
	for _, c := range []struct {
		channel string
		// why is the routing claim the row pins, not a restatement of it.
		why      string
		register string
		// setup drives the real writer for this channel.
		setup func(t *testing.T, dir string)
		// reach and absent are payloads, never headings.
		reach  map[surface][]string
		absent map[surface][]string
		// ledgerHandle asserts the ledger's own record of this channel never
		// stands in for the payload. The changes row carries a 12-hex digest by
		// design, and a brief handing that to an agent has handed it nothing.
		ledgerHandle bool
	}{{
		channel:  "sent-back section",
		why:      "the item body is the whole of what the next run reads, and request for changes is the only thing that writes there",
		register: noRegister,
		setup: func(t *testing.T, dir string) {
			path := filepath.Join(dir, "evidence.md")
			write(t, path, changesEvidence)
			drove(t, "request-changes", item, "-f", path)
		},
		reach: map[surface][]string{
			runBrief:    {"internal/cmd/run.go:210 has no else", "grep -c 'did not answer' answers 0"},
			reviewBrief: {"internal/cmd/run.go:210 has no else", "grep -c 'did not answer' answers 0"},
			runProse:    {prompts.ChangesHeading},
		},
		ledgerHandle: true,
	}, {
		channel:  "owned issue body, owned",
		why:      "RISK-TO-OWNER promises the owner's next run gets the defect, and a subject line is not one",
		register: ghRegister,
		setup:    func(t *testing.T, dir string) { stubGH(t, dir, ghPage) },
		reach: map[surface][]string{
			runBrief:    {"Reproduced at internal/walk/walk.go:348", "answers 0 findings where 3 are open"},
			reviewBrief: {"Reproduced at internal/walk/walk.go:348", "answers 0 findings where 3 are open"},
		},
	}, {
		channel:  "owned issue body, a dependency's",
		why:      "carried one hop so nobody re-files it, and deliberately a headline: discharging it is its owner's step",
		register: ghRegister,
		setup:    func(t *testing.T, dir string) { stubGH(t, dir, ghPage) },
		reach: map[surface][]string{
			runBrief:    {"#90", "owned by B1"},
			reviewBrief: {"#90", "owned by B1"},
		},
		absent: map[surface][]string{
			runBrief:    {"Nobody on B2 should act on this evidence"},
			reviewBrief: {"Nobody on B2 should act on this evidence"},
		},
	}, {
		channel:  "unreadable register",
		why:      "an empty follow-up list and a tracker nobody could reach render alike unless the reason rides along",
		register: fileRegister,
		setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "register.jsonl"),
				`{"number":1,"title":"fine","owners":["B2"],"state":"open","date":"2026-01-01"}`+"\n"+
					`{"number":2,`+"\n")
		},
		reach: map[surface][]string{
			runBrief:    {"register.jsonl:2", "is not a register row"},
			reviewBrief: {"register.jsonl:2", "is not a register row"},
		},
	}, {
		channel:  "a row from a register with no body column",
		why:      "a title with nothing under it reads as a filer who wrote nothing, which is not what a file register means",
		register: fileRegister,
		setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "register.jsonl"),
				`{"number":7,"title":"the summary undercounts","severity":"M","owners":["B2"],"state":"open","date":"2026-01-01"}`+"\n")
		},
		reach: map[surface][]string{
			runBrief:    {"carries no body"},
			reviewBrief: {"carries no body"},
		},
	}, {
		channel:  "a register no project declared",
		why:      "Off has failed nothing, and a prompt reporting an absence there reports a setting as a fault",
		register: noRegister,
		setup:    func(*testing.T, string) {},
		absent: map[surface][]string{
			runBrief:    {"could not be read", "did not answer"},
			reviewBrief: {"could not be read", "did not answer"},
		},
	}, {
		channel:  "dependency audit level",
		why:      "a zero Weak on a field nothing populated reads as a foundation somebody independently confirmed",
		register: noRegister,
		setup: func(t *testing.T, _ string) {
			drove(t, "audit", dep, "claimed", "--code-by", "sam", "--reviewer", "sam", "read my own diff")
		},
		reach: map[surface][]string{
			runBrief:    {"audited claimed by its own author"},
			reviewBrief: {"audited claimed by its own author"},
		},
	}, {
		channel:  "this item's own audit note",
		why:      "a reviewer reads the last note; the next run does not, which is why a finding has to be sent back as well as audited",
		register: noRegister,
		setup: func(t *testing.T, _ string) {
			drove(t, "audit", item, "reviewed", "--code-by", "agent", auditNote)
		},
		reach:  map[surface][]string{reviewBrief: {auditNote}},
		absent: map[surface][]string{runBrief: {auditNote}},
	}, {
		channel:  "the rounds before this one",
		why:      "request for changes replaces the section, so without the trail nothing says whether five rounds closed five defects or circled one",
		register: noRegister,
		setup: func(t *testing.T, dir string) {
			first := filepath.Join(dir, "round1.md")
			write(t, first, closedRound)
			drove(t, "request-changes", item, "-f", first)
			second := filepath.Join(dir, "round2.md")
			write(t, second, changesEvidence)
			drove(t, "request-changes", item, "-f", second)
		},
		reach: map[surface][]string{
			runProse:    {"The cache hit is recorded against the configured upstream. cache.go:41.", "changes-1.md"},
			reviewBrief: {"The cache hit is recorded against the configured upstream. cache.go:41.", "changes-1.md"},
		},
		absent: map[surface][]string{
			runBrief:    {"Nobody may act on this round again"},
			reviewBrief: {"Nobody may act on this round again"},
		},
	}, {
		channel:  "a finding the previous round sent back",
		why:      "only the reviewer can say a finding carried over, and drive parks repeating on that word alone",
		register: noRegister,
		setup: func(t *testing.T, dir string) {
			path := filepath.Join(dir, "evidence.md")
			write(t, path, changesEvidence)
			drove(t, "request-changes", item, "-f", path)
		},
		reach: map[surface][]string{
			reviewBrief: {"drover request-changes B2 -f <file> --still-open N[,N...]",
				"Only for the same finding, reproduced again. A new defect under a requirement"},
		},
		absent: map[surface][]string{runBrief: {"--still-open"}},
	}, {
		channel:  "source files the contract never named",
		why:      "an item about one storage file whose branch rewrote three backends read exactly like one that stayed put",
		register: noRegister,
		setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "work", "B02-item.md"),
				"---\ntitle: The item under review\ndeps: B1\n---\n\n"+
					"The row is written in `internal/store/local.go`.\n\n"+
					"Requirements:\n\n1. Record the upstream in `internal/store/local.go`.\n")
			gitStand(t, dir)
			drove(t, "audit", item, "reviewed", "--code-by", "agent", auditNote)
			drove(t, "audit", item, "reviewed", "--code-by", "agent", "traced it again")
		},
		reach: map[surface][]string{
			reviewBrief: {"modified 4 source files and the item body names 1",
				"internal/store/s3.go", "internal/server/proxy.go"},
		},
		absent: map[surface][]string{
			// The test file is not the contract's to name, and the run brief is
			// not where a scope decision gets made: routing is the reviewer's.
			reviewBrief: {"local_test.go"},
			runBrief:    {"still one item", "internal/store/s3.go"},
		},
	}, {
		channel:  "a guarantee an earlier pass established",
		why:      "requirements are frozen and findings are replaced, so a property learned on round three had nowhere to live",
		register: noRegister,
		setup: func(t *testing.T, dir string) {
			path := filepath.Join(dir, "guarantee.md")
			write(t, path, guarantee)
			drove(t, "invariant", item, "-f", path)
			evidence := filepath.Join(dir, "evidence.md")
			write(t, evidence, changesEvidence)
			drove(t, "request-changes", item, "-f", evidence)
		},
		reach: map[surface][]string{
			runBrief:    {"preserves whatever mode an operator set"},
			reviewBrief: {"preserves whatever mode an operator set"},
			runProse:    {prompts.GuaranteeHeading},
		},
	}, {
		channel:  "how many passes this item has had",
		why:      "the agent choosing how wide to look is the implementing one, and the counts reached the reviewer alone",
		register: noRegister,
		setup: func(t *testing.T, dir string) {
			path := filepath.Join(dir, "evidence.md")
			write(t, path, changesEvidence)
			drove(t, "request-changes", item, "-f", path)
			drove(t, "audit", item, "reviewed", "--code-by", "agent", auditNote)
			drove(t, "audit", item, "reviewed", "--code-by", "agent", "traced it again")
		},
		reach: map[surface][]string{
			runProse:    {"sent this item back 1 time before this run", "what has to stay true for the whole operation"},
			reviewBrief: {"1 pass that sent work back"},
		},
	}} {
		t.Run(c.channel, func(t *testing.T) {
			dir := stand(t, c.register)
			c.setup(t, dir)

			run := drove(t, "run", item, "--dry-run")
			_, prose, cut := strings.Cut(run, "--- END "+item+" ---")
			if !cut {
				t.Fatalf("the run prompt no longer fences the item body, so nothing can tell drover's prose from it:\n%s", run)
			}
			rendered := map[surface]string{
				runBrief:    run,
				runProse:    prose,
				reviewBrief: drove(t, "review", item),
			}
			for where, wants := range c.reach {
				for _, want := range wants {
					if !strings.Contains(rendered[where], want) {
						t.Errorf("%s does not carry %q — %s\n---\n%s", where, want, c.why, rendered[where])
					}
				}
			}
			for where, nots := range c.absent {
				for _, not := range nots {
					if strings.Contains(rendered[where], not) {
						t.Errorf("%s carries %q, which is not its to carry — %s\n---\n%s", where, not, c.why, rendered[where])
					}
				}
			}
			if c.ledgerHandle {
				handle := requestChangesHandle(t, dir)
				for where, text := range rendered {
					if strings.Contains(text, handle) {
						t.Errorf("%s carries the ledger's handle %q where the evidence belongs — %s", where, handle, c.why)
					}
				}
			}
		})
	}
}

// gitStand turns the fixture into a real git repository on this item's branch,
// with one file the body names modified beside three it does not. The scope
// channel is the only one that reads the tree rather than the ledger or the
// backlog, so it is the only row that cannot be driven from files alone.
func gitStand(t *testing.T, dir string) {
	t.Helper()
	cfg := filepath.Join(dir, ".drover.toml")
	raw, err := os.ReadFile(cfg) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, strings.Replace(string(raw), "enabled = false", "enabled = true", 1))

	if gitBin == "" {
		t.Skip("no git on PATH, so the tree half of this channel cannot be driven")
	}
	if err := os.Symlink(gitBin, filepath.Join(dir, "bin", "git")); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // #nosec G204
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	touch := func(v string) {
		for _, f := range []string{"internal/store/local.go", "internal/store/s3.go",
			"internal/store/memory.go", "internal/server/proxy.go", "internal/store/local_test.go"} {
			write(t, filepath.Join(dir, f), "package x // "+v+"\n")
		}
	}
	git("init", "-q", "-b", "main", ".")
	git("config", "user.email", "t@e")
	git("config", "user.name", "t")
	touch("v1")
	git("add", "-A")
	git("commit", "-qm", "init")
	git("checkout", "-qb", "b02-item")
	touch("v2")
	git("commit", "-qam", "widen")
}

// stand writes a project with one item, one merged dependency, and the register
// the row asked for, then stands in it.
//
// Every environment variable an answer depends on is set here. XDG_CACHE_HOME
// keys the log directory on the project name alone, so a stray run log under
// the developer's own cache would decide whether a brief reports a self-report;
// PATH holds only what a row put there, so no row can reach a live tracker; and
// drover exports the actor variable into every session it launches, which seals
// commands against a value no test wrote.
func stand(t *testing.T, register string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv(runner.EnvActor, "")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(dir, "bin"))

	write(t, filepath.Join(dir, ".drover.toml"), `schema_version = 2
project = "surface"

[paths]
backlog  = "work"
ledger   = "ledger.jsonl"
reports = "reports"
register = "register.jsonl"

[ids]
namespaces = { B = "bug" }

[frontmatter]
required = ["title", "deps"]
optional = ["kind", "severity"]

[verify]
default = "true"

[git]
enabled = false
`+register)
	write(t, filepath.Join(dir, "work", "B01-dep.md"), "---\ntitle: The dependency\ndeps:\n---\n\nRequirements:\n\n1. Build the foundation.\n")
	write(t, filepath.Join(dir, "work", "B02-item.md"), "---\ntitle: The item under review\ndeps: B1\n---\n\nRequirements:\n\n1. Do the thing.\n")
	write(t, filepath.Join(dir, "ledger.jsonl"), `{"id":"B1","merged":true,"implementer":"upstream","date":"2026-01-01"}`+"\n")
	t.Chdir(dir)
	return dir
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// drove runs one command through the same root an operator's argv reaches, and
// answers with what it printed. Driving the command rather than the assembler
// is the point: a payload that reaches prompts.Data and no template is a
// payload that reaches nobody.
func drove(t *testing.T, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path) // #nosec G304 -- the path is this test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	stdout, argv := os.Stdout, os.Args
	os.Stdout, os.Args = f, append([]string{"drover"}, args...)
	err = cmd.Execute()
	os.Stdout, os.Args = stdout, argv
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("drover %s: %v", strings.Join(args, " "), err)
	}
	out, err := os.ReadFile(path) // #nosec G304 -- as above
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// stubGH answers the two calls a register read makes and fails anything else,
// so a call these rows do not model shows up here rather than on the network.
func stubGH(t *testing.T, dir, page string) {
	t.Helper()
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'issue list') printf '%s' '" + page + "' ;;\n" +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "bin", "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
}

// requestChangesHandle reads back what the ledger's changes row recorded: the digest,
// which identifies the evidence without carrying any of it.
func requestChangesHandle(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl")) // #nosec G304 -- the fixture's own ledger
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row ledger.Row
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("ledger line %q: %v", line, err)
		}
		if row.Changes != "" {
			return row.Changes
		}
	}
	t.Fatal("request for changes wrote no ledger row, so this row cannot pin what the prompt must not carry")
	return ""
}
