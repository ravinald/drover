package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/gh"
)

// PendingRow is one tracker issue drover could take in but has not, and the
// unit of --json.
//
// Owners is present because a routed issue is the common case rather than the
// exception: a defect found during an audit is filed against the item that owns
// the gap, and it sits here until somebody decides it is worth its own item.
type PendingRow struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	// Severity is the tracker's own rank, which intake would seed the
	// scaffold's frontmatter from.
	Severity string   `json:"severity,omitempty"`
	Owners   []string `json:"owners"`
}

// listIntake is the import queue: every open issue carrying a label that matches
// an id namespace, minus the ones a body already claims. It is not the register:
// a row here is work somebody is going to do, and a row in follow-ups is a
// follow-up nobody has priced.
func listIntake() error {
	s, err := open()
	if err != nil {
		return err
	}
	if backend := s.cfg.RegisterBackend(); backend != config.BackendGitHub {
		return Refusal{fmt.Errorf(
			"%s declares %s, so there is no tracker to take anything from.\n"+
				"Hand-write the body instead: drover only ever reads the file",
			s.cfg.Project, s.cfg.RegisterDeclaration())}
	}
	index := gh.LoadTracker(s.labels())
	if !index.OK {
		return index.Reason()
	}

	byKind := map[string]string{}
	for prefix, kind := range s.cfg.IDs.Namespaces {
		byKind[kind] = prefix
	}

	rows := []PendingRow{}
	for _, is := range index.Issues {
		kind := matchedKind(is.Labels, byKind)
		if kind == "" {
			continue
		}
		if _, _, claimed := s.itemForIssue(is.Number); claimed {
			continue
		}
		owners := is.Owners
		if owners == nil {
			owners = []string{}
		}
		rows = append(rows, PendingRow{
			Number: is.Number, Title: is.Title, Kind: kind,
			Severity: is.Severity, Owners: owners,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Number < rows[j].Number })

	if intakeJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Printf("nothing pending: every open issue matching a namespace (%s) is already an item\n",
			strings.Join(sortedKeys(byKind), ", "))
		return nil
	}
	fmt.Println("pending intake — open issues drover could take in:")
	width := titleWidth()
	for _, r := range rows {
		sev := r.Severity
		if sev == "" {
			sev = "-"
		}
		owned := ""
		if len(r.Owners) > 0 {
			owned = "  (owns: " + strings.Join(r.Owners, " ") + ")"
		}
		fmt.Printf("  %s [%s] %-11s %s%s\n", padRef(s.issueRefTo(os.Stdout, r.Number), r.Number), sev, r.Kind, trunc(r.Title, width), owned)
	}
	fmt.Printf("\n%d pending  ·  take one in:  drover intake %d\n", len(rows), rows[0].Number)
	return nil
}

// matchedKind is intake's namespace inference reduced to the half --list
// needs: which declared kind an issue carries, strictest first.
//
// It shares namespaceFor's tie-break — the kind that sorts first wins, since a
// bug carries a stricter definition of done than an enhancement — so the kind
// shown here is the one intake would allocate against.
func matchedKind(labels []string, byKind map[string]string) string {
	var matched []string
	for _, l := range labels {
		if _, ok := byKind[l]; ok {
			matched = append(matched, l)
		}
	}
	if len(matched) == 0 {
		return ""
	}
	sort.Strings(matched)
	return matched[0]
}
