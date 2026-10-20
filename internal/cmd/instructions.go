package cmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// agentsHost is a markdown file at the project root carrying the generated
// block, found by its markers rather than by name. The filename is the
// operator's choice — `agents --write` takes any path, and DROVER.md is a
// suggestion — so a check keyed on "AGENTS.md" would report nothing about the
// projects that took the suggestion.
type agentsHost struct {
	// Name is the file, project-relative, as a finding should name it.
	Name string
	// Block is the text between the markers, exclusive, trimmed. Empty when the
	// closing marker sits above the opening one, which writeAgents refuses to
	// repair and this refuses to interpret.
	Block string
	// Malformed records that inverted-marker state, so a caller reports a file
	// nobody can regenerate rather than silently calling its block stale.
	Malformed bool
}

// blockSpan is where the generated block sits in a file, as line indexes of the
// two marker lines.
//
// One locator for both sides of the question. writeAgents decides what to
// replace and check decides what to compare, and answering that twice is how
// `drover check` came to call a file clean while `agents --write` spliced into
// the middle of one of its sentences.
type blockSpan struct {
	Start, End int
	// Found reports an opening marker owning a line. False means the file holds
	// no block, whatever its prose quotes.
	Found bool
	// Malformed is an opening marker with no closing one after it. Distinct from
	// absent: appending a second block to a file already carrying half of one
	// leaves two openings and no way to tell which the next run should replace.
	Malformed bool
}

// findBlock locates the block, matching each marker the way markerLine matches
// the intake stop marker: a whole line, outside any fence. Prose quotes a
// marker mid-line or in a code block, as docs/agents.md does.
func findBlock(text string) blockSpan {
	start := markerLine(text, agentsBegin)
	if start < 0 {
		return blockSpan{}
	}
	end := markerLine(text, agentsEnd)
	if end < 0 || end < start {
		return blockSpan{Start: start, End: end, Found: true, Malformed: true}
	}
	return blockSpan{Start: start, End: end, Found: true}
}

// pointerConvention is the file an agent harness loads without being told to.
// Nothing in drover writes it, and a project keeping the block there directly
// is doing the ordinary thing rather than the wrong one.
const pointerConvention = "AGENTS.md"

// findAgentsHosts returns every root-level markdown file holding the block.
//
// A marker counts only where markerLine already counts the stop marker: a whole
// line, outside any fence. drover's own CHANGELOG.md carries
// `<!-- BEGIN drover -->` inline in the entry announcing the command, and a
// substring match reports that entry as a stale block and tells the reader to
// regenerate their changelog. Prose quotes a marker mid-line or inside a fence;
// agents --write puts it alone on its own.
func findAgentsHosts(root string) ([]agentsHost, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var hosts []agentsHost
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name())) // #nosec G304 -- a .md at the project root
		if err != nil {
			continue
		}
		text := string(b)
		span := findBlock(text)
		switch {
		case !span.Found:
			continue
		case span.Malformed:
			hosts = append(hosts, agentsHost{Name: e.Name(), Malformed: true})
		default:
			hosts = append(hosts, agentsHost{Name: e.Name(), Block: span.between(text)})
		}
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	return hosts, nil
}

// currentBlock is what `agents --write` would put between the markers now,
// trimmed to compare against what findAgentsHosts read back.
//
// The comparison is on the rendered text rather than on a version stamp: a
// block goes stale when the config changes under it just as surely as when the
// binary does, and only one of those two moves a version.
func (s *session) currentBlock() (string, error) {
	body, err := s.renderAgents()
	if err != nil {
		return "", err
	}
	span := findBlock(body)
	if !span.Found || span.Malformed {
		return strings.TrimSpace(body), nil
	}
	return span.between(body), nil
}

// referencesHost reports whether the convention file sends a reader to the file
// actually holding the block.
//
// The judge is the filename appearing anywhere in the prose, which is loose on
// purpose. Anything tighter is a claim about how the operator worded their own
// pointer, and a finding that fires on valid wording teaches people to ignore
// findings.
func referencesHost(root, host string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(root, pointerConvention)) // #nosec G304 -- a fixed name at the project root
	if err != nil {
		return false, err
	}
	return strings.Contains(string(b), host), nil
}

// between is the text the markers enclose, exclusive of the marker lines.
func (b blockSpan) between(text string) string {
	lines := strings.Split(text, "\n")
	return strings.TrimSpace(strings.Join(lines[b.Start+1:b.End], "\n"))
}
