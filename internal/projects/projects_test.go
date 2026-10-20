package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFoldsAndSkipsWhatItCannotParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	lines := `{"project":"b","root":"/src/b","registered":"2026-01-01"}
not json at all
{"project":"a","root":"/src/a","repo":"me/a","registered":"2026-01-02"}
{"project":"b","root":"/src/b","repo":"me/b","registered":"2026-03-01"}
{"registered":"2026-03-02"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	// Three of the five survive: one line is not JSON and one names no root,
	// and a corrupt line must not hide the history around it.
	if len(rows) != 3 {
		t.Fatalf("read %d rows, want 3: %+v", len(rows), rows)
	}

	latest := Latest(rows)
	if len(latest) != 2 {
		t.Fatalf("folded to %d, want 2 — one root registered twice is one project", len(latest))
	}
	if latest[0].Project != "a" || latest[1].Project != "b" {
		t.Errorf("order = %q %q, want a b", latest[0].Project, latest[1].Project)
	}
	if latest[1].Repo != "me/b" || latest[1].Registered != "2026-03-01" {
		t.Errorf("b folded to %+v, want the newest row", latest[1])
	}
}

// A missing file is an empty registry, not an error: nothing reads it to decide
// anything, and a machine that has never run init has none.
func TestReadTreatsAnAbsentFileAsEmpty(t *testing.T) {
	rows, err := Read(filepath.Join(t.TempDir(), FileName))
	if err != nil || rows != nil {
		t.Errorf("Read(missing) = %v, %v; want nil, nil", rows, err)
	}
	if rows, err := Read(""); err != nil || rows != nil {
		t.Errorf("Read(\"\") = %v, %v; want nil, nil — a machine with no home has no registry", rows, err)
	}
}

// Appending is what init does, and a second registration of one root is another
// line rather than an edit: the file records what happened.
func TestAppendStampsTheDateAndKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	for i := 0; i < 2; i++ {
		if err := Append(path, Row{Project: "p", Root: "/src/p"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2 — the second registration rewrote the first", len(rows))
	}
	if rows[0].Registered == "" {
		t.Error("Append left the date empty; nothing else stamps it")
	}
	if got := Latest(rows); len(got) != 1 {
		t.Errorf("Latest folded to %d, want 1", len(got))
	}
}

func TestPathSitsBesideTheConfig(t *testing.T) {
	if got := Path("/home/me/.config/drover/config.toml"); got != "/home/me/.config/drover/"+FileName {
		t.Errorf("Path = %q", got)
	}
	// No home directory resolves to no registry, which every caller treats as
	// "nothing to record" and never as an error.
	if got := Path(""); got != "" {
		t.Errorf("Path(\"\") = %q, want empty", got)
	}
}
