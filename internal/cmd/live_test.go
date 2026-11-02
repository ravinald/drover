package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
)

// liveProject is mergeProject with a [live] default whose check prints the
// item, the branch and the head it was handed and exits with exit.
func liveProject(t *testing.T, exit string) (ghLog, dir string) {
	t.Helper()
	ghLog = mergeProject(t)
	dir, _ = forge(t, ghLog)
	check := filepath.Join(t.TempDir(), "live-check")
	stub(t, check, "#!/bin/sh\necho \"client saw $DROVER_ITEM on $DROVER_BRANCH at $DROVER_SHA\"\nexit "+exit+"\n")
	conf := filepath.Join(dir, ".drover.toml")
	b, err := os.ReadFile(conf) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, conf, string(b)+"\n[live]\ndefault = \"client\"\n\n[live.client]\nargv = [\""+check+"\"]\nlock = \"hosts\"\n")
	return ghLog, dir
}

func liveRows(t *testing.T, dir string) []ledger.Row {
	t.Helper()
	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Row
	for _, r := range l.Rows {
		if r.Live != nil {
			out = append(out, r)
		}
	}
	return out
}

func TestALiveCheckThatPassesLetsTheItemMerge(t *testing.T) {
	ghLog, dir := liveProject(t, "0")
	got := driveAll(t)[0]
	if !got.Merged {
		t.Fatalf("item 1 is %q %q (%s), want merged", got.Outcome, got.Park, got.Reason)
	}
	if merges := mergeCalls(t, ghLog); len(merges) != 1 {
		t.Errorf("merges = %q, want one", merges)
	}
	rows := liveRows(t, dir)
	if len(rows) != 1 || rows[0].Live.Check != "client" || rows[0].Live.Exit != 0 || !strings.HasPrefix(rows[0].Live.Line, "client saw 1 on ") {
		t.Errorf("live rows = %+v, want one passing run naming the item", rows)
	}
}

// A real client failing is a finding about the work, so it goes back to the
// branch through request-changes and counts as a review round.
func TestALiveCheckThatFailsGoesBackToTheBranch(t *testing.T) {
	ghLog, dir := liveProject(t, "1")
	got := driveAll(t)[0]
	if got.Merged {
		t.Fatal("an item whose live check failed merged")
	}
	if merges := mergeCalls(t, ghLog); len(merges) != 0 {
		t.Errorf("merges = %q, want none", merges)
	}
	body, err := os.ReadFile(filepath.Join(dir, "work", "01-first.md")) // #nosec G304 -- the fixture's own body
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "### Live check client") || !strings.Contains(string(body), "client saw 1 on ") {
		t.Errorf("the body does not carry the check's output under its heading:\n%s", body)
	}
	l, _ := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if c := l.Counts("1"); c.ChangesRequested == 0 {
		t.Error("the live send-back did not count as a review round")
	}
}

// Exit 4 says the environment is down, which is not the work's fault, so the
// item parks for the operator and nothing goes back.
func TestALiveCheckThatCannotReachItsHostsParks(t *testing.T) {
	_, dir := liveProject(t, "4")
	got := driveAll(t)[0]
	if got.Outcome != outcomeParked || got.Park != parkLiveUnavailable {
		t.Fatalf("item 1 is %q %q (%s), want parked %s", got.Outcome, got.Park, got.Reason, parkLiveUnavailable)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "work", "01-first.md")) // #nosec G304 -- the fixture's own body
	if strings.Contains(string(body), "### Live check") {
		t.Error("an unavailable environment was sent back to the branch")
	}
}

// Two checks naming one lock never run at once: the second waits, and parks
// busy once [live] wait runs out.
func TestALiveLockHoldsASecondCheckOut(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := liveLockPoll
	liveLockPoll = 10 * time.Millisecond
	t.Cleanup(func() { liveLockPoll = old })

	release, err := acquireLiveLock("hosts", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLiveLock("hosts", 50*time.Millisecond); err != errLiveBusy {
		t.Fatalf("a second holder got %v, want errLiveBusy while the first holds it", err)
	}
	if _, err := acquireLiveLock("other", 50*time.Millisecond); err != nil {
		t.Errorf("a different lock waited on this one: %v", err)
	}
	release()
	again, err := acquireLiveLock("hosts", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("the lock did not come free on release: %v", err)
	}
	again()
}

// A pass on an older head says nothing about the head that would merge.
func TestOpenPRWantsALivePassOnTheHeadItself(t *testing.T) {
	_, dir := liveProject(t, "0")
	s := openIn(t, dir)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []ledger.Row{{ID: "1", Live: &ledger.LiveRun{Check: "client", Head: "aaaaaaa1111111", Exit: 0}, Date: ledger.Today()}} {
		if err := s.record(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.liveGate(it, "bbbbbbb2222222"); err == nil || Code(err) != ExitRefused || !strings.Contains(err.Error(), "drover live 1") {
		t.Errorf("liveGate on a newer head = %v, want a refusal naming drover live 1", err)
	}
	if err := s.liveGate(it, "aaaaaaa1111111"); err != nil {
		t.Errorf("liveGate on the head that passed = %v", err)
	}
}

// live: none opts an item out of [live] default.
func TestLiveNoneSkipsTheDefault(t *testing.T) {
	_, dir := liveProject(t, "1")
	s := openIn(t, dir)
	it, err := s.item("1")
	if err != nil {
		t.Fatal(err)
	}
	it.Meta[backlog.KeyLive] = "none"
	if name, _, err := s.liveCheckFor(it); err != nil || name != "" {
		t.Errorf("liveCheckFor(live: none) = %q, %v; want no check", name, err)
	}
	delete(it.Meta, backlog.KeyLive)
	if name, _, _ := s.liveCheckFor(it); name != "client" {
		t.Errorf("liveCheckFor(no key) = %q, want the default", name)
	}
}
