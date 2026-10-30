// Package gh reads the issue tracker that holds the follow-up list.
//
// Labels classify and links relate. Membership and severity are labels; the
// item that owns a follow-up is a link: the GitHub sub-issue parent, when that
// parent is a backlog item's issue, and every `Owner: <id>` line in the body.
// One label per item would be a foreign key spelled as a classification, and a
// repository accrues one per item it has ever had.
//
// Every read here degrades rather than fails. A caller that cannot reach GitHub
// must be able to tell "nobody filed one" from "nobody could ask", because those
// rank the same item very differently.
package gh

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Available reports whether the gh CLI is installed and authenticated.
func Available() bool {
	if _, err := exec.LookPath("gh"); err != nil {
		return false
	}
	return exec.Command("gh", "auth", "status").Run() == nil
}

// scope adds the repo selector when one is configured.
func (l Labels) scope(args []string) []string {
	if l.Repo != "" {
		return append(args, "--repo", l.Repo)
	}
	return args
}

// Labels names the vocabulary a project classifies on, and the items an owner
// link can resolve to.
type Labels struct {
	// FollowUp marks membership. An issue carrying it is a follow-up; an issue
	// without it is the tracker's business and not drover's. The query asks for
	// it server-side, so a repository's ordinary backlog is never read at all.
	FollowUp string
	Severity string
	// ItemIssues maps a backlog item's issue number to its bare id. A sub-issue
	// parent names an owner only through it: a parent that is nobody's item
	// issue is an epic or a tracking issue, and reading it as an owner would
	// invent one.
	ItemIssues map[int]string
	Limit      int
	// Dir is the directory gh runs in. It must be the project root: gh infers
	// the repository from the working directory's git remote, so running from
	// the caller's cwd resolves whatever tree they happen to stand in.
	Dir string
	// Repo is "owner/name". Setting it removes the inference entirely, which is
	// the only way a config living outside the work tree can be right.
	Repo string
}

// Issue is one open tracker item.
type Issue struct {
	Number int
	Title  string
	Body   string
	State  string
	// Owners are the ids that own this issue: its sub-issue parent's item, then
	// every `Owner: <id>` line in the body, each once.
	Owners []string
	// Parent is the sub-issue parent's number, zero when it has none.
	Parent int
	// Severity is the word behind the severity prefix, empty when unlabeled.
	Severity string
	Labels   []string
	// BlockedBy is the issues GitHub records this one as blocked by. Only
	// LoadMirror and ViewLinked ask for it; every other read leaves it empty.
	BlockedBy []int
}

// Index is a snapshot of the open register.
//
// OK distinguishes an empty register from an unreachable one. Callers must
// check it before reporting a zero.
//
// Err carries why a read failed. Without it every cause collapses into one
// flag, and the message a caller prints sends someone to re-authenticate when
// the real problem is a repository that does not exist yet — the ordinary state
// of a project on its first day.
type Index struct {
	Issues []Issue
	OK     bool
	Err    error
	// Off records that the project declared no register to read. OK stays true:
	// an empty index is the whole answer here rather than a read that failed, so
	// a caller reporting no follow-ups is not reporting a guess.
	Off bool
	// Unpaged records that the source was read whole rather than a page at a
	// time. A file holding exactly page_limit rows is not a cut-short read, and
	// Truncated cannot tell the two apart from a count alone.
	Unpaged bool
}

// Reason renders why the register could not be read, for a caller reporting it.
func (ix Index) Reason() error {
	if ix.OK {
		return nil
	}
	if ix.Err != nil {
		return ix.Err
	}
	return fmt.Errorf("the follow-up list could not be read")
}

// For returns the issues owned by any of the given ids, deduped so a row owned
// by both an item and one of its dependencies is listed once.
func (ix Index) For(ids ...string) []Issue {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	seen := map[int]bool{}
	var out []Issue
	for _, is := range ix.Issues {
		if seen[is.Number] {
			continue
		}
		for _, owner := range is.Owners {
			if want[owner] {
				out = append(out, is)
				seen[is.Number] = true
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// rawIssue is the shape gh emits.
type rawIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Parent *struct {
		Number int `json:"number"`
	} `json:"parent"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	BlockedBy struct {
		Nodes []struct {
			Number int `json:"number"`
		} `json:"nodes"`
	} `json:"blockedBy"`
}

// issueFields is what every issue read asks for. parent rides on each one,
// because a sub-issue link is half of what says who owns an issue.
const issueFields = "number,title,body,state,labels,parent"

// Load reads the follow-up list in one call.
//
// The follow-up label is asked for server-side rather than filtered out
// afterwards. Membership is the whole difference between the follow-up list and
// a repository's open issues, and filtering locally would spend the page limit
// on issues that were never follow-ups: on a project with 900 open issues and 8
// follow-ups, a 300-issue page reports a cut-short read of a list that is
// complete.
//
// One call feeds every reader on purpose: keying the query on each item would
// turn the unaudited report into a round trip per item.
func Load(labels Labels) Index { return load(labels, true) }

// LoadTracker reads every open issue, membership label or not.
//
// One caller: the pending queue, whose whole question is what the tracker holds
// that drover has not taken in. Everything else uses Load, because reading a
// repository's ordinary backlog as the follow-up list is the defect the
// membership label exists to prevent.
func LoadTracker(labels Labels) Index { return load(labels, false) }

// Owned reads the open issues owned by any of ids that are not follow-ups: a
// `bug` linked to an item and carrying no follow-up label is somebody's report
// about that item, and Load never sees it because membership is asked for
// server-side.
//
// Two reads, one per kind of owner link. An `Owner:` line is found by a body
// search per id; GitHub's search tokenizes, so it can match more than the line,
// and every hit is re-read through issue and kept only when it names the id. A
// sub-issue is found from the parent: each id whose item names an issue lists
// that issue's sub-issues, and each is read whole. Reading the whole open
// tracker and filtering here would spend the page limit on issues nobody owns.
//
// Unlike Load it fails rather than degrades. The caller renders the reason, and
// an empty slice beside a nil error is the one answer that means none owned.
func Owned(labels Labels, ids ...string) ([]Issue, error) {
	if labels.FollowUp == "" || len(ids) == 0 {
		return nil, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh is not on PATH — install it from https://cli.github.com")
	}
	limit := labels.Limit
	if limit <= 0 {
		limit = 300
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	seen := map[int]bool{}
	var out []Issue
	keep := func(is Issue) {
		if seen[is.Number] || !strings.EqualFold(is.State, "OPEN") || slices.Contains(is.Labels, labels.FollowUp) {
			return
		}
		if !slices.ContainsFunc(is.Owners, func(o string) bool { return want[o] }) {
			return
		}
		seen[is.Number] = true
		out = append(out, is)
	}
	for _, id := range ids {
		query := fmt.Sprintf("%q in:body", ownerPrefix+" "+id)
		raw, err := run(labels.Dir, labels.scope([]string{"issue", "list", "--state", "open",
			"--limit", strconv.Itoa(limit), "--search", query, "--json", issueFields}))
		if err != nil {
			return nil, fmt.Errorf("cannot search issues for %s: %w", query, err)
		}
		var page []rawIssue
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("gh returned something that is not an issue list for %s: %w", query, err)
		}
		for _, r := range page {
			keep(labels.issue(r))
		}
	}
	parents := make([]int, 0, len(labels.ItemIssues))
	for n, id := range labels.ItemIssues {
		if want[id] {
			parents = append(parents, n)
		}
	}
	slices.Sort(parents)
	for _, parent := range parents {
		children, err := subIssues(labels, parent)
		if err != nil {
			return nil, err
		}
		for _, n := range children {
			if seen[n] {
				continue
			}
			is, err := View(labels, n)
			if err != nil {
				return nil, err
			}
			keep(is)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// subIssues lists the numbers GitHub records as sub-issues of parent.
func subIssues(labels Labels, parent int) ([]int, error) {
	raw, err := run(labels.Dir, labels.scope([]string{"issue", "view", strconv.Itoa(parent), "--json", "subIssues"}))
	if err != nil {
		return nil, fmt.Errorf("cannot read the sub-issues of #%d: %w", parent, err)
	}
	var v struct {
		SubIssues struct {
			Nodes []struct {
				Number int `json:"number"`
			} `json:"nodes"`
		} `json:"subIssues"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("gh returned something that is not a sub-issue list for #%d: %w", parent, err)
	}
	out := make([]int, 0, len(v.SubIssues.Nodes))
	for _, n := range v.SubIssues.Nodes {
		out = append(out, n.Number)
	}
	return out, nil
}

func load(labels Labels, member bool) Index {
	if _, err := exec.LookPath("gh"); err != nil {
		return Index{Err: fmt.Errorf("gh is not on PATH — install it from https://cli.github.com")}
	}
	if err := exec.Command("gh", "auth", "status").Run(); err != nil {
		return Index{Err: fmt.Errorf("gh is not authenticated — run `gh auth login`")}
	}
	limit := labels.Limit
	if limit <= 0 {
		limit = 300
	}
	// A silent truncation at the page limit would read as "no more issues", so
	// the caller is told when the cap is reached.
	args := []string{"issue", "list", "--state", "open",
		"--limit", strconv.Itoa(limit), "--json", issueFields}
	// Guarded: gh errors on an empty --label, and applyDefaults refills the key,
	// so an empty one here means a caller built Labels by hand and would
	// otherwise read the whole tracker as its follow-up list.
	if member && labels.FollowUp != "" {
		args = append(args, "--label", labels.FollowUp)
	}
	args = labels.scope(args)
	out, err := run(labels.Dir, args)
	if err != nil {
		if labels.Repo != "" {
			return Index{Err: fmt.Errorf("cannot read issues for %s: %w", labels.Repo, err)}
		}
		return Index{Err: fmt.Errorf("cannot read issues (no [github] repo set, so gh inferred one from %s): %w", labels.Dir, err)}
	}
	var raw []rawIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return Index{Err: fmt.Errorf("gh returned something that is not an issue list: %w", err)}
	}

	ix := Index{OK: true}
	for _, r := range raw {
		ix.Issues = append(ix.Issues, labels.issue(r))
	}
	return ix
}

// labelPage is the number of labels asked for at once. `gh label list` defaults
// to 30, and a repository whose follow-up label sorts past that would answer "no such
// label" for one it defines — the wrong answer in the one place this call exists
// to get right.
const labelPage = 1000

// DefinesLabel reports whether the repository defines the named label.
//
// The follow-up read cannot answer this. `gh issue list --label` exits 0 with an
// empty array for a label nobody created, so a typo'd follow_up_label and a
// project with no follow-ups are one output. This is the second fact that separates
// them, and it is asked only when the first came back empty.
func DefinesLabel(l Labels, name string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("no label to look for")
	}
	args := []string{"label", "list", "--limit", strconv.Itoa(labelPage), "--json", "name"}
	out, err := run(l.Dir, l.scope(args))
	if err != nil {
		return false, fmt.Errorf("cannot read the label set: %w", err)
	}
	var raw []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return false, fmt.Errorf("gh returned something that is not a label list: %w", err)
	}
	for _, r := range raw {
		if r.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// issue resolves one raw issue against the project's label vocabulary.
//
// Every read builds its Issue here. A second switch elsewhere would let View
// return an unranked severity for issues the tracker had ranked, and intake
// would scaffold that guess into the field unaudited ranks on.
func (l Labels) issue(r rawIssue) Issue {
	// Labels keeps every raw name whatever else it feeds: intake reads it to
	// pick the id namespace, which no derived field carries.
	is := Issue{Number: r.Number, Title: r.Title, Body: r.Body, State: r.State}
	for _, n := range r.BlockedBy.Nodes {
		is.BlockedBy = append(is.BlockedBy, n.Number)
	}
	for _, name := range r.Labels {
		is.Labels = append(is.Labels, name.Name)
		if l.Severity != "" && strings.HasPrefix(name.Name, l.Severity) {
			is.Severity = strings.TrimPrefix(name.Name, l.Severity)
		}
	}
	if r.Parent != nil {
		is.Parent = r.Parent.Number
		if id, ok := l.ItemIssues[is.Parent]; ok {
			is.Owners = append(is.Owners, id)
		}
	}
	for _, id := range OwnerLines(r.Body) {
		if !slices.Contains(is.Owners, id) {
			is.Owners = append(is.Owners, id)
		}
	}
	return is
}

// ownerPrefix opens the body line naming an owner when no sub-issue link can.
const ownerPrefix = "Owner:"

var ownerLine = regexp.MustCompile(`(?m)^[ \t]*` + ownerPrefix + `[ \t]*([A-Za-z0-9][A-Za-z0-9._-]*)[ \t]*\r?$`)

// OwnerLines are the ids every `Owner: <id>` line in body names, in body order.
// The line must hold the id alone: "Owner: B12, maybe B13" is prose about an
// owner, and guessing which token is the id is how a follow-up lands on the
// wrong item.
func OwnerLines(body string) []string {
	var out []string
	for _, m := range ownerLine.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// OwnerLine is the body line that names id as an owner, for a follow-up filed
// where no sub-issue link can say it.
func OwnerLine(id string) string { return ownerPrefix + " " + id }

// Truncated reports whether the read hit the page limit, meaning there may be
// issues the index does not know about.
func (ix Index) Truncated(limit int) bool {
	if !ix.OK || ix.Unpaged {
		return false
	}
	if limit <= 0 {
		limit = 300
	}
	return len(ix.Issues) >= limit
}

// View fetches one issue, whatever its state.
func View(labels Labels, number int) (Issue, error) {
	return view(labels, number, issueFields)
}

// ViewLinked is View plus the blocked-by links, which cost GitHub a second
// query and so are asked for only by the mirror that writes them.
func ViewLinked(labels Labels, number int) (Issue, error) {
	return view(labels, number, issueFields+",blockedBy")
}

func view(labels Labels, number int, fields string) (Issue, error) {
	out, err := run(labels.Dir, labels.scope([]string{"issue", "view", strconv.Itoa(number),
		"--json", fields}))
	if err != nil {
		return Issue{}, fmt.Errorf("read issue #%d (not found, or no access): %w", number, err)
	}
	var r rawIssue
	if err := json.Unmarshal(out, &r); err != nil {
		return Issue{}, err
	}
	return labels.issue(r), nil
}

// Close closes an issue, leaving comment behind as the reason.
//
// This, CloseNotPlanned and Comment are the calls in this package that do not
// degrade. Every read here reports what it could not ask, because a caller must
// tell an empty register from an unreachable one. A write has no such reading:
// a close that silently did nothing would be reported to the operator as a
// close that happened, and the issue stays open with a record saying otherwise.
func Close(labels Labels, number int, comment string) error {
	return closeAs(labels, number, "", comment)
}

// CloseNotPlanned is Close with GitHub's "not planned" reason, for work retired
// rather than finished. It does not degrade either, for Close's reason.
func CloseNotPlanned(labels Labels, number int, comment string) error {
	return closeAs(labels, number, "not planned", comment)
}

func closeAs(labels Labels, number int, reason, comment string) error {
	args := []string{"issue", "close", strconv.Itoa(number)}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	if _, err := run(labels.Dir, labels.scope(args)); err != nil {
		return fmt.Errorf("close issue #%d: %w", number, err)
	}
	return nil
}

// Comment posts to an issue without changing its state.
func Comment(labels Labels, number int, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("comment on issue #%d: nothing to say", number)
	}
	if _, err := run(labels.Dir, labels.scope([]string{
		"issue", "comment", strconv.Itoa(number), "--body", body,
	})); err != nil {
		return fmt.Errorf("comment on issue #%d: %w", number, err)
	}
	return nil
}

// RemoveLabel drops a label from an issue.
//
// It degrades: a caller reports the failure and carries on, because the body
// it wrote is what drover reads and the tracker write can fail after the file
// has landed. The label is not
// deleted from the repository — other issues carry it.
func RemoveLabel(labels Labels, number int, label string) error {
	if _, err := run(labels.Dir, labels.scope([]string{"issue", "edit", strconv.Itoa(number), "--remove-label", label})); err != nil {
		return fmt.Errorf("remove %s from issue #%d: %w", label, number, err)
	}
	return nil
}

// LoadMirror reads every issue carrying the mirror's membership label, open
// and closed, with the dependency links GitHub records on each.
//
// Closed ones are read on purpose: an issue left closed under an item somebody
// withdrew the completion of is one of the disagreements the read exists to
// find. It degrades the way Load does.
func LoadMirror(labels Labels, member string) Index {
	if _, err := exec.LookPath("gh"); err != nil {
		return Index{Err: fmt.Errorf("gh is not on PATH — install it from https://cli.github.com")}
	}
	limit := labels.Limit
	if limit <= 0 {
		limit = 300
	}
	out, err := run(labels.Dir, labels.scope([]string{"issue", "list", "--state", "all",
		"--limit", strconv.Itoa(limit), "--label", member,
		"--json", issueFields + ",blockedBy"}))
	if err != nil {
		return Index{Err: fmt.Errorf("cannot read issues labeled %s: %w", member, err)}
	}
	var raw []rawIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return Index{Err: fmt.Errorf("gh returned something that is not an issue list: %w", err)}
	}
	ix := Index{OK: true}
	for _, r := range raw {
		ix.Issues = append(ix.Issues, labels.issue(r))
	}
	return ix
}

// Label is a label as a write applies it: created first when the repository
// does not define it, since gh refuses to apply one that does not exist.
type Label struct {
	Name, Color, Description string
}

func (l Labels) ensure(want []Label) {
	for _, lb := range want {
		_, _ = run(l.Dir, l.scope([]string{"label", "create", lb.Name, "--color", lb.Color, "--description", lb.Description}))
	}
}

// CreateIssue opens an issue and returns its number, as a sub-issue of parent
// when parent is non-zero.
//
// It does not degrade, for Close's reason: an issue that silently did not open
// would be recorded nowhere, and the next publish would open it again. A zero
// number beside a nil error says gh printed no URL; the issue exists, and the
// caller reports it as unknown rather than opening a second.
func CreateIssue(labels Labels, title, body string, apply []Label, parent int) (int, error) {
	if strings.TrimSpace(title) == "" {
		return 0, fmt.Errorf("open an issue: no title")
	}
	labels.ensure(apply)
	args := []string{"issue", "create", "--title", title, "--body", body}
	for _, lb := range apply {
		args = append(args, "--label", lb.Name)
	}
	if parent > 0 {
		args = append(args, "--parent", strconv.Itoa(parent))
	}
	out, err := run(labels.Dir, labels.scope(args))
	if err != nil {
		return 0, fmt.Errorf("open an issue for %q: %w", title, err)
	}
	return prNumberIn(string(out)), nil
}

// EditIssue rewrites what drover owns on an issue: labels to add and remove in
// one call, and the body when body is non-nil.
func EditIssue(labels Labels, number int, add []Label, remove []string, body *string) error {
	args := []string{"issue", "edit", strconv.Itoa(number)}
	labels.ensure(add)
	for _, lb := range add {
		args = append(args, "--add-label", lb.Name)
	}
	for _, name := range remove {
		args = append(args, "--remove-label", name)
	}
	if body != nil {
		args = append(args, "--body", *body)
	}
	if len(args) == 3 {
		return nil
	}
	if _, err := run(labels.Dir, labels.scope(args)); err != nil {
		return fmt.Errorf("edit issue #%d: %w", number, err)
	}
	return nil
}

// AddBlockedBy records on the tracker that number cannot proceed until each of
// deps closes. GitHub keeps the graph; drover only adds edges, because an edge
// somebody drew by hand is theirs to remove.
func AddBlockedBy(labels Labels, number int, deps []int) error {
	if len(deps) == 0 {
		return nil
	}
	nums := make([]string, len(deps))
	for i, n := range deps {
		nums[i] = strconv.Itoa(n)
	}
	if _, err := run(labels.Dir, labels.scope([]string{"issue", "edit", strconv.Itoa(number),
		"--add-blocked-by", strings.Join(nums, ",")})); err != nil {
		return fmt.Errorf("mark issue #%d blocked by %s: %w", number, strings.Join(nums, ", "), err)
	}
	return nil
}

// Reopen reopens an issue, leaving comment behind as the reason. It does not
// degrade, for Close's reason.
func Reopen(labels Labels, number int, comment string) error {
	args := []string{"issue", "reopen", strconv.Itoa(number)}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	if _, err := run(labels.Dir, labels.scope(args)); err != nil {
		return fmt.Errorf("reopen issue #%d: %w", number, err)
	}
	return nil
}

func run(dir string, args []string) ([]byte, error) {
	return runWithin(dir, args, 30*time.Second)
}

func runWithin(dir string, args []string, limit time.Duration) ([]byte, error) {
	cmd := exec.Command("gh", args...) // #nosec G204 -- args are built here, never from user text
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// A hung network call must not hang the status table.
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("gh timed out after %s", limit)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// MergedPR reports whether a merged pull request exists for a head branch.
//
// This is the authoritative answer and git ancestry is not. A squash merge
// replaces the branch's commits with one new commit, so `git branch --merged`
// reports an unmerged branch forever; a rebase merge does the same. Local
// ancestry also depends on having fetched, which makes it wrong twice over on a
// tree nobody has pulled.
func MergedPR(labels Labels, branch string) (int, bool, error) {
	n, _, merged, err := MergedPRHead(labels, branch)
	return n, merged, err
}

// MergedPRHead is MergedPR with the commit the pull request's head pointed at
// when it merged, which is what a review is compared against. Empty when the
// forge did not say.
func MergedPRHead(labels Labels, branch string) (number int, head string, merged bool, err error) {
	if !Available() {
		return 0, "", false, fmt.Errorf("gh unavailable")
	}
	out, err := run(labels.Dir, labels.scope([]string{
		"pr", "list", "--head", branch, "--state", "merged",
		"--limit", "1", "--json", "number,mergedAt,headRefOid",
	}))
	if err != nil {
		return 0, "", false, err
	}
	var prs []struct {
		Number     int    `json:"number"`
		MergedAt   string `json:"mergedAt"`
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, "", false, err
	}
	if len(prs) == 0 || prs[0].MergedAt == "" {
		return 0, "", false, nil
	}
	return prs[0].Number, prs[0].HeadRefOid, true, nil
}

// RequiredChecks reports whether the trunk has required status checks: whether
// the forge has anything to wait on before an auto-merge, or merges it on the
// spot.
//
// GitHub answers 404 for a branch with no protection or no required checks, and
// 403 where the plan cannot protect the branch at all (a private repository on
// the free plan). Both mean nothing waits, so both are false rather than errors.
func RequiredChecks(labels Labels, trunk string) (bool, error) {
	if !Available() {
		return false, fmt.Errorf("gh unavailable")
	}
	repo := labels.Repo
	if repo == "" {
		repo = "{owner}/{repo}"
	}
	out, err := run(labels.Dir, []string{"api", "repos/" + repo + "/branches/" + trunk + "/protection/required_status_checks"})
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "(HTTP 404)") || strings.Contains(msg, "(HTTP 403)") {
			return false, nil
		}
		return false, err
	}
	var rc struct {
		Contexts []string          `json:"contexts"`
		Checks   []json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal(out, &rc); err != nil {
		return false, fmt.Errorf("required_status_checks answered %q: %w", strings.TrimSpace(string(out)), err)
	}
	return len(rc.Contexts) > 0 || len(rc.Checks) > 0, nil
}

// OpenPR reports the open pull request for a head branch, if there is one.
//
// Read before every create. `gh pr create` fails on a branch that already has
// one, and a caller treating that failure as a forge problem would park an item
// over a pull request that exists and is fine. A correction pass pushes to the
// same one on purpose: two pull requests split the review history of one
// contract across two places.
func OpenPR(labels Labels, branch string) (int, bool, error) {
	n, _, ok, err := OpenPRHead(labels, branch)
	return n, ok, err
}

// OpenPRHead is OpenPR with the commit the pull request's head points at on the
// forge, which is what a merge of it carries whatever this clone holds. Empty
// when the forge did not say.
func OpenPRHead(labels Labels, branch string) (number int, head string, ok bool, err error) {
	if !Available() {
		return 0, "", false, fmt.Errorf("gh unavailable")
	}
	out, err := run(labels.Dir, labels.scope([]string{
		"pr", "list", "--head", branch, "--state", "open",
		"--limit", "1", "--json", "number,headRefOid",
	}))
	if err != nil {
		return 0, "", false, err
	}
	var prs []struct {
		Number     int    `json:"number"`
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, "", false, err
	}
	if len(prs) == 0 || prs[0].Number == 0 {
		return 0, "", false, nil
	}
	return prs[0].Number, prs[0].HeadRefOid, true, nil
}

// CreatePR opens a pull request from head into base, or reports the one already
// open for that head.
//
// The reuse is here rather than only in the caller. `gh pr create` exits non-zero
// on a branch that already has one, and every caller that failed to check first
// would read that as a forge problem and report a pull request as unopened while
// one sits open; a correction pass returns to the same branch, so finding one is
// the ordinary case.
//
// It does not degrade, for Close's reason: a pull request that silently did not
// open would be reported to the operator as one that did, and the branch sits
// with nobody expecting it to. The number comes back out of the URL gh prints,
// which is the only thing it gives a caller without a second round trip.
func CreatePR(labels Labels, head, base, title, body string) (int, error) {
	if !Available() {
		return 0, fmt.Errorf("open a pull request for %s: gh unavailable", head)
	}
	if strings.TrimSpace(title) == "" {
		return 0, fmt.Errorf("open a pull request for %s: no title", head)
	}
	if n, ok, err := OpenPR(labels, head); err != nil {
		return 0, fmt.Errorf("open a pull request for %s: %w", head, err)
	} else if ok {
		return n, nil
	}
	out, err := run(labels.Dir, labels.scope([]string{
		"pr", "create", "--head", head, "--base", base,
		"--title", title, "--body", body,
	}))
	if err != nil {
		return 0, fmt.Errorf("open a pull request for %s: %w", head, err)
	}
	return prNumberIn(string(out)), nil
}

// prNumberIn reads the number off the tail of a pull request URL. A zero says
// gh printed something else, which is worth reporting as an unknown number
// rather than as a failure: the pull request is open either way, and a caller
// that treated the parse as the write would open a second one.
func prNumberIn(out string) int {
	for _, f := range strings.Fields(out) {
		i := strings.LastIndex(f, "/")
		if i < 0 {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(f[i+1:])); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// PR is one pull request as status and show report it.
type PR struct {
	Number int
	// State is open, draft, merged or closed: gh's state with a draft split out
	// of open, because a draft is a pull request nobody has asked to merge yet.
	State string
	URL   string
	// Head is the branch it was opened from, and Fork says that branch lives in
	// another repository. A fork's branch can share a name with an item's branch
	// and is never the item's work.
	Head string
	Fork bool
	// Checks is the head's check conclusion: success, failure, pending or none.
	// Only PullRequestsFor asks for it.
	Checks string
}

// Why a forge read did not complete, in a word a table cell can carry.
const (
	UnreadMissing     = "gh missing"
	UnreadAuth        = "not authenticated"
	UnreadUnreachable = "forge unreachable"
	UnreadFailed      = "gh failed"
	// UnreadNoGitHub is a checkout whose remotes name no GitHub host, or no
	// remote at all. It is not a failure to report on every row: there are no
	// pull requests here to read.
	UnreadNoGitHub = "no GitHub remote"
)

// Unread is a forge read that did not complete. Reason is one of the Unread
// constants; Err is what gh said, which is what the operator acts on.
type Unread struct {
	Reason string
	Err    error
}

func (u *Unread) Error() string { return u.Reason + ": " + u.Err.Error() }
func (u *Unread) Unwrap() error { return u.Err }

// unread classifies a failed gh call. gh exits 4 when it needs a login, and an
// expired token comes back from the API as a 401. "gh auth login" in the text
// decides nothing: gh also suggests it, exiting 1, to a checkout whose remotes
// point at no GitHub host, where the user may be logged in. A timeout or a
// resolver failure is the forge rather than the request.
func unread(err error) *Unread {
	msg := err.Error()
	var exit *exec.ExitError
	switch {
	case strings.Contains(msg, "point to a known GitHub host"), strings.Contains(msg, "no git remotes found"):
		return &Unread{Reason: UnreadNoGitHub, Err: err}
	case errors.As(err, &exit) && exit.ExitCode() == 4, strings.Contains(msg, "HTTP 401"):
		return &Unread{Reason: UnreadAuth, Err: err}
	case strings.Contains(msg, "timed out"), strings.Contains(msg, "could not resolve host"),
		strings.Contains(msg, "error connecting"), strings.Contains(msg, "dial tcp"):
		return &Unread{Reason: UnreadUnreachable, Err: err}
	}
	return &Unread{Reason: UnreadFailed, Err: err}
}

type rawPR struct {
	Number            int    `json:"number"`
	State             string `json:"state"`
	IsDraft           bool   `json:"isDraft"`
	URL               string `json:"url"`
	HeadRefName       string `json:"headRefName"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Context    string `json:"context"`
		Workflow   string `json:"workflowName"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
		StartedAt  string `json:"startedAt"`
	} `json:"statusCheckRollup"`
}

const prFields = "number,state,isDraft,url,headRefName,isCrossRepository"

// PullRequests reads the repository's pull requests in every state, newest
// first, in one call. status asks once per invocation and matches each item's
// branch against the page, because a query per item is a round trip per row.
func PullRequests(labels Labels, limit int) ([]PR, error) {
	if limit <= 0 {
		limit = 300
	}
	return listPRs(labels, []string{"pr", "list", "--state", "all",
		"--limit", strconv.Itoa(limit), "--json", prFields}, false)
}

// PullRequestsFor reads the pull requests opened from one branch, with each
// head's check conclusion, which costs GitHub enough that only the one-item
// read asks for it.
func PullRequestsFor(labels Labels, branch string) ([]PR, error) {
	return listPRs(labels, []string{"pr", "list", "--state", "all", "--head", branch,
		"--limit", "10", "--json", prFields + ",statusCheckRollup"}, true)
}

func listPRs(labels Labels, args []string, checks bool) ([]PR, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, &Unread{Reason: UnreadMissing, Err: fmt.Errorf("gh is not on PATH — install it from https://cli.github.com")}
	}
	out, err := run(labels.Dir, labels.scope(args))
	if err != nil {
		return nil, unread(err)
	}
	var raw []rawPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, &Unread{Reason: UnreadFailed, Err: fmt.Errorf("gh returned something that is not a pull request list: %w", err)}
	}
	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		p := PR{Number: r.Number, State: strings.ToLower(r.State), URL: r.URL,
			Head: r.HeadRefName, Fork: r.IsCrossRepository}
		if p.State == "open" && r.IsDraft {
			p.State = "draft"
		}
		if checks {
			p.Checks = rollup(r)
		}
		prs = append(prs, p)
	}
	return prs, nil
}

// rollup reduces a head's checks to one word. A check re-run on the same head
// appears once per run, so only the latest run of each name counts: an earlier
// failure the re-run cleared is not the head's state.
func rollup(r rawPR) string {
	type attempt struct{ started, result string }
	latest := map[string]attempt{}
	for _, c := range r.StatusCheckRollup {
		name := c.Workflow + "/" + c.Name + c.Context
		state := "success"
		switch {
		case c.State == "PENDING" || c.State == "EXPECTED",
			c.Status != "" && c.Status != "COMPLETED":
			state = "pending"
		case c.State == "FAILURE" || c.State == "ERROR",
			slices.Contains([]string{"FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE"}, c.Conclusion):
			state = "failure"
		}
		if prev, ok := latest[name]; !ok || c.StartedAt >= prev.started {
			latest[name] = attempt{c.StartedAt, state}
		}
	}
	if len(latest) == 0 {
		return "none"
	}
	out := "success"
	for _, a := range latest {
		switch a.result {
		case "failure":
			return "failure"
		case "pending":
			out = "pending"
		}
	}
	return out
}

// RepoURL is the web address of the repository gh resolves for this project,
// which is what a link to one of its issues or pull requests is built on. It
// asks gh rather than assembling github.com/<repo>, so a fork or a GitHub
// Enterprise host gets its own address.
//
// It is bounded well under run's 30s: the address only decorates a number
// already printed, and the commands that ask include ones that otherwise reach
// no forge at all.
func RepoURL(labels Labels) (string, error) {
	args := []string{"repo", "view"}
	if labels.Repo != "" {
		args = append(args, labels.Repo)
	}
	out, err := runWithin(labels.Dir, append(args, "--json", "url"), 5*time.Second)
	if err != nil {
		return "", err
	}
	var r struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", err
	}
	return strings.TrimSuffix(r.URL, "/"), nil
}
