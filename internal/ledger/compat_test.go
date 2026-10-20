package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fixture is a real ledger to check compatibility against. Point
// DROVER_LEDGER_FIXTURE at one; without it these skip.
func fixture(t *testing.T) []string {
	t.Helper()
	path := os.Getenv("DROVER_LEDGER_FIXTURE")
	if path == "" {
		t.Skip("set DROVER_LEDGER_FIXTURE to a real ledger to run")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if text := strings.TrimSpace(sc.Text()); text != "" {
			lines = append(lines, text)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// The existing ledger is the compatibility contract. drover appends and never
// rewrites, so byte-identical re-emission is not required — but modelling every
// field that is actually present is, or reading a row silently drops data the
// file still holds.
func TestExistingLedgerHasNoUnmodelledFields(t *testing.T) {
	for i, text := range fixture(t) {
		dec := json.NewDecoder(bytes.NewReader([]byte(text)))
		dec.DisallowUnknownFields()
		var r Row
		if err := dec.Decode(&r); err != nil {
			t.Errorf("line %d: %v\n  %s", i+1, err, text)
		}
	}
}

func TestExistingLedgerRoundTripsSemantically(t *testing.T) {
	lines := fixture(t)
	for i, text := range lines {
		var first Row
		if err := json.Unmarshal([]byte(text), &first); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		out, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		var second Row
		if err := json.Unmarshal(out, &second); err != nil {
			t.Fatalf("line %d re-parse: %v", i+1, err)
		}
		// Compare the encodings, not the structs: Done is a pointer, so struct
		// equality would compare addresses.
		again, err := json.Marshal(second)
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if !bytes.Equal(out, again) {
			t.Errorf("line %d does not survive a round trip\n  %s\n  %s", i+1, out, again)
		}
	}
	t.Logf("%d rows round-trip", len(lines))
}

// Every id must resolve without losing its completion to a later audit row.
func TestExistingCorpusResolves(t *testing.T) {
	lines := fixture(t)
	l := &Ledger{}
	for i, text := range lines {
		var r Row
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		l.Rows = append(l.Rows, r)
	}
	var done, audited, byUnknown int
	for _, id := range l.IDs() {
		s := l.State(id)
		if s.Merged {
			done++
		}
		if s.Audited() {
			audited++
			if s.CodeBy == "" {
				byUnknown++
			}
		}
	}
	t.Logf("%d ids · %d done · %d audited · %d of those with unknown authorship",
		len(l.IDs()), done, audited, byUnknown)
	if done == 0 {
		t.Fatal("no completions resolved from a ledger that has them")
	}
}
