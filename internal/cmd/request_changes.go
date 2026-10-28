package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/ravinald/drover/internal/prompts"
	"github.com/spf13/cobra"
)

var (
	requestChangesFile      string
	requestChangesOverLimit bool
	requestChangesYes       bool
	requestChangesStillOpen []int
	requestChangesOperator  []int
)

var requestChangesCmd = &cobra.Command{
	Use:   "request-changes <id> -f <file>",
	Short: "Write what a review sent back into the item body.",
	Long: `A review routes a finding back to the branch, and the operator re-runs the item.
What that run reads is the item body and nothing else: not the reviewer's
report, not the audit note, not the conversation the review happened in. So a
gap recorded only in those is a gap the next run rediscovers or ships again.

request-changes is the step that closes that. It replaces the ` + "`" + prompts.ChangesHeading + "`" + `
section with the evidence you hand it, appending the section when the body has
none, and it touches nothing else in the file.

What it will not do is as much of the point as what it does. The numbered
requirements are the contract a re-run is audited against, and a reviewer who can
edit them while auditing against them is auditing a target they moved: request-changes
writes below the heading only, and refuses the write outright if the requirement
count would change either way.

It also demotes a top-level "N." line in your evidence to a bullet. Requirements
are parsed as top-level numbered lines wherever they appear in the body, so a
numbered list of findings silently becomes requirement 8 — which has happened,
and the prompt then carries three findings dressed as contract.

Nor will it send work back on an ` + "`" + backlog.OperatorTag + "`" + ` requirement alone. A finding is a heading in
your evidence, traced through the R<n> it names; when every finding traces to
an [operator] requirement, request-changes refuses and names them, since no session can
meet one. The item goes to its pull request with them pending, and drive holds
the merge until drover signoff records each.

The section is replaced rather than appended to. The body is a prompt, not a
history: a run reads it as current instructions and cannot tell a closed finding
from an open one.

Nor will it write evidence that describes a contract the body does not hold.
Evidence naming a requirement by number (R9, "requirement 9") the body's list
lacks is refused, and so is evidence saying requirements were removed or
replaced when no drover amend has changed the list since the last round. Both
are a heuristic over prose, read outside code fences: a prompt to look, not a
proof. Amend the contract first, or correct the evidence. With --over-limit,
which only the operator can pass, it warns instead and writes once --yes says
you looked.

The replaced text is not lost. Each round is kept beside the saved reports as
<stem>-<slug>.changes-N.md, and both prompts list the earlier ones as headlines
with the path. That list is the only thing that says whether five rounds closed
five defects or circled one guarantee nobody wrote down: on its own, each round
reads as a clean fix to a real gap, which is how five of them ship.

--still-open names the requirements whose finding from the previous request
you reproduced as still open, rather than a new defect found under the same
number. The changes row records the list as still_open, and drive parks the
item repeating once two rows in a row mark the same requirement. Only the
reviewer can tell a carried-over finding from a new one, so drover never
infers it from the numbers alone. It refuses a number the previous request did
not name, and refuses the flag on a first request.

--operator names the requirements whose finding no implementing session can
meet as written: one that depends on a release upstream never made, on a
decision nobody has taken, or on evidence no source publishes. The reason goes
in the evidence under a heading naming that requirement. Each number must name
a requirement in the body's list and a finding in the evidence, or the request
is refused. The changes row records them as operator, and drive parks the item
needs_decision rather than sending it back to a session that cannot discharge
it. Findings against other requirements in the same evidence are written to the
body as always, for the run after the operator amends the contract or signs the
requirement off.`,
	Example: `  drover request-changes B1 -f findings.md
  drover review B1 | ...                       # write the evidence, then:
  cat findings.md | drover request-changes B1 -f -      # or from stdin
  drover request-changes B1 -f findings.md --still-open 3   # R3's earlier finding reproduced
  drover request-changes B1 -f findings.md --operator 3     # R3 waits on a decision, not a run

  Nothing enforces that a review requests changes. drover cannot tell a pass that found
  nothing from one that found something and did not write it down, so the
  instruction lives in the review prompt and this is the command behind it.

Safety: local writes. Rewrites one section of the item body, keeps the evidence
under [paths] reports, appends a body digest row and one changes row, refuses to
change the numbered requirements, refuses evidence whose only findings are
[operator] requirements, and reaches no tracker.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		if requestChangesFile == "" {
			return Refusal{fmt.Errorf("request-changes needs the evidence: -f <file>, or -f - for stdin")}
		}
		text, err := readEvidence(requestChangesFile, os.Stdin)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return Refusal{fmt.Errorf("%s is empty: a request for changes with no evidence tells the next run nothing", requestChangesFile)}
		}

		stillOpen, err := s.stillOpenAgainstLast(it.ID.Bare(), requestChangesStillOpen)
		if err != nil {
			return err
		}
		operator, err := operatorAgainstBody(it, text, requestChangesOperator)
		if err != nil {
			return err
		}

		// Before the cap, because this refusal is the more specific one: a round
		// past the cap is the operator's call, and a round on an [operator]
		// requirement is nobody's.
		if ops, structure := operatorOnlyFindings(it, text); len(ops) > 0 {
			return Refusal{operatorRefusal(it.ID.Bare(), ops, structure)}
		}

		// After the usage checks and before the write, so request-changes with no -f past
		// the cap reports the missing flag rather than the cap: the caller can
		// act on the first, and the second is not their decision to make.
		if limit := s.cfg.Review.MaxReviewRounds; limit > 0 {
			if n := s.ledger.Counts(it.ID.Bare()).ChangesRequested; n >= limit {
				if !requestChangesOverLimit {
					// Recorded before the refusal, because the refusal is the
					// evidence. Without a row, a review that wanted to send work
					// back and was turned away leaves an audit and no request for changes row,
					// which is what a review that found nothing leaves; passResult
					// would then have to guess, and would guess at_limit for every
					// later pass on the item. A failure to write the row does not
					// swallow the refusal: the caller still gets told no, and the
					// worst case is that ambiguity rather than a request that appears
					// to have been recorded.
					if err := s.record(ledger.Row{
						ID: it.ID.Bare(), Refused: "max_review_rounds", Reqs: findingRequirements(it, text), Date: ledger.Today(),
					}); err != nil {
						fmt.Fprintf(errOut, "warn: the cap refusal went unrecorded: %v\n", err)
					}
					return Refusal{s.capRefusal(it.ID.Bare(), n, limit)}
				}
				if err := sealedFlag("request-changes", "over-limit",
					"the cap bounds what a review may spend, and this session is the party it bounds"); err != nil {
					return err
				}
				fmt.Printf("  ! %s is at [review] max_review_rounds (%d of %d) and --over-limit sent it back anyway.\n",
					it.ID.Bare(), n, limit)
			}
		}

		if why := s.contractContradictions(it, text); len(why) > 0 {
			if err := contradictionRefusal(it.ID.Bare(), why, requestChangesOverLimit, requestChangesYes); err != nil {
				return err
			}
		}

		raw, err := os.ReadFile(it.Path)
		if err != nil {
			return err
		}
		head, body, err := splitFrontmatter(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", it.Path, err)
		}

		section, demoted, sunk := demoteEvidence(strings.TrimSpace(text))
		next := spliceSection(body, section)

		before := len(it.Requirements())
		after := len(backlog.Item{Body: next}.Requirements())
		if before != after {
			return Refusal{fmt.Errorf(
				"refusing to write: the body has %d numbered requirements and this request would leave %d. "+
					"The requirements are the contract; evidence goes below the heading as prose or bullets", before, after)}
		}

		written := []byte(head + next + "\n")
		if err := os.WriteFile(it.Path, written, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", it.Path, err)
		}
		if err := s.wroteBody(it.ID.Bare(), "request-changes", written); err != nil {
			return err
		}
		// The section this just overwrote is gone from the tree, and with it the
		// one thing that says whether five rounds have been closing one defect or
		// five. A failure here costs that record and nothing else, so it warns.
		round, err := s.keepRound(it, text)
		if err != nil {
			fmt.Fprintf(errOut, "warn: round not kept: %v\n", err)
		}
		// After the body write, never before. A row claiming a request whose body
		// write failed is the one wrong direction; a body written whose row
		// failed reports the error and leaves the body correct.
		if err := s.record(ledger.Row{
			ID: it.ID.Bare(), Changes: digest(text), Reqs: findingRequirements(it, text), StillOpen: stillOpen,
			Operator: operator, Date: ledger.Today(),
		}); err != nil {
			return err
		}
		fmt.Printf("changes requested: %s -> %s (%d requirement(s) unchanged)\n",
			it.ID.Bare(), rel(s.cfg.Root, it.Path), after)
		if round != "" {
			fmt.Printf("round kept: %s\n", rel(s.cfg.Root, round))
		}
		reportDemotions(demoted, sunk)
		if len(operator) > 0 {
			fmt.Printf("handed to the operator: %s; drive parks %s %s rather than launching another run\n",
				requirementList(operator), it.ID.Bare(), passNeedsDecision)
			return nil
		}
		fmt.Printf("The next run reads it: drover run %s --dry-run\n", it.ID.Bare())
		return nil
	},
}

// stillOpenAgainstLast checks --still-open against the previous changes row and
// returns the list sorted and deduplicated. A number that row did not send back
// cannot carry a finding over from it, and a first request has nothing to carry.
func (s *session) stillOpenAgainstLast(id string, nums []int) ([]int, error) {
	if len(nums) == 0 {
		return nil, nil
	}
	c := s.ledger.Counts(id)
	if c.ChangesRequested == 0 {
		return nil, Refusal{fmt.Errorf("--still-open names a finding from the previous request for changes, and %s has none: "+
			"drop the flag, every finding in a first request is new", id)}
	}
	out := slices.Clone(nums)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, n := range out {
		if !slices.Contains(c.Reqs, n) {
			return nil, Refusal{fmt.Errorf("--still-open %d: the previous request for changes on %s named %s, so R%d carries no finding over from it; "+
				"pass only numbers from that list, and none for a new defect under a requirement", n, id, intList(c.Reqs), n)}
		}
	}
	return out, nil
}

// operatorAgainstBody checks --operator against the body's list and the
// evidence's findings and returns it sorted and deduplicated. A number the list
// lacks hands the operator nothing to decide, and one no finding names leaves
// the park with no reason to carry.
func operatorAgainstBody(it backlog.Item, text string, nums []int) ([]int, error) {
	if len(nums) == 0 {
		return nil, nil
	}
	out := slices.Clone(nums)
	slices.Sort(out)
	out = slices.Compact(out)
	reqs := requirementsByNum(it)
	named := findingRequirements(it, text)
	for _, n := range out {
		if _, ok := reqs[n]; !ok {
			return nil, Refusal{fmt.Errorf("--operator %d: %s has %d numbered requirement(s), so R%d names none of them; "+
				"pass the number of the requirement the code cannot meet as written", n, it.ID.Bare(), len(reqs), n)}
		}
		if !slices.Contains(named, n) {
			return nil, Refusal{fmt.Errorf("--operator %d: no finding in the evidence names R%d, and the reason it cannot be met goes under that requirement; "+
				"add a heading naming R%d with the reason, then run this again", n, n, n)}
		}
	}
	return out, nil
}

// operatorReasons is the first sentence of the evidence under each requirement
// in nums, as "R3: <sentence>", in nums' order. The evidence is the section of
// the first heading naming R<n>: every line below it until a heading at the
// same depth or shallower, so a subordinate "#### Reason" stays inside the
// finding it explains. With no heading the whole text is the section. The
// reason is the section's first prose paragraph, skipping subordinate headings
// and fenced code, and only a section with no prose falls back to its title.
func operatorReasons(text string, nums []int) []string {
	lines := strings.Split(text, "\n")
	heads := headings(lines)
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		reason := ""
		if len(heads) == 0 {
			if namesRequirement(text, n, false) {
				reason = firstSentence(proseParagraph(lines))
			}
		}
		for i, h := range heads {
			if !namesRequirement(h.text, n, true) {
				continue
			}
			end := len(lines)
			for _, next := range heads[i+1:] {
				if next.depth <= h.depth {
					end = next.line
					break
				}
			}
			reason = firstSentence(proseParagraph(lines[h.line+1 : end]))
			if reason == "" {
				reason = strings.TrimSpace(strings.TrimLeft(h.text, "#"))
			}
			break
		}
		out = append(out, "R"+strconv.Itoa(n)+": "+reason)
	}
	return out
}

// heading is one Markdown heading outside a code fence: its line index, its
// depth in leading #s, and its trimmed text.
type heading struct {
	line, depth int
	text        string
}

// headings is every heading in lines outside a code fence, in order.
func headings(lines []string) []heading {
	var hs []heading
	var f fence
	for i, l := range lines {
		if f.skip(l) {
			continue
		}
		if d := headingDepth(l); d > 0 {
			hs = append(hs, heading{line: i, depth: d, text: strings.TrimSpace(l)})
		}
	}
	return hs
}

// headingDepth is the number of leading #s on an ATX heading line, and zero for
// anything else: the run has to end the line or be followed by a space or tab,
// so "#62 fixed this" stays prose.
func headingDepth(line string) int {
	t := strings.TrimLeft(line, " \t")
	rest := strings.TrimLeft(t, "#")
	d := len(t) - len(rest)
	if d == 0 || rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0
	}
	return d
}

// proseParagraph is the first run of non-blank lines in lines that are neither
// headings nor fenced code, joined by newlines.
func proseParagraph(lines []string) string {
	var out []string
	var f fence
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if f.skip(l) || headingDepth(l) > 0 || t == "" {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, "\n")
}

// namesRequirement reports a finding naming R<n>: its heading when it has one,
// since that is where findingRequirements traces it, and its text otherwise.
func namesRequirement(finding string, n int, headed bool) bool {
	if headed {
		finding, _, _ = strings.Cut(finding, "\n")
	}
	for _, m := range requirementRef.FindAllStringSubmatch(finding, -1) {
		if m[1] == strconv.Itoa(n) {
			return true
		}
	}
	return false
}

// firstSentence is text up to and including its first full stop followed by
// whitespace, on one line, or the whole of it when it has none.
func firstSentence(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if i := strings.Index(text, ". "); i >= 0 {
		return text[:i+1]
	}
	return text
}

// intList renders nums as [3, 5], and an empty list as [] rather than nothing.
func intList(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// requirementRef is a requirement named in evidence, as R6.
var requirementRef = regexp.MustCompile(`\bR(\d+)\b`)

// structuralHeadings are the leading words of a heading that organizes the
// evidence rather than reporting a finding ("Settled, do not reopen"). The list
// is closed on purpose: a heading outside it is read as a finding.
var structuralHeadings = map[string]bool{
	"settled": true, "holds": true, "held": true, "closed": true, "resolved": true,
}

// operatorOnlyFindings returns the [operator] requirements a request's findings
// trace to when every finding traces to one, with the headings it read as
// structure, and no requirements otherwise.
//
// A finding is a heading in the evidence outside a code fence, traced through
// the R<n> it names; a body with no headings is one finding. It fails open:
// refusing a legitimate round sends a real defect to the merge with nobody sent
// back, and letting an operator-only one through costs one round. So any
// BRANCH- token, any session requirement, and any heading that names no
// requirement lets the request through, except a heading whose leading word is
// in structuralHeadings.
func operatorOnlyFindings(it backlog.Item, text string) (ops []backlog.Requirement, structure []string) {
	reqs := requirementsByNum(it)
	findings, headed := findingTexts(text)
	seen := map[int]bool{}
	for _, f := range findings {
		if strings.Contains(f, "BRANCH-") {
			return nil, nil
		}
		named := namedRequirements(f, reqs)
		if len(named) == 0 {
			if headed && structuralHeadings[leadingWord(f)] {
				structure = append(structure, f)
				continue
			}
			return nil, nil
		}
		for _, r := range named {
			if !r.Operator {
				return nil, nil
			}
		}
		for _, r := range named {
			if !seen[r.Num] {
				seen[r.Num] = true
				ops = append(ops, r)
			}
		}
	}
	if len(ops) == 0 {
		return nil, nil
	}
	return ops, structure
}

// findingTexts is each finding in evidence: a heading outside a code fence, or
// the whole text when it has no heading.
func findingTexts(text string) (findings []string, headed bool) {
	var f fence
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); !f.skip(l) && headingDepth(l) > 0 {
			findings = append(findings, t)
		}
	}
	if len(findings) == 0 {
		return []string{text}, false
	}
	return findings, true
}

// requirementsByNum is the body's requirements keyed by their number.
func requirementsByNum(it backlog.Item) map[int]backlog.Requirement {
	reqs := map[int]backlog.Requirement{}
	for _, r := range it.Requirements() {
		reqs[r.Num] = r
	}
	return reqs
}

// namedRequirements is each requirement of the body a finding names as R<n>. A
// number the body lacks is not one: contractContradictions answers for it.
func namedRequirements(finding string, reqs map[int]backlog.Requirement) []backlog.Requirement {
	var named []backlog.Requirement
	for _, m := range requirementRef.FindAllStringSubmatch(finding, -1) {
		n, _ := strconv.Atoi(m[1])
		if r, ok := reqs[n]; ok {
			named = append(named, r)
		}
	}
	return named
}

// findingRequirements is every requirement the evidence's findings name,
// sorted and once each, traced as operatorOnlyFindings traces them. It is what
// the changes row records as reqs, so two rounds naming the same unmet
// requirements in different words read as one finding.
func findingRequirements(it backlog.Item, text string) []int {
	reqs := requirementsByNum(it)
	findings, _ := findingTexts(text)
	seen := map[int]bool{}
	var out []int
	for _, f := range findings {
		for _, r := range namedRequirements(f, reqs) {
			if !seen[r.Num] {
				seen[r.Num] = true
				out = append(out, r.Num)
			}
		}
	}
	sort.Ints(out)
	return out
}

// requirementWord is a requirement named in prose, as "requirement 6".
var requirementWord = regexp.MustCompile(`(?i)\brequirement\s+#?(\d+)\b`)

// removalClaim is evidence saying the contract lost or swapped a requirement:
// "removed R4", "requirement 4 was dropped", "requirements were replaced". It
// wants the verb next to the requirement, so "R4's dead code was removed",
// which is about code, does not match.
var removalClaim = regexp.MustCompile(`(?i)\b(?:removed|dropped|deleted|struck|withdrawn|replaced)\s+(?:the\s+)?(?:requirements?|R\d+)\b` +
	`|\b(?:requirements?(?:\s+#?\d+)?|R\d+)\s+(?:has|have|was|were|is|are)\s+(?:been\s+)?(?:removed|dropped|deleted|struck|withdrawn|replaced)\b`)

// contractContradictions is each way evidence describes a contract the body
// does not hold: a requirement number the list lacks, and a claim that
// requirements were removed or replaced when no amend row follows the item's
// last changes row, which is the only sanctioned way the count moves. A number
// an amend since that row took away is the amended contract being described,
// not contradicted, so it passes. A
// heuristic over prose with fenced blocks left out, because pasted output
// carries tokens that look like R12: it exists to make somebody look before a
// run is told to strip code a standing requirement still demands.
func (s *session) contractContradictions(it backlog.Item, text string) []string {
	have := map[int]bool{}
	reqs := it.Requirements()
	for _, r := range reqs {
		have[r.Num] = true
	}
	prose := unfenced(text)
	amendedFrom := s.amendedSinceRound(it.ID.Bare())
	var why []string
	seen := map[int]bool{}
	for _, re := range []*regexp.Regexp{requirementRef, requirementWord} {
		for _, m := range re.FindAllStringSubmatch(prose, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil || have[n] || seen[n] || n <= amendedFrom {
				continue
			}
			seen[n] = true
			why = append(why, fmt.Sprintf("it names %s, and the body's list has no requirement %d", m[0], n))
		}
	}
	if m := removalClaim.FindString(prose); m != "" && amendedFrom == 0 {
		why = append(why, fmt.Sprintf("it says %q, and the body still holds %d requirement(s) with no drover amend since the last round", m, len(reqs)))
	}
	return why
}

// amendedSinceRound is the largest requirement count an amend row after id's
// last changes row replaced, or after any row when it has none, and zero when
// no amend follows. Each amend row records at least one requirement before it.
func (s *session) amendedSinceRound(id string) int {
	from := 0
	for _, r := range s.ledger.Rows {
		switch {
		case r.ID != id:
		case r.Changes != "":
			from = 0
		case r.Amend != nil:
			from = max(from, r.Amend.From, 1)
		}
	}
	return from
}

// unfenced is text with every fenced block dropped.
func unfenced(text string) string {
	var out []string
	var f fence
	for _, l := range strings.Split(text, "\n") {
		if !f.skip(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// contradictionRefusal refuses evidence that contradicts the contract, or
// under --over-limit warns and lets --yes through. The over-limit caller is the
// operator, who may know the contract is about to change; a reviewer is not,
// and the evidence it wrote is what has to change.
func contradictionRefusal(id string, why []string, overLimit, yes bool) error {
	list := "    " + strings.Join(why, "\n    ")
	if overLimit {
		fmt.Fprintf(errOut, "  ! the evidence for %s may describe a contract the body does not hold:\n%s\n", id, list)
		if yes {
			return nil
		}
		return Refusal{fmt.Errorf(
			"nothing was written: a run told this would act on requirements its body does not carry.\n"+
				"Change the contract first with drover amend %s -f <requirements> --reason \"<why>\", "+
				"or pass --yes once you have looked", id)}
	}
	return Refusal{fmt.Errorf(
		"the evidence for %s describes a contract the body does not hold:\n%s\n\n"+
			"Nothing was written. This is a heuristic over the evidence's prose, so look before changing anything:\n"+
			"correct the evidence to name the requirements the body carries, or, if the contract itself should change,\n"+
			"that is the operator's: drover amend %s -f <requirements> --reason \"<why>\"", id, list, id)}
}

// leadingWord is a heading's first word, lowercased and without the #s or the
// punctuation that follows it.
func leadingWord(heading string) string {
	fields := strings.Fields(strings.TrimLeft(heading, "# "))
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimRight(fields[0], ",.:;!?-"))
}

// operatorRefusal names the requirements, the headings read as structure, and
// what happens to the requirements instead.
func operatorRefusal(id string, ops []backlog.Requirement, structure []string) error {
	lines := make([]string, 0, len(ops))
	for _, r := range ops {
		lines = append(lines, fmt.Sprintf("    R%d: %s", r.Num, r.Text))
	}
	read := ""
	if len(structure) > 0 {
		read = "\n\nRead as structure, not findings:\n    " + strings.Join(structure, "\n    ")
	}
	return fmt.Errorf(
		"every finding here traces to an %s requirement, which no session can meet:\n%s%s\n\n"+
			"Another round would spend itself on work only the operator can do, so the item body is untouched.\n"+
			"Record the look and let the item go to its pull request: it waits there with %s pending,\n"+
			"and the merge holds until the operator runs drover signoff %s <n> \"<what was checked>\".\n\n"+
			"    drover audit %s <reviewed|verified> --code-by agent \"<what you traced>\"\n\n"+
			"A finding the session can fix goes in its own heading, and then request-changes takes it",
		backlog.OperatorTag, strings.Join(lines, "\n"), read, reqNumbers(ops), id, id)
}

// readEvidence reads the evidence from a file, or from in when the path is
// "-". Reading a document rather than taking an argv string is deliberate: the
// evidence carries pasted output and code fences, and a command line invites
// the one-line result the review brief asks reviewers not to write.
func readEvidence(path string, in io.Reader) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path) // #nosec G304 — the operator names the file
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// splitFrontmatter returns the fence (with its trailing newline) and the body
// below it, so a rewrite of the body cannot disturb the metadata above.
func splitFrontmatter(raw string) (head, body string, err error) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", fmt.Errorf("no frontmatter (expected a '---' fence on line 1)")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[:i+1], "\n") + "\n", strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return "", "", fmt.Errorf("frontmatter fence is never closed")
}

// spliceSection replaces everything from the review heading to the next "## "
// with body, or appends the section when the heading is absent.
func spliceSection(text, body string) string {
	heading := prompts.ChangesHeading
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	section := heading + "\n\n" + body
	if start < 0 {
		return strings.TrimRight(text, "\n") + "\n\n" + section
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.Split(section, "\n")...)
	if end < len(lines) {
		out = append(out, "")
		out = append(out, lines[end:]...)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// topLevelNumbered matches the line shape backlog reads as a requirement.
var topLevelNumbered = regexp.MustCompile(`(?m)^(\d+)\.[ \t]+`)

// demoteHeadings pushes every heading in reviewer-supplied evidence below the
// level a body's own sections use, and reports how many it moved.
//
// The same reason demoteNumbering exists, one level up: prose written into a
// section must not create structure the body's own readers act on. A "## " line
// in evidence ends the section as far as spliceSection is concerned, so a
// replacement would land above the reviewer's own heading and everything below
// it would stay, and the next run would read those settled findings as current
// instructions.
//
// Prevention rather than a narrower boundary, because position cannot tell the
// two apart. A project's implement template writes its own "## " sections into a
// body and those have to survive a request-changes, which is what
// TestSpliceSectionReplacesRatherThanAppends pins.
//
// Headings outside a fence all move by the same amount, so the shallowest lands
// at ### and each keeps its depth relative to the rest. operatorReasons reads a
// finding as everything under its heading down to the next one as shallow, and
// a floor that lifted "## R3" and "### Reason" both to ### would make the reason
// a sibling of its finding and drop it. Fenced lines are code, not structure,
// but spliceSection does not know fences, so a "# " or "## " there still sinks
// to ###.
func demoteHeadings(text string) (string, int) {
	lines := strings.Split(text, "\n")
	shallowest := 0
	var f fence
	for _, l := range lines {
		if f.skip(l) {
			continue
		}
		if d := headingDepth(l); d > 0 && (shallowest == 0 || d < shallowest) {
			shallowest = d
		}
	}
	shift := 0
	if shallowest > 0 && shallowest < 3 {
		shift = 3 - shallowest
	}
	n := 0
	f = fence{}
	for i, l := range lines {
		if f.skip(l) {
			trimmed := strings.TrimLeft(l, "#")
			if hashes := len(l) - len(trimmed); hashes > 0 && hashes <= 2 && strings.HasPrefix(trimmed, " ") {
				lines[i] = "###" + trimmed
				n++
			}
			continue
		}
		if shift == 0 || headingDepth(l) == 0 {
			continue
		}
		at := strings.Index(l, "#")
		lines[i] = l[:at] + strings.Repeat("#", shift) + l[at:]
		n++
	}
	if n == 0 {
		return text, 0
	}
	return strings.Join(lines, "\n"), n
}

// demoteEvidence is what every command writing reviewer prose into an item body
// applies first, and reportDemotions says what it moved. Two rules, both there so
// that prose cannot create structure the body's own readers act on.
func demoteEvidence(text string) (out string, numbered, headings int) {
	out, headings = demoteHeadings(text)
	out, numbered = demoteNumbering(out)
	return out, numbered, headings
}

// reportDemotions names what demoteEvidence moved, on stderr, because a writer
// is owed the news that their file reached the body in a different shape.
func reportDemotions(numbered, headings int) {
	if numbered > 0 {
		fmt.Fprintf(errOut, "note: demoted %d numbered line(s) to bullets; a top-level \"N.\" in the body is read as a requirement.\n", numbered)
	}
	if headings > 0 {
		fmt.Fprintf(errOut, "note: demoted %d heading(s) below ###; a \"## \" line is one of the body's own sections.\n", headings)
	}
}

// demoteNumbering turns a top-level numbered line into a bullet and reports how
// many it rewrote. Findings written as "1. 2. 3." are the natural way to list
// them and the one shape the body cannot carry.
func demoteNumbering(text string) (string, int) {
	n := len(topLevelNumbered.FindAllString(text, -1))
	if n == 0 {
		return text, 0
	}
	return topLevelNumbered.ReplaceAllString(text, "- "), n
}

// digest identifies the evidence without copying it. Whitespace is collapsed
// first, so a reflowed paragraph is the same finding and reads as one: the
// point of the field is to tell a review repeating itself from one that found
// something new, and a rewrap is not new.
func digest(text string) string {
	return digestBytes([]byte(strings.Join(strings.Fields(text), " ")))
}

// digestBytes is the 12-hex sum both ledger digests are written in.
func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// bodySum is the body digest: the file's bytes with trailing spaces, tabs,
// carriage returns and newlines at end of file removed, and nothing else.
//
// It skips digest's whitespace collapse. A rewrap is not a new finding, but it
// is an edit, and drive has to see it. What follows the last character is
// nobody's edit: an editor or a snapshot hook that adds or drops the final
// newline would move an exact-bytes digest, and a review that found nothing
// would park as edited over it.
func bodySum(b []byte) string {
	return digestBytes(bytes.TrimRight(b, " \t\r\n"))
}

// bodyMatches reports whether want is one body's digest in either form a
// ledger may carry: sum, its bodySum, or exact, the digest of its exact bytes,
// which is what every row recorded before bodySum holds. Without the second, an
// item mid-review when the binary changed parks as edited on a body nobody
// touched. An empty want matches nothing.
func bodyMatches(want, sum, exact string) bool {
	return want != "" && (want == sum || want == exact)
}

// capRefusal is read out of a log in the morning, so it names the count, the cap,
// the key the number came from, what the reviewer does instead and what the
// operator can do.
//
// Backend-aware, because `drover follow-up` refuses under [register] backend =
// "github" and a refusal that sends a reviewer to a command that will refuse is
// a dead end.
func (s *session) capRefusal(id string, n, limit int) error {
	file := fmt.Sprintf(
		"    drover new <kind> \"<what you found>\" --found-in %s -f <file>", id)
	if s.cfg.RegisterBackend() == config.BackendFile {
		file = fmt.Sprintf("    drover follow-up open \"<what you found>\" --owner %s --sev <C|H|M|L>", id)
	}
	return fmt.Errorf(
		"%s has been sent back %d times and [review] max_review_rounds is %d: this finding does not go\n"+
			"back to the branch. The refusal is recorded; the item body is untouched.\n\n"+
			"The branch is no longer the destination. File it and let the work finish:\n\n"+
			"%s\n\n"+
			"Then record the look, which the cap does not change:\n\n"+
			"    drover audit %s <reviewed|verified> --code-by agent \"<what you traced>\"\n\n"+
			"To send it back anyway, the operator raises [review] max_review_rounds or runs request-changes --over-limit.\n"+
			"Both are the operator's: --over-limit refuses inside a session drover launched, and the\n"+
			"config is not this session's to edit",
		id, n, limit, file, id)
}

func init() {
	requestChangesCmd.Flags().StringVarP(&requestChangesFile, "file", "f", "",
		"the evidence to write, or - for stdin (required)")
	requestChangesCmd.Flags().BoolVar(&requestChangesOverLimit, "over-limit", false,
		"send it back although the item is at [review] max_review_rounds; the operator's call, refused inside a launched session")
	requestChangesCmd.Flags().BoolVar(&requestChangesYes, "yes", false,
		"with --over-limit, write evidence drover warned contradicts the item's requirements")
	requestChangesCmd.Flags().IntSliceVar(&requestChangesStillOpen, "still-open", nil,
		"requirement numbers whose finding from the previous request for changes is still open, as N[,N...]; never a new defect under the same requirement")
	requestChangesCmd.Flags().IntSliceVar(&requestChangesOperator, "operator", nil,
		"requirement numbers whose finding no implementing session can meet as written, as N[,N...]; the reason goes in the evidence under each, and drive parks the item needs_decision")
	root.AddCommand(requestChangesCmd)
}
