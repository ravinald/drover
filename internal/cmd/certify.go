package cmd

import (
	"fmt"
	"strings"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/gh"
	"github.com/ravinald/drover/internal/gitcmd"
	"github.com/ravinald/drover/internal/ledger"
	"github.com/spf13/cobra"
)

// parkAwaitingCertify is drive holding a merge under [git] signoff = "operator"
// until the operator certifies the head it would merge.
const parkAwaitingCertify = "awaiting_certify"

// parkForgedSignoff is a branch under [git] signoff whose own commits carry a
// Signed-off-by, which only the operator's certification may write.
const parkForgedSignoff = "forged_signoff"

// dcoVersion names the certificate a certify row records. Changing it changes
// what every later row claims, so it moves only with the text below.
const dcoVersion = "1.1"

// dcoText is the Developer Certificate of Origin 1.1, from
// https://developercertificate.org, the part a sign-off certifies. Its license
// permits verbatim copies only.
const dcoText = `Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.`

var certifyHead string

var certifyCmd = &cobra.Command{
	Use:   "certify <id> --head <sha>",
	Short: "Certify the DCO for the head of an item's pull request.",
	Long: `Under [git] signoff = "operator", nothing merges until the operator certifies the
Developer Certificate of Origin for the head that merges. certify records that,
for one item, bound to one head: the head of its open pull request, which the
item's last passing review read.

--head is required and must be that head. Typing it is the point: it binds the
certification to the diff you read, and a push after it leaves that push
uncertified. A trunk merge drive makes to catch the branch up is not new work
and does not void it; any other commit does.

The Signed-off-by is the identity git commit -s would use at the project root
(GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL, else user.name and user.email),
written onto the squash commit when the pull request merges. certify
prints the certificate, the diffstat and that identity, then appends one row.
It merges nothing: drover finish <id> or the next drive does, with the sign-off.

certify is the operator's: it refuses inside any session drover launched,
supervisor included. A sign-off is a person's statement about a diff, and no
session can make it for them.`,
	Example: `  drover show F55                        # the pull request and its head
  gh pr diff 123                          # read what you are certifying
  drover certify F55 --head 3f2a9c1
  drover finish F55                       # merges it, signed off

  A head that is not the pull request's, or one holding commits no review
  read, exits 3 and writes nothing.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		if len(args) < 1 || certifyHead == "" {
			return fmt.Errorf("certify needs the id and the head you read: drover certify <id> --head <sha>")
		}
		s, err := open()
		if err != nil {
			return err
		}
		if !s.cfg.Git.SignsOff() {
			return Refusal{fmt.Errorf("[git] signoff is not %q in this project, so no merge waits on a certification and certify would record one nothing reads",
				"operator")}
		}
		it, err := s.item(args[0])
		if err != nil {
			return err
		}
		id := it.ID.Bare()
		head, err := s.certifiableHead(it, certifyHead)
		if err != nil {
			return err
		}
		c, err := s.certifier()
		if err != nil {
			return err
		}
		c.Head, c.DCO = head, dcoVersion

		fmt.Println(dcoText)
		fmt.Println()
		if trunk, err := s.trunkRef(s.trunk()); err == nil {
			if stat, err := gitcmd.Command(s.cfg.Root, "diff", "--stat", trunk+"..."+head).Output(); err == nil {
				fmt.Print(string(stat))
			}
		}
		if err := s.record(ledger.Stamp(ledger.Row{ID: id, Certify: &c})); err != nil {
			return err
		}
		fmt.Printf("\ncertified %s at %s as %s\n", id, shortSHA(head), c.Trailer())
		fmt.Printf("next: drover finish %s merges it with that sign-off\n", id)
		return nil
	},
}

// certifiableHead resolves typed against the head of the item's open pull
// request and refuses unless they are one commit that the item's last passing
// review read, give or take trunk merges.
func (s *session) certifiableHead(it backlog.Item, typed string) (string, error) {
	id := it.ID.Bare()
	branch, err := s.branchFor(it)
	if err != nil {
		return "", err
	}
	if s.offline {
		return "", Refusal{fmt.Errorf("certify reads the pull request's head from the forge, and this session is offline")}
	}
	_, open, ok, err := gh.OpenPRHead(s.labels(), branch)
	switch {
	case err != nil:
		return "", fmt.Errorf("read the pull request for %s: %w", branch, err)
	case !ok || open == "":
		return "", Refusal{fmt.Errorf("%s has no open pull request, and certify certifies the head one carries: drover drive %s opens it once a review passes", branch, id)}
	}
	head, err := revParse(s.cfg.Root, typed)
	if err != nil {
		_ = gitcmd.Command(s.cfg.Root, "fetch", "--quiet", "origin", branch).Run()
		if head, err = revParse(s.cfg.Root, typed); err != nil {
			return "", Refusal{fmt.Errorf("--head %s names no commit here: %w", typed, err)}
		}
	}
	if head != open {
		return "", Refusal{fmt.Errorf("--head %s is not the head of %s's pull request, which is %s: read that diff and certify it",
			shortSHA(head), id, shortSHA(open))}
	}
	reviewed, commits, err := s.unreviewedSince(id, head)
	switch {
	case err != nil:
		return "", fmt.Errorf("list what %s holds after its last passing review: %w", head, err)
	case reviewed == "":
		return "", Refusal{fmt.Errorf("%s has no passing review, and certify certifies a head a review read", id)}
	case len(commits) > 0:
		return "", Refusal{fmt.Errorf("%s holds %d %s after %s, the head %s's last passing review read:\n%s\nreview it first: drover drive --review-only %s",
			shortSHA(head), len(commits), plural(len(commits), "commit", "commits"), shortSHA(reviewed), id,
			commitList(commits, "  "), id)}
	}
	return head, nil
}

// certifier is the identity `git commit -s` would sign as at the project
// root: git's committer ident, which honors GIT_COMMITTER_NAME and
// GIT_COMMITTER_EMAIL over user.name and user.email. user.useConfigOnly stops
// git falling back to a name and address it guessed from the host, which a
// sign-off must never carry.
func (s *session) certifier() (ledger.Certification, error) {
	c, err := s.committer()
	if err != nil {
		return c, Refusal{err}
	}
	return c, nil
}

// committer is certifier without the refusal, for check, which reports and
// never refuses.
func (s *session) committer() (ledger.Certification, error) {
	out, err := gitcmd.Command(s.cfg.Root, "-c", "user.useConfigOnly=true", "var", "GIT_COMMITTER_IDENT").CombinedOutput()
	ident := strings.TrimSpace(string(out))
	name, rest, ok := strings.Cut(ident, " <")
	email, _, closed := strings.Cut(rest, ">")
	if err != nil || !ok || !closed || strings.TrimSpace(name) == "" || email == "" {
		return ledger.Certification{}, fmt.Errorf("the Signed-off-by carries your git identity, and git has none set at %s: set user.name and user.email (git said: %s)",
			s.cfg.Root, firstLine(ident))
	}
	return ledger.Certification{Name: strings.TrimSpace(name), Email: email}, nil
}

// certification is the item's certification when it covers head, the head
// about to merge. A trunk merge past the certified head is drive catching the
// branch up and leaves it covered. A review: commit does not, unlike for the
// review's own coverage: the operator read the head, not what a reviewer
// added after it.
func (s *session) certification(id, head string) (ledger.Certification, error) {
	c, ok := s.ledger.Certified(id)
	if !ok {
		return c, fmt.Errorf("no drover certify row names %s", id)
	}
	if ledger.SameCommit(c.Head, head) {
		return c, nil
	}
	commits, err := commitsAfter(s.cfg.Root, s.trunk(), c.Head, head, false)
	switch {
	case err != nil:
		return c, fmt.Errorf("list what %s holds after %s, the head certified: %w", shortSHA(head), shortSHA(c.Head), err)
	case len(commits) > 0:
		return c, fmt.Errorf("%s holds %d %s after %s, the head certified:\n%s",
			shortSHA(head), len(commits), plural(len(commits), "commit", "commits"), shortSHA(c.Head), commitList(commits, "  "))
	}
	return c, nil
}

// certifyHold is the merge hold [git] signoff places: the park token, its
// reason and the command that lifts it, empty when the merge may go ahead.
func (s *session) certifyHold(it backlog.Item, head string) (park, reason, run string) {
	if !s.cfg.Git.SignsOff() {
		return "", "", ""
	}
	id := it.ID.Bare()
	_, err := s.certification(id, head)
	if err == nil {
		return "", "", ""
	}
	return parkAwaitingCertify,
		"[git] signoff holds the merge until the operator certifies " + shortSHA(head) + ": " + err.Error(),
		"read the diff, then drover certify " + id + " --head " + shortSHA(head)
}

// squashMessage is the body of the squash commit under [git] signoff: the
// branch's own commit subjects, as the forge's default squash message lists
// them, then the configured trailers and the operator's Signed-off-by.
func (s *session) squashMessage(id, head string, c ledger.Certification) string {
	var b strings.Builder
	var out []byte
	if trunk, err := s.trunkRef(s.trunk()); err == nil {
		out, _ = gitcmd.Command(s.cfg.Root, "log", "--reverse", "--no-merges", "--format=* %s", trunk+".."+head).Output()
	}
	if subjects := strings.TrimSpace(string(out)); subjects != "" {
		b.WriteString(subjects)
		b.WriteString("\n\n")
	}
	for _, t := range s.cfg.Git.Trailers {
		if line, ok := s.renderTrailer(id, t); ok {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString(c.Trailer())
	b.WriteString("\n")
	return b.String()
}

// renderTrailer fills {runner} and {model} from the item's ledger rows: the
// runner of its latest implement launch and the model its latest usage row
// names, which may be a review's: usage rows do not say which stage ran. A line asking for one the ledger cannot answer is dropped, because a
// trailer with a hole in it says something nobody recorded.
func (s *session) renderTrailer(id, t string) (string, bool) {
	var runner, model string
	for _, r := range s.ledger.Rows {
		if r.ID == id && r.Stage == ledger.StageImplement && r.Runner != "" {
			runner = r.Runner
		}
	}
	for _, r := range s.ledger.Usage {
		if r.ID == id && r.Model != "" {
			model = r.Model
		}
	}
	for key, val := range map[string]string{"{runner}": runner, "{model}": model} {
		if !strings.Contains(t, key) {
			continue
		}
		if val == "" {
			return "", false
		}
		t = strings.ReplaceAll(t, key, val)
	}
	return t, true
}

// forgedSignoffs is each commit past the trunk on branch, merges aside, whose
// message carries a Signed-off-by trailer, as git's own trailer parser reads
// it. Empty unless [git] signoff is set: without it a sign-off is the
// project's ordinary business.
func (s *session) forgedSignoffs(branch string) ([]string, error) {
	if !s.cfg.Git.SignsOff() {
		return nil, nil
	}
	trunk, err := s.trunkRef(s.trunk())
	if err != nil {
		return nil, err
	}
	out, err := gitcmd.Command(s.cfg.Root, "log", "--no-merges",
		"--format=%h %s%x00%(trailers:key=Signed-off-by,valueonly,separator=%x2C )", trunk+".."+branch, "--").Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..%s: %w", trunk, branch, err)
	}
	var forged []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		commit, by, _ := strings.Cut(line, "\x00")
		if by = strings.TrimSpace(by); by != "" {
			forged = append(forged, commit+" (Signed-off-by: "+by+")")
		}
	}
	return forged, nil
}

func init() {
	certifyCmd.Flags().StringVar(&certifyHead, "head", "", "the pull request head you read and certify (required)")
	root.AddCommand(certifyCmd)
}
