package register

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeRegister(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "register.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The fold is the whole reason the file is append-only: a close is a new line,
// and an edit to an open row is another line rather than a rewrite of the one
// that recorded it.
func TestReadFoldsNewestWinsAndDropsClosed(t *testing.T) {
	path := writeRegister(t,
		`{"number":1,"title":"first cut","severity":"L","state":"open","date":"2026-01-01"}`,
		`{"number":2,"title":"a second row","owners":["B1"],"state":"open","date":"2026-01-02"}`,
		`{"number":1,"title":"first cut, regraded","severity":"H","state":"open","date":"2026-01-03"}`,
		`{"number":2,"title":"a second row","owners":["B1"],"state":"closed","date":"2026-01-04"}`,
	)
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Max != 2 {
		t.Errorf("Max = %d, want 2 — a closed number is still spent", f.Max)
	}
	open := f.Open()
	if len(open) != 1 {
		t.Fatalf("open = %+v, want only #1", open)
	}
	if open[0].Severity != "H" {
		t.Errorf("#1 severity = %q, want the newest line's H", open[0].Severity)
	}

	ix := Load(path)
	if !ix.OK || len(ix.Issues) != 1 || ix.Issues[0].Number != 1 {
		t.Fatalf("index = %+v, want one open row", ix)
	}
	if ix.Issues[0].Title != "first cut, regraded" {
		t.Errorf("row = %+v, want the folded row", ix.Issues[0])
	}
}

// "Nobody filed one" and "nobody could ask" rank the same item differently, and
// a file source that called an empty register unreadable would break that in
// the direction that hides work.
func TestMissingAndEmptyReadAsAnEmptyRegister(t *testing.T) {
	for _, c := range []struct{ name, path string }{
		{"missing", filepath.Join(t.TempDir(), "nothing.jsonl")},
		{"empty", writeRegister(t)},
		{"blank lines", writeRegister(t, "", "  ")},
	} {
		t.Run(c.name, func(t *testing.T) {
			ix := Load(c.path)
			if !ix.OK || ix.Err != nil {
				t.Fatalf("OK = %v, err = %v — an empty register is an answer, not a failure", ix.OK, ix.Err)
			}
			if len(ix.Issues) != 0 {
				t.Errorf("issues = %+v, want none", ix.Issues)
			}
		})
	}
}

// A read that answered with the lines that happened to parse would report a
// smaller register as a whole one, which is the answer a caller cannot check.
func TestMalformedLineIsUnreadableAndNamesTheLine(t *testing.T) {
	for _, c := range []struct{ name, bad, want string }{
		{"not json", `{"number":2,`, "is not a register row"},
		{"no number", `{"title":"nameless","state":"open"}`, "has no number"},
		{"unknown state", `{"number":2,"title":"x","state":"OPEN"}`, `has state "OPEN"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := writeRegister(t,
				`{"number":1,"title":"fine","state":"open","date":"2026-01-01"}`,
				c.bad)
			ix := Load(path)
			if ix.OK {
				t.Fatalf("a malformed line read as OK with %d issues", len(ix.Issues))
			}
			msg := ix.Reason().Error()
			if !strings.Contains(msg, ":2") {
				t.Errorf("error %q does not name line 2", msg)
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q does not say what is wrong (%q)", msg, c.want)
			}
		})
	}
}

// Truncated reads a count against the page limit, so a file holding exactly
// page_limit rows would report a page that ran out on a source with no pages.
func TestAFullPageOfRowsIsNotATruncatedRead(t *testing.T) {
	var lines []string
	for n := 1; n <= 5; n++ {
		lines = append(lines, `{"number":`+strconv.Itoa(n)+`,"title":"row","state":"open","date":"2026-01-01"}`)
	}
	ix := Load(writeRegister(t, lines...))
	if len(ix.Issues) != 5 {
		t.Fatalf("read %d rows, want 5", len(ix.Issues))
	}
	if ix.Truncated(5) {
		t.Error("a file read whole reported itself cut off at the page limit")
	}
}

func TestAppendStampsTheDateAndRefusesAnUnkeyedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "register.jsonl")
	if err := Append(path, Row{Number: 1, Title: "filed", State: StateOpen}); err != nil {
		t.Fatal(err)
	}
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rows) != 1 || f.Rows[0].Date == "" {
		t.Fatalf("rows = %+v, want one carrying a date", f.Rows)
	}
	if err := Append(path, Row{Title: "no number", State: StateOpen}); err == nil {
		t.Error("appended a row nothing can key on")
	}
}
