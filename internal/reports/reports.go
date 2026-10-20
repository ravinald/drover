// Package reports snapshots what a run claimed, beside a checklist for
// confirming it.
//
// Capture happens when the run ends, not when the work merges: the run log is
// the only copy and it lives in a cache directory, while merging can be days
// later, by which point a cache sweep has taken the self-report with it.
package reports

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/backlog"
)

// NoResult stands in for a self-report a log never produced. It doubles as the
// marker Write greps to decide an existing file is a clobberable stub.
const NoResult = "_(no result event in the log — the run may have been interrupted)_"

// Data is what a result records.
type Data struct {
	ID       string
	Title    string
	Slug     string
	Meta     map[string]string
	LogPath  string
	Headline string
	// Runner is the runner that produced this report, from the recorded run.
	// Empty means no run was recorded, which is rendered as unknown: the item's
	// frontmatter says who was asked for, never who answered.
	Runner string
	// RunnerKey is the frontmatter key that pins a runner per item, so the
	// metadata block can label it as the request it is rather than let it read
	// as the answer.
	RunnerKey string
	// Report is the agent's own account of what it did.
	Report string
	// Body is the item text, mined for its numbered requirements.
	Body string
}

// numbered matches a top-level "N. " line. The numbering is load-bearing: a
// requirement that is not a numbered line vanishes from the checklist, so an
// author who writes prose instead gets no audit row for it.
var numbered = regexp.MustCompile(`(?m)^(\d+)\.[ \t]+(.*)$`)

// Requirements extracts one checklist entry per numbered requirement.
func Requirements(body string) []string {
	matches := numbered.FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		text := strings.TrimSpace(m[2])
		// Strip markdown emphasis so the checklist reads as a list of claims.
		if rest, op := backlog.CutOperator(text); op {
			out = append(out, fmt.Sprintf("- [ ] R%s (operator): %s", m[1], rest))
			continue
		}
		text = strings.TrimPrefix(text, "**")
		out = append(out, fmt.Sprintf("- [ ] R%s: %s", m[1], text))
	}
	return out
}

// Render builds the file contents.
func Render(d Data) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", d.ID, d.Title)
	fmt.Fprintf(&b, "- Item %s · slug `%s`\n", d.ID, d.Slug)
	for _, k := range sortedKeys(d.Meta) {
		v := d.Meta[k]
		if v == "" || k == "title" {
			continue
		}
		if k == d.RunnerKey && d.RunnerKey != "" {
			fmt.Fprintf(&b, "- %s (requested): %s\n", k, v)
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", k, v)
	}
	runner := d.Runner
	if runner == "" {
		runner = "unknown (no run recorded)"
	}
	fmt.Fprintf(&b, "- Runner %s\n", runner)
	fmt.Fprintf(&b, "- Captured %s\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(&b, "- Source log `%s`\n", d.LogPath)
	if d.Headline != "" {
		fmt.Fprintf(&b, "- Outcome %s\n", d.Headline)
	}

	report := d.Report
	if strings.TrimSpace(report) == "" {
		report = NoResult
	}
	fmt.Fprintf(&b, "\n## Report (self-reported)\n\n%s\n\n", report)

	fmt.Fprintf(&b, "## Validation addendum\n")
	fmt.Fprintf(&b, "_Confirm each requirement against the merged code, not against the summary above._\n\n")
	checks := Requirements(d.Body)
	if len(checks) == 0 {
		fmt.Fprintf(&b, "- [ ] (no numbered requirements found — validate against the spec)\n")
	} else {
		fmt.Fprintf(&b, "%s\n", strings.Join(checks, "\n"))
	}
	fmt.Fprintf(&b, "\n**Divergences:**\n\n**Fix:**\n\n**Verify re-run:**\n\n**Validated by / date:**\n")
	return b.String()
}

// Write saves a result, refusing to overwrite one that already exists.
//
// The one exception is a stub from an interrupted run: it carries no
// self-report and no filled addendum, and keeping it would lock out the result
// of the successful re-run that follows. Any other existing file is somebody's
// work.
func Write(path string, d Data) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(existing), NoResult) {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create report directory: %w", err)
	}
	// Temp and rename: a truncating write interrupted halfway leaves a file that
	// matches no stub marker and can therefore never be replaced.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.WriteString(Render(d)); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, fmt.Errorf("write report %s: %w", path, err)
	}
	return true, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
