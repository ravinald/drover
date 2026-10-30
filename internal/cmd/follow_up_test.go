package cmd

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/register"
	"github.com/ravinald/drover/internal/sev"
)

// fileRegisterProject is a project whose register is the file backend, with the
// given rows already in it at the default [paths] register.
//
// It carries one item, because `follow-up open --owner` refuses an id no body
// answers to and a fixture with an empty backlog can route a row nowhere.
func fileRegisterProject(t *testing.T, extra string, rows ...string) string {
	t.Helper()
	dir := registerProject(t, "[register]\nbackend = \"file\"\n"+extra)
	if err := os.WriteFile(filepath.Join(dir, "work", "B01-a-bug.md"),
		[]byte(body("a bug", "[]")), 0o600); err != nil {
		t.Fatal(err)
	}
	rowText := ""
	for _, r := range rows {
		rowText += r + "\n"
	}
	path := filepath.Join(dir, ".drover", "register.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rowText), 0o600); err != nil {
		t.Fatal(err)
	}
	// PATH carries no gh at all: a read that reached the tracker would fail
	// here rather than quietly answering from it.
	stubPATH(t)
	return dir
}

const (
	fileRisk = `{"number":1,"title":"contract checks assert shape","severity":"M",` +
		`"owners":["B1"],"state":"open","date":"2026-01-01"}`
	fileSecond = `{"number":2,"title":"resolver drops the trailing dot","severity":"H",` +
		`"owners":["B1"],"state":"open","date":"2026-01-02"}`
	fileClosedOpen  = `{"number":3,"title":"gh output shape is unpinned","state":"open","date":"2026-01-03"}`
	fileClosedAgain = `{"number":3,"title":"gh output shape is unpinned","state":"closed","date":"2026-01-04"}`
)

// The read path end to end: one command in both moods, answering from a file
// instead of gh. Asserting the printed lines rather than the helpers that build
// them, because the helpers were right while the command printed the wrong
// sentence.
func TestFileRegisterPrintsThroughFollowUps(t *testing.T) {
	fileRegisterProject(t, "", fileRisk, fileSecond, fileClosedOpen, fileClosedAgain)

	for _, c := range []struct {
		name string
		args []string
		want []string
		deny []string
	}{
		{
			name: "all",
			args: []string{"follow-ups"},
			want: []string{
				"open follow-ups:",
				"  #1     [M] contract checks assert shape",
				"  #2     [H] resolver drops the trailing dot",
			},
			// #3 opened and closed in the same file: the fold drops it, and the
			// line that opened it survives so the history is not rewritten.
			deny: []string{"#3", "page limit"},
		},
		{
			name: "owned",
			args: []string{"follow-ups", "B1"},
			want: []string{"open follow-ups owned by B1:", "#1", "#2"},
			deny: []string{"#3"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var err error
			out := capture(t, func() { err = execute(t, c.args...) })
			if err != nil {
				t.Fatalf("%v: %v", c.args, err)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("%v wrote:\n%s\nwant it to contain %q", c.args, out, want)
				}
			}
			for _, deny := range c.deny {
				if strings.Contains(out, deny) {
					t.Errorf("%v wrote %q, which belongs to another row or another source:\n%s", c.args, deny, out)
				}
			}
		})
	}
}

// A file with exactly page_limit rows was read whole. The notice is keyed on a
// count against that limit, so without a source that says it has no pages it
// fires on a register nothing truncated.
func TestFileRegisterAtThePageLimitReportsNoTruncation(t *testing.T) {
	fileRegisterProject(t, "[github]\npage_limit = 2\n", fileRisk, fileSecond)

	var err error
	out := capture(t, func() { err = execute(t, "follow-ups") })
	if err != nil {
		t.Fatalf("follow-ups: %v", err)
	}
	if strings.Contains(out, "page limit") {
		t.Errorf("a file read whole reported a page that ran out:\n%s", out)
	}

	out = capture(t, func() { err = execute(t, "follow-ups", "--json") })
	if err != nil {
		t.Fatalf("follow-ups --json: %v", err)
	}
	if strings.Contains(out, `"truncated": true`) {
		t.Errorf("--json claimed a truncated read of a whole file:\n%s", out)
	}
}

// The empty answer and the unreadable one must stay distinguishable, so a line
// nothing can parse fails the read rather than shrinking it.
func TestFileRegisterMalformedLineIsUnreadableNotEmpty(t *testing.T) {
	fileRegisterProject(t, "", fileRisk, `{"number":2,`)
	var err error
	out := capture(t, func() { err = execute(t, "follow-ups") })
	if err == nil {
		t.Fatalf("a malformed register read clean:\n%s", out)
	}
	if !strings.Contains(err.Error(), "register.jsonl:2") {
		t.Errorf("error %q does not name the line that failed", err)
	}
	if strings.Contains(out, "no open follow-ups") {
		t.Errorf("an unreadable register printed as an empty one:\n%s", out)
	}
}

func TestFollowUpWritesAndClosesRows(t *testing.T) {
	dir := fileRegisterProject(t, "")
	markMerged(t, dir, "B1")
	path := filepath.Join(dir, ".drover", "register.jsonl")
	t.Cleanup(func() { followUpOwners, followUpSev = nil, "" })

	out := capture(t, func() {
		if err := execute(t, "follow-up", "open", "gh output shape is unpinned", "--owner", "B1", "--sev", "H"); err != nil {
			t.Fatalf("follow-up open: %v", err)
		}
	})
	if !strings.Contains(out, "filed #1: [H] gh output shape is unpinned (B1)") {
		t.Errorf("follow-up open wrote:\n%s", out)
	}
	// Flags are package state and cobra does not reset them between Execute
	// calls; one process is one invocation everywhere but here.
	followUpOwners = nil
	out = capture(t, func() {
		if err := execute(t, "follow-up", "open", "resolver drops the trailing dot", "--sev", "M"); err != nil {
			t.Fatalf("follow-up open: %v", err)
		}
	})
	if !strings.Contains(out, "filed #2: [M] resolver drops the trailing dot (unowned)") {
		t.Errorf("follow-up open wrote:\n%s", out)
	}

	out = capture(t, func() {
		if err := execute(t, "follow-ups"); err != nil {
			t.Fatalf("follow-ups: %v", err)
		}
	})
	if !strings.Contains(out, "#1") || !strings.Contains(out, "#2") {
		t.Errorf("follow-ups lost a row it had just filed:\n%s", out)
	}

	out = capture(t, func() {
		if err := execute(t, "follow-up", "close", "1"); err != nil {
			t.Fatalf("follow-up close: %v", err)
		}
	})
	if !strings.Contains(out, "closed #1: gh output shape is unpinned") {
		t.Errorf("follow-up close wrote:\n%s", out)
	}
	out = capture(t, func() {
		if err := execute(t, "follow-ups"); err != nil {
			t.Fatalf("follow-ups after close: %v", err)
		}
	})
	if strings.Contains(out, "#1") {
		t.Errorf("a closed row survived the fold:\n%s", out)
	}

	// The close is an append: the line that opened the row is still there, and
	// the number it spent is not handed to the next row.
	f, err := register.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rows) != 3 || f.Max != 2 {
		t.Errorf("file holds %d rows with max %d, want 3 and 2 — a close rewrote history", len(f.Rows), f.Max)
	}
}

// Refused before anything is read, naming the backend that is configured: the
// tracker owns its own writes, and "none" has nowhere to put a row.
func TestFollowUpRefusesUnderEveryOtherBackend(t *testing.T) {
	for _, c := range []struct{ name, block, want string }{
		{"github", ghProject, `backend = "github"`},
		{"declared github", "[register]\nbackend = \"github\"\n", `backend = "github"`},
		{"none", "[register]\nbackend = \"none\"\n", `backend = "none"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			registerProject(t, c.block)
			stubPATH(t)
			err := execute(t, "follow-up", "open", "a row with nowhere to go")
			if err == nil {
				t.Fatal("follow-up wrote a row into a project that has no file register")
			}
			if got := Code(err); got != ExitRefused {
				t.Errorf("exit %d, want %d — a refusal is not a failure to answer", got, ExitRefused)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the configured backend", err)
			}
		})
	}
}

func TestFollowUpRefusesAnUngradedSeverity(t *testing.T) {
	fileRegisterProject(t, "")
	t.Cleanup(func() { followUpOwners, followUpSev = nil, "" })

	// "none" is the second case because it is the one a reader arrives at from
	// status --sev, where it selects the rows carrying no rank. It is not a
	// rank, so filing a row at it would record the word for having none.
	for _, rank := range []string{"hihg", "none"} {
		t.Run(rank, func(t *testing.T) {
			followUpSev = ""
			err := execute(t, "follow-up", "open", "a rank nothing ranks", "--sev", rank)
			if err == nil {
				t.Fatal("filed a row at a rank nothing ranks")
			}
			if got := Code(err); got != ExitRefused {
				t.Errorf("exit %d, want %d", got, ExitRefused)
			}
			if !strings.Contains(err.Error(), "unknown severity \""+rank+"\"") {
				t.Errorf("error %q does not name the rank", err)
			}
			// The shorter vocabulary of the two --sev flags, spelled out so a
			// caller refused here is not sent to status --help to find it.
			if !strings.Contains(err.Error(), strings.Join(sev.Letters(), ", ")) {
				t.Errorf("error %q does not name the accepted values", err)
			}
		})
	}
}

// An owner nothing resolves is the failure the unrouted marker exists to make
// visible: the row prints as though it routes somewhere and reaches no run.
// Refused at the same point a typo'd severity is, and for the same reason.
func TestFollowUpRefusesAnOwnerNoBodyAnswersTo(t *testing.T) {
	dir := fileRegisterProject(t, "")
	markMerged(t, dir, "B1")
	t.Cleanup(func() { followUpOwners, followUpSev = nil, "" })

	for _, c := range []struct{ name, owner, want string }{
		{"wrong case", "b1", "b1"},
		{"no such body", "B9", "no such item: B9"},
		{"unknown namespace", "Z9", "Z9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			followUpOwners = nil
			err := execute(t, "follow-up", "open", "a row owned nowhere", "--owner", c.owner)
			if err == nil {
				t.Fatal("filed a row against an owner no run will ever see")
			}
			// Exit 1: an id with no body is something drover could not
			// resolve. A rule the project declared — an unranked severity — is
			// what exits 3, and the two must not read the same to a wrapper.
			if got := Code(err); got != ExitError {
				t.Errorf("exit %d, want %d", got, ExitError)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the owner it refused", err)
			}
		})
	}

	// The refusal is total: nothing reached the file, so a second owner that
	// does resolve did not smuggle the row past the first that does not.
	followUpOwners = nil
	if err := execute(t, "follow-up", "open", "half-owned", "--owner", "B1", "--owner", "b1"); err == nil {
		t.Fatal("one good owner carried a bad one into the file")
	}
	f, err := register.Read(filepath.Join(dir, ".drover", "register.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rows) != 0 {
		t.Errorf("register holds %d rows, want 0 — a refused open still wrote", len(f.Rows))
	}

	// The good owner on its own still files, so the check refuses a typo rather
	// than the flag.
	followUpOwners = nil
	out := capture(t, func() {
		if err := execute(t, "follow-up", "open", "owned", "--owner", "B1"); err != nil {
			t.Fatalf("follow-up open --owner B1: %v", err)
		}
	})
	if !strings.Contains(out, "filed #1: [-] owned (B1)") {
		t.Errorf("follow-up open wrote:\n%s", out)
	}
}

// An owner that has not merged owns nothing filed: its gaps are its own
// unfinished work and go back to its branch. follow-up open refuses it, writes
// neither the register nor the ledger, and files once the owner has merged.
func TestFollowUpOpenRefusesAnUnmergedOwner(t *testing.T) {
	dir := fileRegisterProject(t, "")
	write(t, filepath.Join(dir, "work", "B02-open.md"), body("an open bug", "[]"))
	t.Cleanup(func() { followUpOwners, followUpSev = nil, "" })
	before := snapshot(t, dir)

	var err error
	capture(t, func() { err = execute(t, "follow-up", "open", "gh output shape is unpinned", "--owner", "B2") })
	if Code(err) != ExitRefused {
		t.Fatalf("follow-up open --owner an unmerged item: exit %d (%v), want %d", Code(err), err, ExitRefused)
	}
	for _, want := range []string{"B2 has not merged", "drover request-changes B2 -f {file}", "drover new {kind} {title} -f {file}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, err)
		}
	}
	if got := snapshot(t, dir); !maps.Equal(got, before) {
		t.Errorf("the refusal wrote to the tree:\nbefore %v\nafter  %v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(got)))
	}

	markMerged(t, dir, "B2")
	followUpOwners = nil
	out := capture(t, func() { err = execute(t, "follow-up", "open", "gh output shape is unpinned", "--owner", "B2") })
	if err != nil {
		t.Fatalf("follow-up open --owner a merged item: %v", err)
	}
	if !strings.Contains(out, "filed #1:") {
		t.Errorf("follow-up open wrote:\n%s", out)
	}
}
