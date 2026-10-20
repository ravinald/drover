package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reportProject stands up a project whose log directory resolves under a home
// drover can shorten: no [paths] logs and no XDG_CACHE_HOME, which is the
// default every project starts on.
func reportProject(t *testing.T) (home, logPath string) {
	t.Helper()
	t.Cleanup(func() { reportJSON, reportSave = false, false })

	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")

	s := fixture(t, "")
	dir := filepath.Join(home, ".cache", "drover", "example")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(dir, "01-first.20260101-000000.000000000.jsonl")
	write(t, logPath,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.1,"result":"done"}`+"\n")
	t.Chdir(s.cfg.Root)
	return home, logPath
}

// The path is printed so somebody can open it, and the default log directory
// sits outside the tree, so the absolute form was a line of noise nobody reads
// to the end.
func TestReportShortensTheLogPathForTheTerminal(t *testing.T) {
	_, _ = reportProject(t)

	var err error
	out := capture(t, func() { err = execute(t, "report", "1") })
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	want := "==> " + filepath.Join("~", ".cache", "drover", "example", "01-first.")
	if !strings.Contains(out, want) {
		t.Errorf("output does not carry %q:\n%s", want, out)
	}
}

// The other half, and the reason the shortening is not applied at the source.
// A wrapper reading log_path opens it; os.Open does not expand a ~, so a
// shortened path there fails with a file-not-found on a file that is present.
func TestReportJSONKeepsTheAbsoluteLogPath(t *testing.T) {
	home, logPath := reportProject(t)

	var err error
	out := capture(t, func() { err = execute(t, "report", "1", "--json") })
	if err != nil {
		t.Fatalf("result --json: %v", err)
	}
	var got RunReport
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.LogPath != logPath {
		t.Errorf("log_path = %q, want the absolute path %q", got.LogPath, logPath)
	}
	if !strings.HasPrefix(got.LogPath, home) {
		t.Errorf("log_path = %q, want it under %q", got.LogPath, home)
	}
	if _, err := os.Open(got.LogPath); err != nil { // #nosec G304 -- the path the report just emitted
		t.Errorf("log_path does not open: %v", err)
	}
}
