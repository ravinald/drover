package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
)

// The trail is every earlier request-changes round, kept because the section they were written
// into is replaced on every pass.
//
// The ledger cannot answer this. Its changes row carries a 12-hex digest of the
// evidence, which tells a review repeating itself from one finding something
// new and tells nobody what either round was about. The item file's own history
// holds the prose, but a backlog kept outside git, or in a repository of its own,
// has that history somewhere no brief can reach by a path.
//
// What the trail buys is the progression. Five rounds reading "record the cache
// hit", "persist by key", "compare the metadata", "take it from the open
// handle", "publish by rename" are one contract nobody has stated, and that is
// visible in the list and in nothing else: each round on its own reads as a
// clean fix to a real defect, which is how five of them can merge.
//
// Headlines, never bodies. A closed finding rendered whole reads as an open one,
// and a run that re-verifies finished work is the failure the replaced section
// exists to prevent. The path is there for a reader who wants the round.

// roundName matches a kept round and yields its number.
var roundName = regexp.MustCompile(`\.changes-(\d+)\.md$`)

// keepRound writes one round's evidence beside the saved reports and answers where
// it landed. The number is the count already on disk plus one, so a hand-deleted
// round leaves a gap rather than renumbering the ones around it.
func (s *session) keepRound(it backlog.Item, text string) (string, error) {
	rounds, err := s.rounds(it)
	if err != nil {
		return "", err
	}
	n := 1
	for _, r := range rounds {
		if r.Number >= n {
			n = r.Number + 1
		}
	}
	path := s.roundPath(it, n)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	header := fmt.Sprintf("# %s changes %d · %s\n\n", it.ID.Bare(), n, ledger.Today())
	if err := os.WriteFile(path, []byte(header+strings.TrimSpace(text)+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (s *session) roundPath(it backlog.Item, n int) string {
	return s.cfg.Abs(filepath.Join(s.cfg.Paths.Reports,
		fmt.Sprintf("%s-%s.changes-%d.md", it.ID.Stem(), it.Slug, n)))
}

// rounds reads the kept rounds for one item, oldest first. A directory that is
// not there is an item nobody has sent back, not an error.
func (s *session) rounds(it backlog.Item) ([]prompts.Round, error) {
	dir := s.cfg.Abs(s.cfg.Paths.Reports)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := it.ID.Stem() + "-" + it.Slug + "."
	var out []prompts.Round
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		m := roundName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		r := prompts.Round{Number: n, Path: rel(s.cfg.Root, path)}
		r.Date, r.Headline = readRound(path)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// readRound answers the date in a kept round's header and the first line of its
// evidence. A file drover cannot read leaves both empty, which the brief renders
// as the round it could not summarize rather than as a round that never
// happened.
func readRound(path string) (date, headline string) {
	raw, err := os.ReadFile(path) // #nosec G304 — drover's own reports directory
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "#"):
			if _, d, ok := strings.Cut(t, "·"); ok && date == "" {
				date = strings.TrimSpace(d)
			}
		default:
			return date, summarize(t)
		}
	}
	return date, ""
}

// markup is the emphasis and list punctuation a headline carries when the
// evidence opens with a bullet or a number, which is how findings get written.
// The kept round holds what the reviewer handed in, so the numbering `request-changes`
// demotes on its way into the body is still here.
var markup = regexp.MustCompile(`^([-*]|\d+\.)\s+|\*\*|__|` + "`")

// summarize is one line of evidence as a headline: its markup stripped and its
// length bounded, because a brief lists these and one paragraph among five
// one-liners reads as the only round that matters.
func summarize(line string) string {
	out := strings.TrimSpace(markup.ReplaceAllString(line, ""))
	const limit = 96
	// Counted and cut in runes. A byte cut lands mid-rune on the first headline
	// that quotes a path with an accent in it, and the brief then carries a
	// replacement character where the evidence was.
	r := []rune(out)
	if len(r) <= limit {
		return out
	}
	cut := strings.LastIndex(string(r[:limit]), " ")
	if cut < 0 {
		return strings.TrimRight(string(r[:limit]), " ,.;:") + "…"
	}
	return strings.TrimRight(out[:cut], " ,.;:") + "…"
}
