package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/t3"
)

const t3ShowOutput = "B12  needs-operator  the handler drops a header\n  park: auth_expired"

// fromEvent runs drover t3 thread --from-event in-process on a T3 host, with
// the event as its stdin, and returns what it printed. drover show is stubbed:
// the host has no project for the real one to read.
func fromEvent(t *testing.T, dir string, e NotifyPayload, args ...string) (string, error) {
	t.Helper()
	show := t3ShowItem
	t.Cleanup(func() { t3ShowItem = show })
	t3ShowItem = func(_ context.Context, _, id string) (string, error) {
		if id != "B12" {
			t.Errorf("drover show ran for %q", id)
		}
		return t3ShowOutput, nil
	}
	t.Chdir(dir)
	line, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	resetFlags(root)
	var out bytes.Buffer
	root.SetArgs(append([]string{"t3", "thread", "--from-event"}, args...))
	root.SetIn(bytes.NewReader(append(line, '\n')))
	root.SetOut(&out)
	root.SetErr(io.Discard)
	t.Cleanup(func() {
		root.SetArgs(nil)
		root.SetIn(nil)
		root.SetOut(nil)
		resetFlags(root)
	})
	err = root.Execute()
	return out.String(), err
}

func operatorEvent() NotifyPayload {
	return NotifyPayload{DriveEvent: DriveEvent{Event: evNeedsOperator, Project: "fixture", Drive: "D4", Seq: 9, ID: "B12",
		Action: "auth_expired", Policy: "operator", Run: "claude auth login, then drover drive B12"},
		Priority: "high", Message: "auth_expired: claude auth login, then drover drive B12"}
}

// openedLine is the one thread_opened line the bridge printed.
func openedLine(t *testing.T, out string) DriveEvent {
	t.Helper()
	var got []DriveEvent
	for th := range threadLines(out) {
		got = append(got, th)
	}
	if len(got) != 1 {
		t.Fatalf("the bridge printed %d thread_opened lines, want 1:\n%s", len(got), out)
	}
	return got[0]
}

// A needs_operator opens one supervised thread titled for the item and action,
// whose first message is the phone's text, drover show's output and the
// command to run first, fenced, and prints the thread_opened the drive records.
func TestFromEventOpensAThreadCarryingShowAndTheCommand(t *testing.T) {
	srv, root := t3Host(t)
	out, err := fromEvent(t, root, operatorEvent())
	if err != nil {
		t.Fatalf("--from-event: %v\n%s", err, out)
	}
	calls := srv.CallsTo("t3_thread_launch")
	if len(calls) != 1 {
		t.Fatalf("%d launches, want 1", len(calls))
	}
	a := calls[0].Args
	if a["title"] != "fixture B12: auth_expired" || a["runtimeMode"] != "approval-required" {
		t.Errorf("the thread was launched as %v under %v", a["title"], a["runtimeMode"])
	}
	want := "auth_expired: claude auth login, then drover drive B12\n\n" +
		"`drover show B12`:\n\n```text\n" + t3ShowOutput + "\n```\n\n" +
		"Run this first:\n\n```sh\nclaude auth login, then drover drive B12\n```"
	if a["message"] != want {
		t.Errorf("the first message is\n%s\nwant\n%s", a["message"], want)
	}
	th := openedLine(t, out)
	if th.ID != "B12" || th.Thread == "" || th.URL != t3.ThreadURL(th.Thread) {
		t.Errorf("thread_opened = %+v", th)
	}
}

// A body carrying a fence of its own is fenced past it, so it cannot close
// the block it is quoted in.
func TestFencedOutrunsTheBacktickRunsItQuotes(t *testing.T) {
	if got := fenced("a\n```\nb", "text"); got != "````text\na\n```\nb\n````" {
		t.Errorf("fenced = %q", got)
	}
}

// Any other event kind opens nothing, prints nothing, and exits 0, so one
// destination can route every event without opening a thread per stage.
func TestFromEventOpensNothingForAnyOtherEvent(t *testing.T) {
	srv, root := t3Host(t)
	for _, kind := range []string{evParked, evMerged, evStage, evExit} {
		e := operatorEvent()
		e.Event = kind
		out, err := fromEvent(t, root, e)
		if err != nil || out != "" {
			t.Errorf("%s: %v, printed %q", kind, err, out)
		}
	}
	if n := len(srv.Calls); n != 0 {
		t.Errorf("%d calls reached the server, want none", n)
	}
}

// A second needs_operator for an item whose thread is open is sent to that
// thread, whether the drive hands the thread over on the event or the bridge
// reads it from the drive's event file; a stopped or deleted thread is
// replaced.
func TestFromEventReusesAnOpenThreadAndReplacesAClosedOne(t *testing.T) {
	ownState(t)
	srv, root := t3Host(t)
	out, err := fromEvent(t, root, operatorEvent())
	if err != nil {
		t.Fatal(err)
	}
	first := openedLine(t, out)

	handed := operatorEvent()
	handed.Thread, handed.URL = first.Thread, first.URL
	out, err = fromEvent(t, root, handed)
	if err != nil {
		t.Fatal(err)
	}
	if th := openedLine(t, out); th.Thread != first.Thread {
		t.Errorf("the second event opened %s, want %s reused", th.Thread, first.Thread)
	}
	sends := srv.CallsTo("t3_thread_send")
	if len(sends) != 1 || sends[0].Args["threadId"] != first.Thread || !strings.Contains(sends[0].Args["message"].(string), t3ShowOutput) {
		t.Fatalf("t3_thread_send calls = %+v", sends)
	}

	path, err := driveEventsPath("fixture", "D4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"event":"parked","project":"fixture","id":"B12","park":"auth_expired"}`+"\n"+strings.TrimSpace(out)+"\n")
	if _, err := fromEvent(t, root, operatorEvent()); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.CallsTo("t3_thread_send")); n != 2 {
		t.Errorf("%d sends after the event file named the thread, want 2", n)
	}
	if n := len(srv.CallsTo("t3_thread_launch")); n != 1 {
		t.Fatalf("%d launches while the thread stood open, want 1", n)
	}

	if _, err := t3.Interrupt(context.Background(), t3Client(t3Cfg(t, root)), first.Thread, "", "test"); err != nil {
		t.Fatal(err)
	}
	out, err = fromEvent(t, root, handed)
	if err != nil {
		t.Fatal(err)
	}
	stopped := openedLine(t, out)
	if stopped.Thread == first.Thread {
		t.Error("a stopped thread was reused")
	}

	srv.Delete(stopped.Thread)
	handed.Thread = stopped.Thread
	out, err = fromEvent(t, root, handed)
	if err != nil {
		t.Fatal(err)
	}
	if th := openedLine(t, out); th.Thread == stopped.Thread {
		t.Error("a deleted thread was reused")
	}
	if n := len(srv.CallsTo("t3_thread_launch")); n != 3 {
		t.Errorf("%d launches, want 3: the first, and one for each closed thread", n)
	}
}

func t3Cfg(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.LoadT3(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// full is a load error for operator_permission, and --permission full is
// refused on this command: a thread opened unattended never skips approval.
func TestFromEventRefusesFullPermission(t *testing.T) {
	srv, root := t3Host(t)
	conf := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "drover", "config.toml")
	data, err := os.ReadFile(conf) // #nosec G304 -- the test's own config
	if err != nil {
		t.Fatal(err)
	}
	out, err := fromEvent(t, root, operatorEvent(), "--permission", "full")
	if Code(err) != ExitRefused || out != "" {
		t.Errorf("--permission full exited %d (%v), printed %q", Code(err), err, out)
	}
	write(t, conf, string(data)+"operator_permission = \"full\"\n")
	if _, err := config.LoadT3(root); err == nil || !strings.Contains(err.Error(), "operator_permission") {
		t.Errorf("LoadT3 with operator_permission = \"full\": %v", err)
	}
	if _, err := fromEvent(t, root, operatorEvent()); err == nil {
		t.Error("--from-event ran under operator_permission = \"full\"")
	}
	if n := len(srv.CallsTo("t3_thread_launch")); n != 0 {
		t.Errorf("%d launches under full, want none", n)
	}
}

// The bridge's answer reaches the drive: the thread_opened goes into the
// event file, the next needs_operator for the item hands the thread over, and
// a message for the item with no pull request opens the thread, while one
// with a pull request still opens that.
func TestAThreadTheBridgeOpenedIsWhatTheItemsMessagesOpen(t *testing.T) {
	srv, seen := ntfyServer(t, http.StatusOK, "")
	bin := t.TempDir()
	stdin := filepath.Join(bin, "stdin")
	stub(t, filepath.Join(bin, "bridge"), "#!/bin/sh\ncat >> "+stdin+"\n"+
		`echo '{"event":"thread_opened","project":"example","id":"B12","thread":"mcp:t1","url":"t3-thread://v1/mcp:t1"}'`+"\n")
	table, _ := ntfyTable(t, srv.URL+"/drover", `{ parked = "default", merged = "default" }`)
	es := notifyFixture(t, table+"\n[notify.destinations.t3]\ntype = \"command\"\nargv = [\""+filepath.Join(bin, "bridge")+"\"]\n"+
		"events = { needs_operator = \"high\" }\n")
	es.notifier.forge = func() string { return "https://github.com/o/r" }

	es.emit(DriveEvent{Event: evNeedsOperator, ID: "B12", Action: "auth_expired", Run: "claude auth login"})
	es.notifier.idle()
	es.emit(DriveEvent{Event: evNeedsOperator, ID: "B12", Action: "auth_expired", Run: "claude auth login"})
	es.emit(DriveEvent{Event: evParked, ID: "B12", Park: "auth_expired"})
	es.emit(DriveEvent{Event: evMerged, ID: "B12", PR: 7})
	es.emit(DriveEvent{Event: evParked, ID: "B13", Park: "auth_expired"})
	es.notifier.idle()

	got := seen()
	if len(got) != 3 {
		t.Fatalf("%d ntfy sends, want 3", len(got))
	}
	for i, want := range []string{"t3-thread://v1/mcp:t1", "https://github.com/o/r/pull/7", ""} {
		if c := got[i].header.Get("Click"); c != want {
			t.Errorf("send %d Click is %q, want %q", i, c, want)
		}
	}

	raw, err := os.ReadFile(stdin) // #nosec G304 -- written by the stub above
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var second NotifyPayload
	if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &second) != nil || second.Thread != "mcp:t1" || second.URL != "t3-thread://v1/mcp:t1" {
		t.Errorf("the second needs_operator reached the bridge as %v", lines)
	}

	path, err := driveEventsPath("example", "D7")
	if err != nil {
		t.Fatal(err)
	}
	var events []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		events, _ = os.ReadFile(path) // #nosec G304 -- under the test's state directory
		if bytes.Contains(events, []byte(`"event":"thread_opened"`)) {
			break
		}
	}
	es.exit(nil)
	events, err = os.ReadFile(path) // #nosec G304 -- under the test's state directory
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(events), `"event":"thread_opened"`); n != 1 {
		t.Errorf("the event file holds %d thread_opened, want 1: a reused thread is not news\n%s", n, events)
	}
}

// notify --test sends a needs_operator through the bridge's destination and
// prints the thread it opened.
func TestNotifyTestPrintsTheThreadTheBridgeOpened(t *testing.T) {
	t.Cleanup(func() { notifyTest, notifyEvent = false, "" })
	bin := t.TempDir()
	stub(t, filepath.Join(bin, "bridge"), "#!/bin/sh\ncat >/dev/null\n"+
		`echo '{"event":"thread_opened","project":"example","id":"TEST","thread":"mcp:t9","url":"t3-thread://v1/mcp:t9"}'`+"\n")
	notifyFixture(t, "\n[notify.destinations.t3]\ntype = \"command\"\nargv = [\""+filepath.Join(bin, "bridge")+"\"]\n"+
		"events = { needs_operator = \"high\" }\n")
	var err error
	out := capture(t, func() { err = execute(t, "notify", "--test", "--event", "needs_operator") })
	if err != nil || !strings.Contains(out, "opened T3 Code thread t3-thread://v1/mcp:t9 for TEST") {
		t.Errorf("notify --test: %v\n%s", err, out)
	}
}
