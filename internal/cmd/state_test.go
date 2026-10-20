package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/runlog"
)

// One classifier, two callers. status renders what this returns and drive
// selects on it, so the states have to mean the same thing to both.
func TestWorkStateNamesEveryStateAnItemMovesThrough(t *testing.T) {
	dir := shelfProject(t, map[string]string{
		"B01-ready.md":   "---\ntitle: Ready\ndeps: \n---\nRequirements:\n\n1. Do it.\n",
		"B02-blocked.md": "---\ntitle: Blocked\ndeps: B1\n---\nRequirements:\n\n1. Do it.\n",
		"B04-done.md":    "---\ntitle: Done\ndeps: \n---\nRequirements:\n\n1. Do it.\n",
	}, nil)
	yes := true
	if err := ledger.Append(filepath.Join(dir, "ledger.jsonl"),
		ledger.Row{ID: "B4", Merged: &yes, Date: ledger.Today()}); err != nil {
		t.Fatal(err)
	}
	s := openIn(t, dir)
	logDir := s.cfg.LogDir()

	want := map[string]string{"B1": stateReady, "B2": stateBlocked, "B4": stateDone}
	for _, it := range s.backlog.Items {
		got, missing := s.workState(it, logDir)
		if w, ok := want[it.ID.Bare()]; ok && got != w {
			t.Errorf("%s is %q, want %q", it.ID.Bare(), got, w)
		}
		if it.ID.Bare() == "B2" && len(missing) != 1 {
			t.Errorf("a blocked item named %d missing deps, want 1", len(missing))
		}
	}

	// The scaffold arm needs a project that declares a stop marker, which is
	// what isScaffold reads; without one the marker is a line of prose.
	sc := openIn(t, markerProject(t, map[string]string{
		"B01-scaffold.md": "---\ntitle: Scaffold\ndeps: \n---\nSTOP: requirements not yet written\n\n1.\n",
	}))
	if got, _ := sc.workState(sc.backlog.Items[0], sc.cfg.LogDir()); got != stateScaffold {
		t.Errorf("an item carrying the stop marker is %q, want %q", got, stateScaffold)
	}
}

// The queue is what a drive attempts with no ids named: ready only, and in
// backlog order so a drive is reproducible.
func TestQueueIsTheReadyItemsInBacklogOrder(t *testing.T) {
	dir := shelfProject(t, map[string]string{
		"B01-first.md":   "---\ntitle: First\ndeps: \n---\nRequirements:\n\n1. Do it.\n",
		"B02-blocked.md": "---\ntitle: Blocked\ndeps: B9\n---\nRequirements:\n\n1. Do it.\n",
		"B03-third.md":   "---\ntitle: Third\ndeps: \n---\nRequirements:\n\n1. Do it.\n",
	}, nil)
	s := openIn(t, dir)
	var got []string
	for _, it := range s.queue() {
		got = append(got, it.ID.Bare())
	}
	if len(got) != 2 || got[0] != "B1" || got[1] != "B3" {
		t.Errorf("queue is %v, want [B1 B3]", got)
	}
}

// An item a drive parked reads as `ran`, which keeps it out of the queue. That
// is wanted: it stopped for a reason a person has to read, and naming it
// positionally is how it resumes.
func TestQueueSkipsAnItemThatAlreadyRan(t *testing.T) {
	// Per-test, not just per-package: TestMain points the cache at one directory
	// for the whole run, and LogDir keys on the project name, so two tests using
	// the same fixture name would read each other's logs.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := shelfProject(t, map[string]string{
		"B01-first.md": "---\ntitle: First\ndeps: \n---\nRequirements:\n\n1. Do it.\n",
	}, nil)
	s := openIn(t, dir)
	it := s.backlog.Items[0]
	logPath := filepath.Join(s.cfg.LogDir(),
		runlog.Name(s.cfg.Paths.LogPrefix+runlog.Base(it.ID.Stem(), it.Slug), time.Now()))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath,
		[]byte(`{"type":"result","subtype":"success","is_error":false}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openIn(t, dir)
	if q := s.queue(); len(q) != 0 {
		t.Errorf("queue carries %d items after a clean run, want 0", len(q))
	}
}

// A log that exists and carries no terminal event is a session that died, and
// it reads nothing like an absent log. A loop that cannot tell them apart
// either re-runs finished work or merges on a session that stopped.
func TestLogResultSeparatesDiedFromNeverRan(t *testing.T) {
	// Per-test, not just per-package: TestMain points the cache at one directory
	// for the whole run, and LogDir keys on the project name, so two tests using
	// the same fixture name would read each other's logs.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := shelfProject(t, map[string]string{
		"B01-first.md": "---\ntitle: First\ndeps: \n---\nRequirements:\n\n1. Do it.\n",
	}, nil)
	s := openIn(t, dir)
	it := s.backlog.Items[0]
	base := runlog.Base(it.ID.Stem(), it.Slug)

	if _, path, found, err := s.logResult(base); err != nil || path != "" || found {
		t.Errorf("no log at all reported path %q found %v err %v", path, found, err)
	}

	logPath := filepath.Join(s.cfg.LogDir(), runlog.Name(s.cfg.Paths.LogPrefix+base, time.Now()))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	// Events but no result: the session died partway.
	if err := os.WriteFile(logPath, []byte(`{"type":"assistant"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, path, found, err := s.logResult(base); err != nil || path == "" || found {
		t.Errorf("a log with no terminal event reported path %q found %v err %v", path, found, err)
	}
}
