package backlog

import (
	"os"
	"path/filepath"
	"testing"
)

func shelfDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The whole design rule in one test: a retired body is read for its id and its
// content is never touched. Frontmatter that would fail every rule the backlog
// enforces, plus a stop marker that would refuse a run, still yields the id and
// no error — nothing is going to run it.
func TestShelfReadsIDsWithoutParsingBodies(t *testing.T) {
	dir := shelfDir(t, map[string]string{
		"F01-first.md":  "---\ntitle\n  deps: [\nmade_up_key: 1\n",
		"F02-second.md": "STOP: requirements not yet written\n",
		"B03-third.md":  "",
		"README.md":     "# What lives here\n",
		"notes.txt":     "F99 is not a body\n",
	})

	sh, err := LoadShelf(dir, []string{"B", "F"})
	if err != nil {
		t.Fatalf("LoadShelf: %v", err)
	}

	var got []string
	for _, id := range sh.IDs() {
		got = append(got, id.Bare())
	}
	want := []string{"B3", "F1", "F2"}
	if len(got) != len(want) {
		t.Fatalf("shelf ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shelf ids = %v, want %v", got, want)
		}
	}

	path, ok := sh.Path(ID{Prefix: "F", Num: 2})
	if !ok || filepath.Base(path) != "F02-second.md" {
		t.Errorf("Path(F2) = %q, %v; want the F02 file", path, ok)
	}
}

// A stem with no slug still spends its id. The corpus rejects that filename,
// but the shelf exists to stop the id being reissued, and being strict here
// would let exactly that happen.
func TestShelfCountsAStemWithNoSlug(t *testing.T) {
	sh, err := LoadShelf(shelfDir(t, map[string]string{"F07.md": ""}), []string{"F"})
	if err != nil {
		t.Fatalf("LoadShelf: %v", err)
	}
	if got := sh.Highest("F"); got != 7 {
		t.Errorf("Highest(F) = %d, want 7", got)
	}
}

// An unconfigured or absent shelf is the ordinary state of every project that
// predates the key, so it must load empty rather than fail.
func TestShelfIsEmptyWhenUnconfiguredOrAbsent(t *testing.T) {
	for name, dir := range map[string]string{
		"unconfigured": "",
		"absent":       filepath.Join(t.TempDir(), "no-such-directory"),
	} {
		sh, err := LoadShelf(dir, []string{"F"})
		if err != nil {
			t.Fatalf("%s: LoadShelf: %v", name, err)
		}
		if len(sh.IDs()) != 0 || sh.Highest("F") != 0 {
			t.Errorf("%s: shelf is not empty", name)
		}
		if _, ok := sh.Path(ID{Prefix: "F", Num: 1}); ok {
			t.Errorf("%s: shelf claims to hold F1", name)
		}
	}
}

// Every command builds a session, and the ones that never consult a shelf leave
// the field nil. A nil-receiver panic there would fail commands that have
// nothing to do with the feature.
func TestNilShelfAnswersAsEmpty(t *testing.T) {
	var sh *Shelf
	if sh.Highest("F") != 0 || len(sh.IDs()) != 0 {
		t.Fatal("nil shelf did not answer as empty")
	}
	if _, ok := sh.Path(ID{Prefix: "F", Num: 1}); ok {
		t.Fatal("nil shelf claims to hold F1")
	}
}
