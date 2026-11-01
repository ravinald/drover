package prompts

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateHistory = flag.Bool("update-history", false, "append each changed block default to history.json")

// A default that changes without its predecessor kept leaves every override
// written against the old one with a hash nothing can resolve, so check can
// say that the default moved and never show how.
func TestEveryDefaultIsInHistory(t *testing.T) {
	hist := History()
	changed := false
	for _, slot := range Slots {
		for _, b := range Blocks(slot) {
			if b.Addition {
				continue
			}
			key := slot + "/" + b.Name
			past := hist[key]
			if len(past) > 0 && past[len(past)-1].Hash == b.Hash {
				continue
			}
			if !*updateHistory {
				t.Errorf("%s's default (%s) is not the last entry in history.json; run "+
					"go test ./internal/prompts -run TestEveryDefaultIsInHistory -update-history and commit the result", key, b.Hash)
				continue
			}
			hist[key] = append(past, Past{Hash: b.Hash, Text: b.Default})
			changed = true
		}
	}
	if !changed {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(hist); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("history.json", buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEarlierResolvesARecordedBase(t *testing.T) {
	b := blockByName(t, SlotImplement, "scope")
	text, ok := Earlier(SlotImplement, "scope", b.Hash)
	if !ok || text != b.Default {
		t.Errorf("Earlier(implement, scope, %s) = %q, %v; want the current default", b.Hash, text, ok)
	}
	if _, ok := Earlier(SlotImplement, "scope", "000000000000"); ok {
		t.Error("Earlier resolved a hash no default ever had")
	}
}
