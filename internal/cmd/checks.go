package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	goversion "go/version"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/ledger"
)

// forgeCheck is one check run the forge holds for a commit, as GitHub's
// check-runs endpoint returns it. For an Actions job the id is the job's id,
// and details_url carries the workflow run it belongs to.
type forgeCheck struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	HTMLURL     string `json:"html_url"`
	DetailsURL  string `json:"details_url"`
	CompletedAt string `json:"completed_at"`
	HeadSHA     string `json:"head_sha"`
}

// red reports a completed check whose conclusion is a failure of some kind. A
// canceled run is red here though merge-gate.sh waits on it: it is the run
// most often canceled by the forge rather than the code, and rerunning it is
// the answer either way.
func (r forgeCheck) red() bool {
	return r.Status == "completed" &&
		slices.Contains([]string{"failure", "timed_out", "startup_failure", "cancelled"}, r.Conclusion)
}

// green reports a completed check whose conclusion lets a head merge.
func (r forgeCheck) green() bool {
	return r.Status == "completed" && slices.Contains([]string{"success", "neutral", "skipped"}, r.Conclusion)
}

// parkInfraFailure is a head whose checks failed on the forge's infrastructure
// while rerun_infra_failure is below auto, so a person or a supervisor reruns
// them rather than drive.
const parkInfraFailure = "infra_failure"

// infraPatterns are the log lines that mark a failed job as the forge's
// infrastructure failing, matched case-insensitively. Each is a failure a
// supervisor reran by hand and that passed unchanged. [forge] infra_patterns
// adds to them.
var infraPatterns = []string{
	"toomanyrequests",     // Docker Hub's pull rate limit
	"rate limit exceeded", // GitHub's API and most registries
	"429 Too Many Requests",
	"No space left on device",
	"The runner has received a shutdown signal",
	"lost communication with the server",
	"Could not resolve host",
	"Temporary failure in name resolution",
	"connection reset by peer",
	"TLS handshake timeout",
}

// failurePatterns pick out the lines of a code failure's log that say what
// failed, matched against each line with its timestamp stripped. [forge]
// failure_patterns adds to them.
var failurePatterns = []string{
	`^--- FAIL`,                     // go test, one test
	`^FAIL\b`,                       // go test, one package
	`^\S+:\d+:\d+: .*\(\w[\w-]*\)$`, // a golangci-lint finding, linter named last
	`^\S+\.\w+:\d+:\d+: `,           // a compiler or vet error
	`^Vulnerability #\d+`,           // govulncheck
	`^\s*Found in: `,
	`^\s*Fixed in: `,
	`^panic: `,
}

// stdlibFixed is govulncheck's Fixed in line for a standard-library package,
// whose version is a Go release where a module's is a semver tag.
var stdlibFixed = regexp.MustCompile(`^\s*Fixed in: \S+@(go\d\S*)$`)

// toolchainFix is the line telling a session how to fix a standard-library
// vulnerability its failure lines name, and empty when they name none. A rerun
// does not fix one: setup-go takes whatever patch release the runner image has
// cached, and only go.mod picks a newer one.
func toolchainFix(failure []string) string {
	var newest string
	for _, l := range failure {
		if m := stdlibFixed.FindStringSubmatch(l); m != nil && goversion.Compare(m[1], newest) > 0 {
			newest = m[1]
		}
	}
	if newest == "" {
		return ""
	}
	return "The fix is a `toolchain " + newest + "` line in go.mod, or one naming a newer release.\n"
}

// failureMax is how many lines a failure carries.
const failureMax = 5

// judged is one red check and what drive took it for.
type judged struct {
	run      forgeCheck
	class    string
	evidence string
	// failure is what the job log says failed, read whenever the log was, so a
	// check read as code by any path carries it.
	failure []string
	// workflow is the Actions run to rerun, zero for a check no workflow run
	// produced.
	workflow int64
}

// settled is what settleChecks concluded about a head before the gate reads
// it. A park stops the item; none reports a head no workflow ran on, for a
// verify refusal that follows to say so; code is the red checks left for the
// gate, for a refusal that follows to say what failed. detail is what a park's
// ledger row carries, the checks that caused it.
type settled struct {
	park, reason string
	detail       parkDetail
	stop         error
	none         string
	code         []judged
}

// settleChecks waits for the checks on sha and classifies every red one before
// [drive] merge_gate reads it, because the gate cannot tell whose failure a red
// check is and sends every one of them back to the item.
//
// infra is rerun and waited on again, up to [forge] reruns per head; trunk
// parks trunk_red, since no branch fixes a check the trunk fails too; code is
// left for the gate, as it always was. Anything it cannot read leaves the gate
// to answer, which is what drive did before it classified anything.
func (s *session) settleChecks(ctx context.Context, d *DriveItem, sha string, deadline time.Time) settled {
	d.head = sha
	superseded := map[int64]bool{}
	for {
		runs, err := s.waitChecks(ctx, d.ID, sha, deadline, superseded)
		if err != nil || ctx.Err() != nil {
			return settled{}
		}
		if len(runs) == 0 {
			why := fmt.Sprintf("no workflow ran on this head (%s) in %s", shortSHA(sha), s.cfg.ForgeChecksWait())
			s.recordCheck(d.ID, sha, judged{class: ledger.ClassNone, evidence: why}, false, "")
			fmt.Fprintf(errOut, "  · %s: %s; [drive] merge_gate decides\n", d.ID, why)
			return settled{none: why}
		}
		// A rerun attempt the forge has not replaced yet is still the old red
		// one, which waitChecks returns only at its limit; classifying it again
		// would rerun it again.
		var reds []judged
		for _, r := range runs {
			if r.red() && !superseded[r.ID] {
				reds = append(reds, s.classify(ctx, r))
			}
		}
		if len(reds) == 0 {
			return settled{}
		}
		out, rerun := s.actOnReds(ctx, d, sha, reds)
		if len(rerun) == 0 {
			// Set here rather than by each caller, so no path that parks on
			// this result records the last session's log in place of the
			// check that caused the park.
			if out.park != "" {
				d.setDetail(out.detail)
			}
			return out
		}
		for _, id := range rerun {
			superseded[id] = true
		}
	}
}

// actOnReds records each red check and decides what follows: a park, the
// check ids drive reran and now waits on, or neither, which leaves the gate.
func (s *session) actOnReds(ctx context.Context, d *DriveItem, sha string, reds []judged) (settled, []int64) {
	trunkPol, _ := s.cfg.Policy("report_trunk_red")
	infraPol, _ := s.cfg.Policy("rerun_infra_failure")
	for i := range reds {
		switch {
		case reds[i].class == ledger.ClassTrunk && trunkPol == polOff:
			reds[i].class, reds[i].evidence = ledger.ClassCode, reds[i].evidence+"; report_trunk_red is off"
		case reds[i].class == ledger.ClassInfra && infraPol == polOff:
			reds[i].class, reds[i].evidence = ledger.ClassCode, reds[i].evidence+"; rerun_infra_failure is off"
		case reds[i].class == ledger.ClassInfra && reds[i].workflow == 0:
			reds[i].class, reds[i].evidence = ledger.ClassCode, reds[i].evidence+"; no workflow run to rerun"
		}
	}
	has := func(class string) bool {
		return slices.ContainsFunc(reds, func(j judged) bool { return j.class == class })
	}

	if has(ledger.ClassTrunk) {
		var named []string
		for _, j := range reds {
			act := ""
			if j.class == ledger.ClassTrunk {
				act = "report_trunk_red"
				named = append(named, j.evidence)
			}
			s.recordCheck(d.ID, sha, j, false, act)
		}
		return settled{park: parkTrunkRed, stop: fmt.Errorf("%w: %s", errTrunkRed, strings.Join(named, "; ")), reason: strings.Join(named, "\n") +
			"\nno branch can fix a check the trunk fails too, so nothing was sent back",
			detail: parkDetail{Detail: ledger.ClassTrunk, Evidence: strings.Join(named, "\n")}}, nil
	}
	// A code failure goes to the gate, which sends it back, so rerunning the
	// infra failures beside it would spend reruns on a head that is going back.
	if has(ledger.ClassCode) {
		var code []judged
		for _, j := range reds {
			s.recordCheck(d.ID, sha, j, false, "")
			if j.class == ledger.ClassCode {
				code = append(code, j)
			}
		}
		return settled{code: code}, nil
	}

	var workflows []int64
	for _, j := range reds {
		if !slices.Contains(workflows, j.workflow) {
			workflows = append(workflows, j.workflow)
		}
	}
	if spent, limit := s.ledger.Reruns(d.ID, sha), s.cfg.ForgeReruns(); spent+len(workflows) > limit {
		for i := range reds {
			reds[i].class = ledger.ClassCode
			reds[i].evidence += fmt.Sprintf("; %d of %d reruns of this head spent, so it is the code's", spent, limit)
			s.recordCheck(d.ID, sha, reds[i], false, "")
		}
		return settled{code: reds}, nil
	}
	if infraPol != polAuto {
		var lines []string
		for _, j := range reds {
			s.recordCheck(d.ID, sha, j, false, "rerun_infra_failure")
			lines = append(lines, j.run.Name+": "+j.evidence)
		}
		return settled{park: parkInfraFailure, reason: "a check failed on the forge's infrastructure, and rerun_infra_failure is " +
			string(infraPol) + ", so drive did not rerun it:\n" + strings.Join(lines, "\n"),
			detail: parkDetail{Detail: ledger.ClassInfra, Evidence: strings.Join(lines, "\n")}}, nil
	}

	var ids []int64
	rerun := map[int64]bool{}
	for _, j := range reds {
		first := !rerun[j.workflow]
		if first {
			rerun[j.workflow] = true
			fmt.Fprintf(errOut, "  · %s: %s failed on the forge (%s); rerunning its failed jobs\n", d.ID, j.run.Name, j.evidence)
			if err := s.rerunFailed(ctx, j.workflow); err != nil {
				fmt.Fprintf(errOut, "  · %s: the rerun of %s would not start, so [drive] merge_gate decides: %v\n", d.ID, j.run.Name, err)
				s.recordCheck(d.ID, sha, j, true, "rerun_infra_failure")
				return settled{}, nil
			}
		}
		s.recordCheck(d.ID, sha, j, first, "rerun_infra_failure")
		ids = append(ids, j.run.ID)
	}
	return settled{}, ids
}

// classify reads one red check: infra when its job ran no step, its run was
// canceled or its log carries an infrastructure line; trunk when the same check
// concluded failure on the trunk's latest completed run; code otherwise.
func (s *session) classify(ctx context.Context, r forgeCheck) judged {
	j := judged{run: r, workflow: workflowRun(r.DetailsURL)}
	if r.Conclusion == "cancelled" {
		j.class, j.evidence = ledger.ClassInfra, "the run was canceled"
		return j
	}
	if j.workflow > 0 {
		if ran, err := s.stepsRun(ctx, r.ID); err == nil && ran == 0 {
			j.class, j.evidence = ledger.ClassInfra, "the job failed with no step run"
			return j
		}
		if log, err := s.jobLog(ctx, r.ID); err == nil {
			j.failure = failureLines(log, s.failurePatterns())
			if line := infraLine(log, append(slices.Clone(infraPatterns), s.cfg.Forge.InfraPatterns...)); line != "" {
				j.class, j.evidence = ledger.ClassInfra, line
				return j
			}
		}
	}
	trunk := s.trunk()
	if f := s.trunkRed(ctx, trunk, r.Name); f != nil {
		j.class, j.evidence = ledger.ClassTrunk, f.evidence(trunk)
		return j
	}
	j.class, j.evidence = ledger.ClassCode, "no infrastructure line in its log, and "+trunk+" does not fail it"
	return j
}

var workflowRunPath = regexp.MustCompile(`/actions/runs/(\d+)`)

// workflowRun is the Actions run a check's details URL names, zero for a check
// some other app reported.
func workflowRun(details string) int64 {
	m := workflowRunPath.FindStringSubmatch(details)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}

// logStamp is the timestamp Actions prefixes to every log line.
var logStamp = regexp.MustCompile(`^\S+Z `)

// evidenceMax bounds the log line a row carries.
const evidenceMax = 200

// infraLine is the first log line carrying one of patterns, without its
// timestamp, or "" when none does.
func infraLine(log string, patterns []string) string {
	for _, line := range strings.Split(log, "\n") {
		low := strings.ToLower(line)
		for _, p := range patterns {
			if strings.Contains(low, strings.ToLower(p)) {
				return logLine(line)
			}
		}
	}
	return ""
}

// logEscape is a terminal escape sequence, which Actions keeps in a job log
// for the colors a step printed.
var logEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// logAnnotation is the workflow command Actions prefixes to a line a step
// reported as an error or a warning.
var logAnnotation = regexp.MustCompile(`^##\[(error|warning|notice)\]`)

// logLine is one log line as a person reads it: no timestamp, no annotation,
// no escapes, and bounded at evidenceMax.
func logLine(line string) string {
	line = logStamp.ReplaceAllString(strings.TrimSpace(line), "")
	line = logAnnotation.ReplaceAllString(logEscape.ReplaceAllString(line, ""), "")
	line = strings.TrimSpace(line)
	if len(line) > evidenceMax {
		line = line[:evidenceMax]
	}
	return line
}

// failureEnd marks where a make target or an Actions step gave up, the line a
// log with no failure pattern in it is read back from.
var failureEnd = regexp.MustCompile(`^(make: \*\*\*|Error:)`)

// failureLines are up to failureMax lines of log matching one of patterns,
// read as logLine reads them. A log matching none falls back to the last
// failureMax non-blank lines before the first make or Actions error, and nil
// when it has neither.
func failureLines(log string, patterns []*regexp.Regexp) []string {
	var lines []string
	for _, raw := range strings.Split(log, "\n") {
		if l := logLine(raw); l != "" && !strings.HasPrefix(l, "##[") {
			lines = append(lines, l)
		}
	}
	var hit []string
	for _, l := range lines {
		if slices.ContainsFunc(patterns, func(p *regexp.Regexp) bool { return p.MatchString(l) }) {
			if hit = append(hit, l); len(hit) == failureMax {
				return hit
			}
		}
	}
	if len(hit) > 0 {
		return hit
	}
	end := slices.IndexFunc(lines, failureEnd.MatchString)
	if end < 0 {
		return nil
	}
	return lines[max(0, end-failureMax):end]
}

// failurePatterns are the built-in failure patterns and [forge]
// failure_patterns, compiled. Load refuses a pattern that does not compile, so
// one that fails here is skipped rather than fatal.
func (s *session) failurePatterns() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range append(slices.Clone(failurePatterns), s.cfg.Forge.FailurePatterns...) {
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// jobLog is one Actions job's log. gh 2.103 refuses to print a response
// holding terminal escapes unless told to, and a job log holds the colors its
// steps printed; an older gh has no such flag, so it is asked plainly when the
// flag is refused.
func (s *session) jobLog(ctx context.Context, job int64) (string, error) {
	path := "repos/" + s.repo() + "/actions/jobs/" + strconv.FormatInt(job, 10) + "/logs"
	out, err := s.ghAPI(ctx, "--allow-escape-sequences", path)
	if err != nil {
		out, err = s.ghAPI(ctx, path)
	}
	return string(out), err
}

// stepsRun counts the steps of an Actions job that ran to a conclusion. A job
// the forge failed before its runner started has none.
func (s *session) stepsRun(ctx context.Context, job int64) (int, error) {
	out, err := s.ghAPI(ctx, "repos/"+s.repo()+"/actions/jobs/"+strconv.FormatInt(job, 10))
	if err != nil {
		return 0, err
	}
	var doc struct {
		Steps []struct {
			Conclusion string `json:"conclusion"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return 0, fmt.Errorf("gh api answered %q: %w", lastLines(string(out), 1), err)
	}
	n := 0
	for _, st := range doc.Steps {
		if st.Conclusion != "" && st.Conclusion != "skipped" {
			n++
		}
	}
	return n, nil
}

// trunkWindow is how many first-parent trunk commits trunkRed reads back from
// the tip before calling a check not red.
const trunkWindow = 10

// trunkFailure is the trunk's run of a check that failed, and where it sits.
type trunkFailure struct {
	run    forgeCheck
	sha    string
	behind int
}

// evidence names the run and the commit it failed on, so a stale read shows in
// the park and the stop message rather than in a gh run list the next morning.
func (f trunkFailure) evidence(trunk string) string {
	at := trunk + "'s tip"
	if f.behind > 0 {
		at = fmt.Sprintf("%d first-parent %s behind %s's tip", f.behind, plural(f.behind, "commit", "commits"), trunk)
	}
	sha := f.sha
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if sha == "" {
		sha = trunk
	}
	return fmt.Sprintf("%s concluded failure on %s's latest completed run too, at %s (%s), completed %s: %s",
		f.run.Name, trunk, sha, at, f.run.CompletedAt, checkURL(f.run))
}

// trunkRed is the trunk's latest completed run of the check named name when
// that run failed, or nil when it passed, the trunk no longer runs it, or it
// cannot be read.
//
// The tip alone is not enough under drive: every merge moves it, so the next
// item's checks finish while the tip's own run of the same check is still in
// progress. It walks back along first parents only past a commit whose check
// runs include one still in progress. A commit whose runs have all completed
// with none of that name ends the walk not red: a path-filtered workflow, a
// renamed matrix shard or a dropped job leaves such commits, and the older
// failure behind them says nothing about the trunk now. The first commit it
// cannot read ends the walk too, so an older failure never outvotes a newer
// commit nobody could see.
//
// The walk starts from the commit the forge answered for the trunk's name,
// never from a local ref: a fetch older than the forge's tip would otherwise
// count its parents from the wrong commit and call one behind it the tip.
func (s *session) trunkRed(ctx context.Context, trunk, name string) *trunkFailure {
	rev := trunk
	for behind := 0; behind <= trunkWindow; behind++ {
		runs, err := s.commitRuns(ctx, rev)
		if err != nil {
			return nil
		}
		latest := latestCompleted(runs, name)
		if latest == nil {
			if !slices.ContainsFunc(runs, func(r forgeCheck) bool { return r.Status != "completed" }) {
				return nil
			}
			at := rev
			if behind == 0 {
				at = runsHead(runs)
			}
			if rev = s.firstParent(ctx, at); rev == "" {
				return nil
			}
			continue
		}
		if !slices.Contains([]string{"failure", "timed_out", "startup_failure"}, latest.Conclusion) {
			return nil
		}
		f := &trunkFailure{run: *latest, sha: latest.HeadSHA, behind: behind}
		if behind > 0 && f.sha == "" {
			f.sha = rev
		}
		return f
	}
	return nil
}

// runsHead is the commit runs were read from, "" when none of them says.
func runsHead(runs []forgeCheck) string {
	for _, r := range runs {
		if r.HeadSHA != "" {
			return r.HeadSHA
		}
	}
	return ""
}

// firstParent is sha's first parent, read from git where the commit is local
// and from the forge where the last fetch predates it. It is "" for a root
// commit, an empty sha, or a parent neither can name.
func (s *session) firstParent(ctx context.Context, sha string) string {
	if sha == "" || strings.HasPrefix(sha, "-") {
		return ""
	}
	if out, err := gitcmd.Command(s.cfg.Root, "rev-parse", "--verify", "--quiet", sha+"^1^{commit}").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	out, err := s.ghAPI(ctx, "repos/"+s.repo()+"/commits/"+url.PathEscape(sha))
	if err != nil {
		return ""
	}
	var c struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if json.Unmarshal(out, &c) != nil || len(c.Parents) == 0 {
		return ""
	}
	return c.Parents[0].SHA
}

// commitRuns are every check run on rev, of every name, across every page:
// gh --paginate prints one JSON object per page, one after another.
func (s *session) commitRuns(ctx context.Context, rev string) ([]forgeCheck, error) {
	out, err := s.ghAPI(ctx, "--paginate", "repos/"+s.repo()+"/commits/"+url.PathEscape(rev)+
		"/check-runs?filter=all&per_page=100")
	if err != nil {
		return nil, err
	}
	var runs []forgeCheck
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page struct {
			Runs []forgeCheck `json:"check_runs"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			return runs, nil
		} else if err != nil {
			return nil, err
		}
		runs = append(runs, page.Runs...)
	}
}

// latestCompleted is the latest completed run named name, nil when runs hold none.
func latestCompleted(runs []forgeCheck, name string) *forgeCheck {
	var latest *forgeCheck
	for i, r := range runs {
		if r.Name == name && r.Status == "completed" && (latest == nil || r.CompletedAt > latest.CompletedAt) {
			latest = &runs[i]
		}
	}
	return latest
}

// rerunFailed asks the forge to rerun the failed jobs of one workflow run.
func (s *session) rerunFailed(ctx context.Context, run int64) error {
	args := []string{"run", "rerun", strconv.FormatInt(run, 10), "--failed"}
	if s.cfg.GitHub.Repo != "" {
		args = append(args, "--repo", s.cfg.GitHub.Repo)
	}
	cmd := exec.CommandContext(ctx, "gh", args...) // #nosec G204 -- run comes from the forge and repo from project config
	cmd.Dir = s.cfg.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh run rerun: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// repo is [github] repo, or the placeholders gh fills from the project's remote.
func (s *session) repo() string {
	if s.cfg.GitHub.Repo != "" {
		return s.cfg.GitHub.Repo
	}
	return "{owner}/{repo}"
}

// ghAPI reads one forge path from the project root. Any flags precede the
// path.
func (s *session) ghAPI(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", append([]string{"api"}, args...)...) // #nosec G204 -- args are fixed flags and a path built from forge ids and project config
	cmd.Dir = s.cfg.Root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// recordCheck writes one classification to the ledger and the event stream. A
// row that will not write is reported and drive goes on: the classification
// already decided what happens to the item, and the ledger is its account.
func (s *session) recordCheck(id, sha string, j judged, rerun bool, act string) {
	row := ledger.Row{ID: id, Head: sha, Check: j.run.Name, Class: j.class, Evidence: j.evidence, Rerun: rerun, Date: ledger.Today()}
	if err := s.record(row); err != nil {
		fmt.Fprintf(errOut, "  · %s: the ledger would not record %s as %s: %v\n", id, row.Check, j.class, err)
	}
	events.emit(DriveEvent{Event: evCheck, ID: id, Action: act, Head: sha, Check: row.Check, Class: j.class, Evidence: j.evidence})
}

// checkURL is where a person reads a check: its own page, or the app's.
func checkURL(r forgeCheck) string {
	if r.HTMLURL != "" {
		return r.HTMLURL
	}
	return r.DetailsURL
}

// codeReason is a code failure as a park's reason names it: the check and the
// first line its log says failed, or what classify found when it read none.
func codeReason(j judged) string {
	if len(j.failure) > 0 {
		return j.run.Name + " failed: " + j.failure[0]
	}
	return j.run.Name + " failed: " + j.evidence
}

// codeEvidence is the lines a park carries for its code failures, each check's
// headed by its name where there is more than one.
func codeEvidence(code []judged) string {
	var lines []string
	for _, j := range code {
		if len(code) > 1 {
			lines = append(lines, j.run.Name+":")
		}
		if len(j.failure) > 0 {
			lines = append(lines, j.failure...)
		} else {
			lines = append(lines, j.evidence)
		}
	}
	return strings.Join(lines, "\n")
}

// noneFirst heads a gate's refusal with the report that no workflow ran on the
// head, where one was made, because that is a different fix from a red check:
// CI that never fires on a branch push rather than a check that failed.
func noneFirst(none, reason string) string {
	if none == "" {
		return reason
	}
	return none + "; " + reason
}

// recheckInfra reads the checks once more on the head of each item this drive
// parked infra_failure, before the drive ends. A rerun somebody started by
// hand, or one the forge retried on its own, turns such a head green while the
// queue runs, and without this the item waits for the operator to find it in
// status. A head that moved is somebody else's push and is left for the next
// drive, as is one whose checks are still red or unreadable.
//
// Every item the drive picked up replaces its entry in done. It returns the
// stop a pickup raised, nil when none did.
func (s *session) recheckInfra(ctx context.Context, done []DriveItem, deadline time.Time, enc *json.Encoder) error {
	for i, d := range done {
		if d.Outcome != outcomeParked || d.Park != parkInfraFailure || d.head == "" {
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if !s.headGreen(ctx, d) {
			fmt.Fprintf(errOut, "  · %s: checks on %s still red at the end of the drive; left parked\n", d.ID, shortSHA(d.head))
			continue
		}
		fmt.Fprintf(errOut, "  · %s: checks on %s went green after the park; picking it up\n", d.ID, shortSHA(d.head))
		r := s.pickUp(ctx, d, deadline)
		done[i] = r
		events.itemEvents(r)
		activeDrive.Load().revise(r)
		if driveJSON {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		if r.stop != nil {
			fmt.Fprintf(errOut, "· %v; stopping the drive\n", r.stop)
			return r.stop
		}
	}
	return nil
}

// headGreen is one read of the checks on d's recorded head: true when every
// run on it completed with a conclusion that merges and the branch still
// points there.
func (s *session) headGreen(ctx context.Context, d DriveItem) bool {
	runs, err := s.checkRuns(ctx, d.head)
	if err != nil || len(runs) == 0 || slices.ContainsFunc(runs, func(r forgeCheck) bool { return !r.green() }) {
		return false
	}
	now, err := revParse(s.cfg.Root, d.Branch)
	return err == nil && now == d.head
}

// pickUp sends an item parked on its checks back through the gate and the
// merge, on a session and worktree of its own as dispatch gives every item.
// The green row goes in first, so the ledger reads the park, the pickup and
// whatever the gate made of it in that order.
func (s *session) pickUp(ctx context.Context, d DriveItem, deadline time.Time) (res DriveItem) {
	d.Outcome, d.Park, d.Reason, d.Detail, d.Evidence, d.Log = "", "", "", "", "", ""
	d.Ended = ""
	defer func() {
		res.Ended = time.Now().Format(time.RFC3339)
		if res.Outcome == outcomeParked {
			s.recordPark(&res, nil, nil)
			s.announcePark(res)
		}
	}()
	park := func(cause, why string) DriveItem {
		d.Outcome, d.Park, d.Reason = outcomeParked, cause, why
		return d
	}
	w, err := s.fork(nil)
	if err != nil {
		return park("branch_failed", err.Error())
	}
	it, err := w.item(d.ID)
	if err != nil {
		return park("branch_failed", err.Error())
	}
	w.recordCheck(d.ID, d.head, judged{class: ledger.ClassGreen,
		evidence: "every check on this head passed when drive read it again at the end of the drive"}, false, "")
	tree, err := w.openItemTree(it, d.Branch, w.trunk())
	defer tree.close()
	if err != nil {
		if tree != nil {
			d.Tree = tree.Path
		}
		return park(parkWorktree, err.Error())
	}
	w.tree, d.Tree = tree.Path, tree.Path
	park = func(cause, why string) DriveItem {
		d.Outcome, d.Park, d.Reason = outcomeParked, cause, why
		w.leaveItem(&d)
		return d
	}
	return w.inTrunkStage(it, func() DriveItem { return w.finishAtHead(ctx, it, &d, park, w.trunk(), deadline, &settled{}) }, park)
}
