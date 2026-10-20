package gh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func index() Index {
	return Index{OK: true, Issues: []Issue{
		{Number: 87, Title: "old gap", Owners: []string{"20"}, Severity: "L"},
		{Number: 234, Title: "muted friend", Owners: []string{"2f"}, Severity: "M"},
		{Number: 238, Title: "empty feed", Owners: []string{"F11", "11"}, Severity: "L"},
		{Number: 81, Title: "broadcast reaches nobody", Owners: []string{"2b"}},
		{Number: 999, Title: "nobody owns this"},
	}}
}

// A row owned by both an item and one of its dependencies must appear once.
func TestForDedupesAcrossOwners(t *testing.T) {
	got := index().For("F11", "11", "2f")
	var nums []int
	for _, is := range got {
		nums = append(nums, is.Number)
	}
	if want := []int{234, 238}; !reflect.DeepEqual(nums, want) {
		t.Errorf("got %v, want %v", nums, want)
	}
}

func TestForReturnsNothingForAnUnknownID(t *testing.T) {
	if got := index().For("nope"); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// A row with no routing label is deliberately inert: it records provenance for
// a human and reaches no run.
func TestUnroutedIssueReachesNobody(t *testing.T) {
	for _, id := range []string{"20", "2f", "F11", "11", "2b"} {
		for _, is := range index().For(id) {
			if is.Number == 999 {
				t.Fatalf("an unowned issue reached %s", id)
			}
		}
	}
}

// The zero Index means "could not ask", which must never read as "none filed".
func TestZeroIndexIsNotOK(t *testing.T) {
	var ix Index
	if ix.OK {
		t.Error("the zero Index must not claim to have read the register")
	}
	if len(ix.For("1")) != 0 {
		t.Error("an unreadable register yields nothing")
	}
}

func TestTruncatedOnlyWhenRead(t *testing.T) {
	var unreadable Index
	if unreadable.Truncated(2) {
		t.Error("an unread register cannot be truncated")
	}
	full := Index{OK: true, Issues: make([]Issue, 5)}
	if !full.Truncated(5) {
		t.Error("hitting the page limit must be reported")
	}
	if full.Truncated(50) {
		t.Error("a short read is not truncated")
	}
}

func TestScopeAddsRepoOnlyWhenSet(t *testing.T) {
	base := []string{"issue", "list"}
	if got := (Labels{}).scope(base); len(got) != 2 {
		t.Errorf("no repo configured should leave args alone, got %v", got)
	}
	got := Labels{Repo: "owner/name"}.scope(base)
	if want := []string{"issue", "list", "--repo", "owner/name"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// git ancestry cannot answer whether a branch merged under squash or rebase
// merge — both replace the branch's commits, so `git branch --merged` reports
// unmerged forever. The tracker is the authority; ancestry is the offline
// fallback and says so.
func TestMergedPRNeedsGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, merged, err := MergedPR(Labels{}, "some-branch"); err == nil || merged {
		t.Errorf("without gh this must error rather than answer: merged=%v err=%v", merged, err)
	}
}

// stubGHReads answers both reads from one fixture: `issue list` returns the
// array, `issue view N` returns the element with that number. A field the two
// paths disagree on then fails here rather than on the live tracker.
func stubGHReads(t *testing.T, issues map[int]string) Labels {
	t.Helper()
	nums := make([]int, 0, len(issues))
	for n := range issues {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	objs := make([]string, 0, len(nums))
	var arms strings.Builder
	for _, n := range nums {
		objs = append(objs, issues[n])
		fmt.Fprintf(&arms, "%d) printf '%%s' '%s' ;;\n", n, issues[n])
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'issue list') printf '%s' '[" + strings.Join(objs, ",") + "]' ;;\n" +
		"'issue view') case \"$3\" in\n" + arms.String() +
		"*) echo \"stub gh: no issue $3\" >&2; exit 1 ;;\n" +
		"esac ;;\n" +
		"*) echo \"stub gh: unexpected $*\" >&2; exit 1 ;;\n" +
		"esac\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return Labels{Dir: dir, Repo: "owner/name", FollowUp: "follow-up", Severity: "severity:",
		ItemIssues: map[int]string{10: "B10", 9: "E9"}}
}

// One issue read two ways is one issue. A View that copied the label names and
// returned would leave Severity and Owners zero on every single-issue read
// while Load filled both from the same labels.
func TestViewAgreesWithLoadFieldForField(t *testing.T) {
	labels := stubGHReads(t, map[int]string{
		44: `{"number":44,"title":"ranked bug","body":"b","state":"OPEN","parent":{"number":10},` +
			`"labels":[{"name":"bug"},{"name":"severity:low"}]}`,
		51: `{"number":51,"title":"ranked follow-up","body":"c\nOwner: B10","state":"OPEN",` +
			`"labels":[{"name":"follow-up"},{"name":"severity:low"}]}`,
		52: `{"number":52,"title":"ranked enhancement","body":"e","state":"OPEN","parent":{"number":9},` +
			`"labels":[{"name":"enhancement"},{"name":"severity:medium"}]}`,
		53: `{"number":53,"title":"ungraded bug","body":"u","state":"OPEN",` +
			`"labels":[{"name":"bug"}]}`,
	})
	ix := Load(labels)
	if !ix.OK {
		t.Fatalf("load: %v", ix.Reason())
	}
	if len(ix.Issues) != 4 {
		t.Fatalf("loaded %d issues, want 4", len(ix.Issues))
	}
	for _, want := range ix.Issues {
		got, err := View(labels, want.Number)
		if err != nil {
			t.Fatalf("view #%d: %v", want.Number, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("#%d: View returned %+v, Load returned %+v", want.Number, got, want)
		}
	}
}

// The follow-up list is the issues carrying the follow-up label and nothing else. Asking
// for it locally instead would read a repository's whole open backlog to find
// eight rows, and spend the page limit on issues that were never in the
// register — so a project with 900 open issues would report a cut-short read of
// a register that is complete.
func TestLoadAsksTheTrackerForMembership(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'auth status') exit 0 ;;\n" +
		"'issue list') printf '%s\\n' \"$*\" > " + argv + "; printf '[]' ;;\n" +
		"*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	for _, c := range []struct {
		name string
		risk string
		want bool
	}{
		{"configured", "follow-up", true},
		// An empty label would render `--label ""`, which gh rejects. Defaults
		// refill the key, so reaching this means a caller built Labels by hand.
		{"empty", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if ix := Load(Labels{Dir: dir, FollowUp: c.risk}); !ix.OK {
				t.Fatalf("load: %v", ix.Reason())
			}
			out, err := os.ReadFile(argv) // #nosec G304 -- a path this test just wrote
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(out), "--label follow-up"); got != c.want {
				t.Errorf("argv %q carries --label = %v, want %v", strings.TrimSpace(string(out)), got, c.want)
			}
		})
	}
}

func TestPRNumberIsReadOffTheURL(t *testing.T) {
	for _, c := range []struct {
		out  string
		want int
	}{
		{"https://github.com/o/r/pull/172\n", 172},
		{"\nhttps://example.invalid/o/r/pull/7", 7},
		{"Warning: 1 uncommitted change\nhttps://github.com/o/r/pull/9\n", 9},
		{"", 0},
		{"https://github.com/o/r/pull/not-a-number\n", 0},
	} {
		if got := prNumberIn(c.out); got != c.want {
			t.Errorf("prNumberIn(%q) = %d, want %d", c.out, got, c.want)
		}
	}
}

// stubGH puts a gh on PATH that answers `pr list` with what the caller names,
// records every call, and returns the log's path.
func stubGH(t *testing.T, list string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"  'pr list')   " + list + " ;;\n" +
		"  'pr create') echo 'https://example.invalid/o/r/pull/9' ;;\n" +
		"  *)           echo '[]' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log) // #nosec G304 -- written by the stub above
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The reuse is the package's, not the caller's. `gh pr create` exits non-zero
// on a branch that already has a pull request, so every caller that skipped the
// read would report a forge failure over a document that exists and is fine.
func TestCreateReusesThePullRequestAlreadyOpen(t *testing.T) {
	for _, c := range []struct {
		name, list string
		want       int
		create     bool
	}{
		{name: "one is already open", list: "echo '[{\"number\":7}]'", want: 7},
		{name: "none is", list: "echo '[]'", want: 9, create: true},
		// gh omits a zero it never assigns; a row carrying one is not a pull
		// request to reuse, and opening is the safe reading.
		{name: "the read answered with no number", list: "echo '[{\"number\":0}]'", want: 9, create: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := stubGH(t, c.list)
			n, err := CreatePR(Labels{Repo: "o/r"}, "topic", "main", "T: a title", "body")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if n != c.want {
				t.Errorf("reported #%d, want #%d", n, c.want)
			}
			var created bool
			for _, call := range calls(t, log) {
				if strings.HasPrefix(call, "pr create") {
					created = true
					if !strings.Contains(call, "--repo o/r") {
						t.Errorf("the create left the configured repo behind: %q", call)
					}
				}
			}
			if created != c.create {
				t.Errorf("opened a pull request = %v, want %v", created, c.create)
			}
		})
	}
}

// A read that failed is not a branch with no pull request. Treating it as one
// opens a second over whatever is there, so the error propagates and names the
// head it was asked about.
func TestCreateDoesNotOpenOverAReadItCouldNotMake(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2\" = 'auth status' ]; then exit 0; fi\n" +
		"echo 'gh: HTTP 502' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if n, err := CreatePR(Labels{}, "topic", "main", "T: a title", "body"); err == nil {
		t.Fatalf("a forge that would not answer opened #%d", n)
	} else if !strings.Contains(err.Error(), "topic") {
		t.Errorf("the error does not name the head: %v", err)
	}
}

// A title is what a person reads in a list of open pull requests, and gh's own
// refusal names neither the branch nor the caller.
func TestCreateRefusesAPullRequestWithNoTitle(t *testing.T) {
	stubGH(t, "echo '[]'")
	if _, err := CreatePR(Labels{}, "topic", "main", "  ", "body"); err == nil {
		t.Fatal("opened a pull request with no title")
	}
}

// Owners come from two links and nothing else: the sub-issue parent, when it is
// an item's issue, and each `Owner:` line. A parent that is nobody's item issue
// is an epic, and a label that once named an owner names none.
func TestOwnersAreTheParentItemAndEveryOwnerLine(t *testing.T) {
	l := Labels{Severity: "severity:", ItemIssues: map[int]string{10: "B10", 20: "E4"}}
	parent := func(n int) *struct {
		Number int `json:"number"`
	} {
		return &struct {
			Number int `json:"number"`
		}{n}
	}
	for _, c := range []struct {
		name string
		raw  rawIssue
		want []string
	}{
		{"sub-issue owner", rawIssue{Number: 1, Parent: parent(10)}, []string{"B10"}},
		{"owner line", rawIssue{Number: 2, Body: "found in review\n\nOwner: B7\n"}, []string{"B7"}},
		{"two owner lines", rawIssue{Number: 3, Body: "Owner: B7\r\n  Owner: E4  \n"}, []string{"B7", "E4"}},
		{"both at once", rawIssue{Number: 4, Parent: parent(20), Body: "Owner: B7\nOwner: E4"}, []string{"E4", "B7"}},
		{"neither", rawIssue{Number: 5, Body: "Owner: B7, maybe B8\nowner: B9\nthe Owner: B6 line"}, nil},
		{"parent that is no item's issue", rawIssue{Number: 6, Parent: parent(99)}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := l.issue(c.raw).Owners
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Owners = %q, want %q", got, c.want)
			}
		})
	}
	is := l.issue(rawIssue{Number: 7, Labels: []struct {
		Name string `json:"name"`
	}{{Name: "owner:B10"}, {Name: "severity:high"}}})
	if len(is.Owners) != 0 || is.Severity != "high" {
		t.Errorf("owners %q severity %q: want no owner from a label and severity high", is.Owners, is.Severity)
	}
}

// Owned searches each id's Owner: line and walks each owner item's sub-issues,
// returns each issue once however many reads listed it, and keeps out what the
// follow-up list already holds and what a search matched loosely.
func TestOwnedReadsBothLinksAndDropsFollowUps(t *testing.T) {
	const search = `[{"number":44,"title":"bug on both","body":"Owner: B1\nOwner: B2","state":"OPEN",` +
		`"labels":[{"name":"bug"}]},` +
		`{"number":51,"title":"a follow-up","body":"Owner: B1","state":"OPEN",` +
		`"labels":[{"name":"follow-up"}]},` +
		`{"number":60,"title":"loose match","body":"the owner: b1 said","state":"OPEN","labels":[]}]`
	const child = `{"number":70,"title":"sub-issue bug","body":"","state":"OPEN","parent":{"number":5},"labels":[{"name":"bug"}]}`
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$1 $2\" in\n" +
		"'issue list') printf '%s' '" + search + "' ;;\n" +
		"'issue view') case \"$3 $5\" in\n" +
		"'5 subIssues') printf '%s' '{\"subIssues\":{\"nodes\":[{\"number\":70},{\"number\":44}]}}' ;;\n" +
		"70*) printf '%s' '" + child + "' ;;\n" +
		"*) exit 1 ;;\nesac ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	labels := Labels{Dir: dir, Repo: "owner/name", FollowUp: "follow-up", ItemIssues: map[int]string{5: "B1"}}

	got, err := Owned(labels, "B1", "B2")
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, is := range got {
		nums = append(nums, is.Number)
	}
	if !reflect.DeepEqual(nums, []int{44, 70}) || !reflect.DeepEqual(got[0].Owners, []string{"B1", "B2"}) ||
		!reflect.DeepEqual(got[1].Owners, []string{"B1"}) {
		t.Errorf("Owned = %+v, want #44 owned by B1 and B2, then #70 owned by B1", got)
	}
	b, err := os.ReadFile(log) // #nosec G304 -- written by the stub above
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 4 {
		t.Fatalf("gh calls: %q, want a search per id, one sub-issue list and one view", calls)
	}
	for i, id := range []string{"B1", "B2"} {
		for _, want := range []string{"issue list --state open", `--search "Owner: ` + id + `" in:body`, "--repo owner/name"} {
			if !strings.Contains(calls[i], want) {
				t.Errorf("call %d %q lacks %q", i, calls[i], want)
			}
		}
	}
}

// A read that fails is an error, never an empty list: the caller renders why.
func TestOwnedReportsAFailedRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\necho nope >&2\nexit 1\n"), 0o700); err != nil { // #nosec G306 -- a stub has to be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := Owned(Labels{Dir: dir, FollowUp: "follow-up"}, "B1")
	if err == nil || !strings.Contains(err.Error(), "Owner: B1") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Owned = %v, %v; want an error naming the search and gh's stderr", got, err)
	}
}

// gh tells a checkout whose remotes name no GitHub host to run gh auth login,
// and exits 1. That user may well be logged in, so the advice is not the
// classification: exit 4 is.
func TestUnreadDecidesAuthOnTheExitCode(t *testing.T) {
	exited := func(code int, stderr string) error {
		err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
		return fmt.Errorf("%w: %s", err, stderr)
	}
	noHost := "none of the git remotes configured for this repository point to a known GitHub host. To tell gh about a new GitHub host, please use `gh auth login`"
	for _, c := range []struct {
		err  error
		want string
	}{
		{exited(1, noHost), UnreadNoGitHub},
		{exited(1, "no git remotes found"), UnreadNoGitHub},
		{exited(4, "To get started with GitHub CLI, please run:  gh auth login"), UnreadAuth},
		{exited(1, "HTTP 401: Bad credentials (https://api.github.com/graphql)\nTry authenticating with:  gh auth login"), UnreadAuth},
		{exited(1, "something else entirely; gh auth login"), UnreadFailed},
		{fmt.Errorf("gh timed out after 30s"), UnreadUnreachable},
	} {
		if got := unread(c.err).Reason; got != c.want {
			t.Errorf("unread(%q) = %q, want %q", c.err, got, c.want)
		}
	}
}
