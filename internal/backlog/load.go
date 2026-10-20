package backlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Broken is a file in the bodies directory that did not parse.
//
// These are carried rather than dropped: a skipped row would make the
// completion count read as a finished backlog, which is the one reporting error
// this tool must never make.
type Broken struct {
	Path string
	Err  error
}

// ID recovers the id from the filename, which is where it was authored. A body
// that will not parse still spends its id, so anything asking "what is on disk"
// — allocation, orphan and dep checks — must read it from here rather than from
// the parsed items.
func (b Broken) ID(prefixes []string) (ID, bool) {
	stem, _, _ := strings.Cut(strings.TrimSuffix(filepath.Base(b.Path), ".md"), "-")
	id, err := ParseStem(stem, prefixes)
	return id, err == nil
}

// Backlog is the backlog as read from disk.
type Backlog struct {
	Dir    string
	Items  []Item
	Broken []Broken
	// Missing records that Dir does not exist. It is carried rather than
	// returned as an error so that check can report it as a finding instead of
	// dying on the fault it exists to diagnose, and it stays distinct from an
	// empty directory: a misspelled paths.backlog and a project nobody has filed
	// against yet need different fixes.
	Missing bool
}

// Find returns the item with the given id.
func (c *Backlog) Find(id ID) (Item, bool) {
	for _, it := range c.Items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// FindBare resolves a bare id string.
func (c *Backlog) FindBare(bare string, prefixes []string) (Item, bool) {
	id, err := ParseID(bare, prefixes)
	if err != nil {
		return Item{}, false
	}
	return c.Find(id)
}

// Load reads every item in dir. The directory listing is the registry, so
// adding an item needs no change to any index.
func Load(dir string, prefixes []string, schema Schema) (*Backlog, error) {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return &Backlog{Dir: dir, Missing: true}, nil
	case err != nil:
		// A permission or I/O failure is not the same as absence: reporting it as
		// an empty backlog would let a completion count read as finished.
		return nil, fmt.Errorf("read corpus %s: %w", dir, err)
	}

	c := &Backlog{Dir: dir}
	seen := map[ID]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		base := strings.TrimSuffix(e.Name(), ".md")
		stem, slug, ok := strings.Cut(base, "-")
		if !ok || slug == "" {
			c.Broken = append(c.Broken, Broken{path, fmt.Errorf("filename is not <id>-<slug>.md")})
			continue
		}
		id, err := ParseStem(stem, prefixes)
		if err != nil {
			c.Broken = append(c.Broken, Broken{path, err})
			continue
		}
		if prev, dup := seen[id]; dup {
			c.Broken = append(c.Broken, Broken{path, fmt.Errorf("id %s already used by %s", id.Bare(), filepath.Base(prev))})
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			c.Broken = append(c.Broken, Broken{path, err})
			continue
		}
		meta, text, err := ParseFrontmatter(path, data, schema)
		if err != nil {
			// The id is still known, so a broken body keeps its place in the
			// table instead of vanishing from the count.
			c.Broken = append(c.Broken, Broken{path, err})
			continue
		}
		seen[id] = path
		c.Items = append(c.Items, Item{ID: id, Slug: slug, Path: path, Meta: meta, Body: text})
	}

	sort.Slice(c.Items, func(i, j int) bool { return c.Items[i].ID.Less(c.Items[j].ID) })
	sort.Slice(c.Broken, func(i, j int) bool { return c.Broken[i].Path < c.Broken[j].Path })
	return c, nil
}

// MissingDeps returns the ids this item depends on that are not yet done.
func MissingDeps(it Item, done func(string) bool) []string {
	var missing []string
	for _, d := range it.Deps() {
		if !done(d) {
			missing = append(missing, d)
		}
	}
	return missing
}
