// Package register reads and writes a project's follow-up register as a local
// file.
//
// It is the second source behind gh: a project on GitLab, on a self-hosted
// forge or in a bare repository still accrues follow-ups, and without this the
// only place to record one is a tracker it does not have. Everything above the
// read path — routing, severity ranking, dormancy, the counts in status and
// unaudited —
// reads gh.Index and nothing else, so this returns one rather than growing a
// second register with its own vocabulary to drift from.
//
// The file is JSONL and append-only, for the reason the ledger is: it lives in
// a git-tracked documentation tree whose purpose is auditable history, closing
// a row must not rewrite the line that opened it, and an agent with no binary
// can record one by appending a line.
package register

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/gh"
)

// The states a row can declare. A row carrying anything else is malformed: the
// fold decides what is open by reading this field, so a typo would otherwise
// hide a row by making it neither open nor closed.
const (
	StateOpen   = "open"
	StateClosed = "closed"
)

// Row is one event about one register entry. Number keys the fold; every other
// field is the state that number is in as of this line.
//
// Owners carries bare ids, the same values the tracker's route labels hold once
// the prefix is stripped. Membership needs no field here: a row is in the file
// or it is not, which is the explicit form of what the tracker spends a label
// saying.
type Row struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	Severity string   `json:"severity,omitempty"`
	Owners   []string `json:"owners,omitempty"`
	State    string   `json:"state"`
	Date     string   `json:"date"`
}

// File is one parsed register.
type File struct {
	Path string
	// Rows are every event in file order. Newest wins per number.
	Rows []Row
	// Max is the highest number the file has ever used, closed rows included.
	// Reusing a closed number would splice the history of two entries into one.
	Max int
}

// Read parses the file. A missing or empty one is an empty register, not an
// error: a project that has filed nothing yet is the ordinary first-day state,
// and reporting it as unreadable hides work in the direction that matters.
//
// A malformed line fails the whole read. The alternative is answering with the
// rows that happened to parse, which reads as a complete register and is the
// one answer a caller cannot check.
func Read(path string) (File, error) {
	f := File{Path: path}
	fh, err := os.Open(path) // #nosec G304 -- the path comes from the project's own config
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		// Unwrapped: os.Open fails with a *PathError, which already renders
		// "open <path>: <reason>". Naming the file again produced "open p: open
		// p: permission denied" on the BAD REGISTER line somebody reads at
		// 03:00. Every os call below is returned the same way for that reason.
		return f, err
	}
	defer func() { _ = fh.Close() }()

	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var r Row
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			return File{Path: path}, fmt.Errorf("%s:%d is not a register row: %w", path, n, err)
		}
		if err := r.validate(); err != nil {
			return File{Path: path}, fmt.Errorf("%s:%d %w", path, n, err)
		}
		if r.Number > f.Max {
			f.Max = r.Number
		}
		f.Rows = append(f.Rows, r)
	}
	if err := sc.Err(); err != nil {
		return File{Path: path}, err
	}
	return f, nil
}

func (r Row) validate() error {
	if r.Number <= 0 {
		return fmt.Errorf("has no number: every row keys on one")
	}
	if r.State != StateOpen && r.State != StateClosed {
		return fmt.Errorf("#%d has state %q (want %q or %q)", r.Number, r.State, StateOpen, StateClosed)
	}
	return nil
}

// Open folds the events newest-wins per number and drops anything whose latest
// state is closed, sorted by number so two reads of one file agree.
func (f File) Open() []Row {
	latest := map[int]Row{}
	for _, r := range f.Rows {
		latest[r.Number] = r
	}
	out := make([]Row, 0, len(latest))
	for _, r := range latest {
		if r.State == StateClosed {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// Load reads the file as the index every consumer already speaks.
//
// Unpaged is set because the whole file was read: a register holding exactly
// page_limit rows is not a page that ran out, and the truncation notice would
// otherwise fire on a source that has no pages.
func Load(path string) gh.Index {
	f, err := Read(path)
	if err != nil {
		return gh.Index{Err: err}
	}
	ix := gh.Index{OK: true, Unpaged: true}
	for _, r := range f.Open() {
		ix.Issues = append(ix.Issues, gh.Issue{
			Number:   r.Number,
			Title:    r.Title,
			State:    r.State,
			Owners:   r.Owners,
			Severity: r.Severity,
		})
	}
	return ix
}

// Append writes one event. The date is stamped here when the caller left it
// empty, so provenance never depends on remembering to set it.
func Append(path string, r Row) error {
	if r.Date == "" {
		r.Date = time.Now().Format("2006-01-02")
	}
	if err := r.validate(); err != nil {
		return fmt.Errorf("register row %w", err)
	}
	// Encoding through the marshaller, never through string formatting: a title
	// carrying a quote or a backslash would otherwise produce a line that parses
	// as nothing and fails every later read.
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode row: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create register directory: %w", err)
	}
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the path comes from the project's own config
	if err != nil {
		return err
	}
	// Sync below is what makes the write survive a crash, so a Close error
	// adds nothing.
	defer func() { _ = fh.Close() }()
	if _, err := fh.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append to %s: %w", path, err)
	}
	return fh.Sync()
}
