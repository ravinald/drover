package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tracker holds three things at once and only one of them is pending: a
// register row (follow-up label, no kind), an issue a body already claims, and an
// issue nobody has taken in. Reading the register's query here would return the
// first and miss the third, which is why pending asks unfiltered.
const trackerPage = `[` +
	`{"number":10,"title":"a follow-up nobody priced","body":"Owner: B1","state":"OPEN",` +
	`"labels":[{"name":"follow-up"},{"name":"severity:low"}]},` +
	`{"number":11,"title":"already an item","body":"","state":"OPEN",` +
	`"labels":[{"name":"bug"},{"name":"severity:medium"}]},` +
	`{"number":12,"title":"owned and waiting","body":"Owner: B1","state":"OPEN",` +
	`"labels":[{"name":"bug"},{"name":"severity:high"}]},` +
	`{"number":13,"title":"not drover business","body":"","state":"OPEN",` +
	`"labels":[{"name":"question"}]}` +
	`]`

func pendingProject(t *testing.T) {
	t.Helper()
	dir := registerProject(t, ghProject)
	// The body claiming #11 is what makes the idempotency half of the filter
	// falsifiable: without it, "already an item" and "pending" look the same.
	// The fixture declares `issue` because an undeclared key lands the body in
	// Broken, which itemForIssue does not scan — the same gap that lets a
	// re-run of intake allocate a second id for one issue.
	claimed := "---\ntitle: Already an item\ndeps:\nissue: 11\n---\n\n1. Do it.\n"
	if err := os.WriteFile(filepath.Join(dir, "work", "B01-already-an-item.md"), []byte(claimed), 0o600); err != nil {
		t.Fatal(err)
	}
	stubGHIssues(t, stubPATH(t), trackerPage)
}

func TestPendingIsTheImportQueueAndNotTheRegister(t *testing.T) {
	pendingProject(t)
	t.Cleanup(func() { intakeList, intakeJSON = false, false })

	var err error
	out := capture(t, func() { err = execute(t, "intake", "--list") })
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if !strings.Contains(out, "#12") {
		t.Errorf("pending lost the one issue nobody has taken in:\n%s", out)
	}
	for _, absent := range []struct{ number, why string }{
		{"#10", "a register row is not pending intake"},
		{"#11", "a body already claims it"},
		{"#13", "it carries no label matching a namespace"},
	} {
		if strings.Contains(out, absent.number) {
			t.Errorf("pending listed %s — %s:\n%s", absent.number, absent.why, out)
		}
	}
}

func TestPendingJSONCarriesTheKindIntakeWouldAllocate(t *testing.T) {
	pendingProject(t)
	intakeJSON = true
	t.Cleanup(func() { intakeList, intakeJSON = false, false })

	var err error
	out := capture(t, func() { err = execute(t, "intake", "--list") })
	if err != nil {
		t.Fatalf("pending --json: %v", err)
	}
	var rows []PendingRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only #12", rows)
	}
	got := rows[0]
	// The kind and the severity are what intake seeds a scaffold from, so a row
	// here that disagrees with them sends an operator to a command that will
	// allocate against a different namespace.
	if got.Number != 12 || got.Kind != "bug" || got.Severity != "high" {
		t.Errorf("row = %+v, want #12 bug high", got)
	}
	if len(got.Owners) != 1 || got.Owners[0] != "B1" {
		t.Errorf("owners = %v, want [B1] — an owned issue is the common case", got.Owners)
	}
}

// A project with no tracker has nothing to be pending from, and the refusal has
// to name the same next step intake's does.
func TestPendingRefusesWithoutATracker(t *testing.T) {
	registerProject(t, "[register]\nbackend = \"none\"\n")
	stubPATH(t)
	err := execute(t, "intake", "--list")
	if err == nil {
		t.Fatal("pending read a tracker the project declared it does not have")
	}
	if code := Code(err); code != ExitRefused {
		t.Errorf("exit %d, want %d", code, ExitRefused)
	}
	if !strings.Contains(err.Error(), "Hand-write the body instead") {
		t.Errorf("refusal %q does not name the way forward", err)
	}
}
