package backlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Shelf is a directory of retired bodies: items written, reviewed and
// deliberately not going to be done, moved out of the backlog rather than
// deleted so the text and the id survive.
//
// It is read for ids and never for content. Nothing on it renders, enters the
// graph or affects a count, so a file here with broken frontmatter, a missing
// required key or a stop marker is not a finding — nothing is going to run it.
// The two questions it answers are the two where a missing id yields a wrong
// answer rather than a smaller one: what id to allocate next, and whether a
// ledger row's body is gone or merely moved.
type Shelf struct {
	Dir string
	// Missing records that a configured Dir does not exist. Without it a typo in
	// paths.shelf is indistinguishable from an unset key, and a shelf drover
	// could not open silently restores every behavior the shelf prevents:
	// ORPHAN ROW returns for shelved ids and intake reissues them.
	Missing bool
	ids     map[ID]string
}

// LoadShelf reads ids from the filenames in dir.
//
// An unnamed or absent directory yields an empty shelf rather than an error:
// the key is optional, and a project that has not configured one must behave
// exactly as it did before the key existed. An absent one sets Missing, which
// is the difference between the two that check reports.
func LoadShelf(dir string, prefixes []string) (*Shelf, error) {
	sh := &Shelf{Dir: dir, ids: map[ID]string{}}
	if dir == "" {
		return sh, nil
	}
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		sh.Missing = true
		return sh, nil
	case err != nil:
		return nil, fmt.Errorf("read shelf %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".md")
		// The slug is optional here where it is mandatory in the backlog: a stem
		// with no slug still spends an id, and the shelf exists to stop that id
		// being reissued. A name that carries no id at all — the README a project
		// keeps beside the files — is skipped in silence.
		stem, _, _ := strings.Cut(base, "-")
		id, err := ParseStem(stem, prefixes)
		if err != nil {
			continue
		}
		if _, dup := sh.ids[id]; dup {
			continue
		}
		sh.ids[id] = filepath.Join(dir, e.Name())
	}
	return sh, nil
}

// Path returns where a shelved id's body sits.
func (s *Shelf) Path(id ID) (string, bool) {
	if s == nil {
		return "", false
	}
	path, ok := s.ids[id]
	return path, ok
}

// IDs returns every shelved id in backlog order.
func (s *Shelf) IDs() []ID {
	if s == nil {
		return nil
	}
	out := make([]ID, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// Highest returns the largest number a shelved id holds in this namespace, or
// zero when the shelf holds none.
func (s *Shelf) Highest(prefix string) int {
	if s == nil {
		return 0
	}
	highest := 0
	for id := range s.ids {
		if id.Prefix == prefix && id.Num > highest {
			highest = id.Num
		}
	}
	return highest
}
