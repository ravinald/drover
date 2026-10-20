// Package projects records which repositories drover has been pointed at.
//
// It is a per-user audit trail, not configuration: nothing reads it to decide
// behavior, and deleting the file changes what no command does. It exists so
// that the question "what is drover driving on this machine" has an answer that
// does not depend on remembering, and so that a change touching every project
// has a list of trees to apply it to.
//
// Append-only JSONL, for the reasons the ledger is: it stays diffable, a second
// registration of one root is a row rather than an edit, and a shell can append
// to it with no binary and no network.
package projects

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FileName sits beside the per-user config rather than inside it. The config is
// layered over every project's own, so a key there would become a project key
// too, and a list of projects is meaningless read from inside one of them.
const FileName = "projects.jsonl"

// Row is one registration.
type Row struct {
	Project string `json:"project"`
	Root    string `json:"root"`
	// Repo is "owner/name" when the project named one, empty otherwise. It is
	// recorded because the tracker is the half of a project that lives outside
	// the tree, and a root that has been deleted still had one.
	Repo       string `json:"repo,omitempty"`
	Registered string `json:"registered"`
	// Forgotten marks a tombstone: `projects --forget` appends one rather than
	// deleting the root's rows, so the record that drover was pointed there
	// survives the root leaving every list.
	Forgotten bool `json:"forgotten,omitempty"`
	// Parent is the root of the registered project this root is a git
	// worktree of, recorded when that project was registered first. A reader
	// checks again for a row without it, since either may register first.
	Parent string `json:"parent,omitempty"`
}

// Path is the file, beside the per-user config. Empty when there is no home
// directory to resolve, which drover treats as "no registry" rather than as an
// error: the file is optional and nothing reads it to decide anything.
func Path(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), FileName)
}

// Read returns every row in file order. A missing file is an empty registry, and
// a line that does not parse is skipped rather than fatal: this is a record of
// what happened, and one corrupt line must not hide the rest of the history.
func Read(path string) ([]Row, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path) // #nosec G304 -- the path is derived from the config location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []Row
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Row
		if json.Unmarshal(line, &r) != nil || r.Root == "" {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Latest folds the rows to one per root, newest wins, sorted by project name.
// A root registered twice is one project somebody ran init in again, not two,
// and a root whose newest row is a tombstone is left out.
func Latest(rows []Row) []Row {
	var out []Row
	for _, r := range Fold(rows) {
		if !r.Forgotten {
			out = append(out, r)
		}
	}
	return out
}

// Fold is Latest with the tombstones kept: the newest row of every root the
// registry ever named, sorted by project name.
func Fold(rows []Row) []Row {
	byRoot := map[string]Row{}
	for _, r := range rows {
		byRoot[r.Root] = r
	}
	out := make([]Row, 0, len(byRoot))
	for _, r := range byRoot {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Root < out[j].Root
	})
	return out
}

// Registered reports whether root is in the registry and not forgotten, which
// is the question a command asks before registering the project it runs in.
func Registered(rows []Row, root string) bool {
	in := false
	for _, r := range rows {
		if r.Root == root {
			in = !r.Forgotten
		}
	}
	return in
}

// Append records one registration. It never fails the caller's real work: a
// registry that cannot be written is a lost line in an audit trail, not a
// reason to refuse to create a project.
func Append(path string, r Row) error {
	if path == "" {
		return nil
	}
	if r.Registered == "" {
		r.Registered = time.Now().Format("2006-01-02")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, "%s\n", line); err != nil {
		return err
	}
	return f.Sync()
}
