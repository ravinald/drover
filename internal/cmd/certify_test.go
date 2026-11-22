package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/config"
	"github.com/ravinald/drover/internal/ledger"
)

// certifiedRepo is a repository whose branch b1 carries one commit, with a
// session that signs off and certifies that commit for item 1.
func certifiedRepo(t *testing.T) (s *session, dir, head string) {
	t.Helper()
	dir = newRepo(t)
	git(t, dir, "switch", "-q", "-c", "b1")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "the handler returns its error")
	head = git(t, dir, "rev-parse", "b1")
	s = &session{cfg: &config.Config{Root: dir}, ledger: &ledger.Ledger{}}
	s.cfg.Run.Trunk = "main"
	s.cfg.Git.MergeMethod = "squash"
	s.cfg.Git.Signoff = config.SignoffOperator
	s.ledger.Rows = append(s.ledger.Rows, ledger.Row{ID: "1", Certify: &ledger.Certification{
		Head: head, Name: "Op Erator", Email: "op@example.invalid", DCO: dcoVersion,
	}})
	return s, dir, head
}

// A certification covers the head it names and the trunk merges drive makes
// to catch that head up. Any other commit is uncertified, a review: commit
// included: the operator read the head, not what came after it.
func TestCertificationCoversTrunkMergesAndNothingElse(t *testing.T) {
	s, dir, head := certifiedRepo(t)
	if _, err := s.certification("1", head); err != nil {
		t.Fatalf("the certified head is not covered: %v", err)
	}
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "trunk moved")
	git(t, dir, "switch", "-q", "b1")
	git(t, dir, "merge", "-q", "--no-edit", "main")
	merged := git(t, dir, "rev-parse", "b1")
	if _, err := s.certification("1", merged); err != nil {
		t.Errorf("a trunk merge past the certified head voided it: %v", err)
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "review: a typo")
	after := git(t, dir, "rev-parse", "b1")
	if _, err := s.certification("1", after); err == nil || !strings.Contains(err.Error(), "review: a typo") {
		t.Errorf("a commit after the certified head is covered, or unnamed: %v", err)
	}
	if _, err := s.certification("2", head); err == nil {
		t.Error("an item nobody certified is covered")
	}
}

// argStub is a gh that records each argument on its own line, so a body that
// spans lines is read back whole.
func argStub(t *testing.T) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "gh.log")
	t.Setenv("DROVER_TEST_GH_LOG", log)
	stub(t, filepath.Join(bin, "gh"), "#!/bin/sh\nfor a; do printf '%s\\n' \"$a\" >> \"$DROVER_TEST_GH_LOG\"; done\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// Under [git] signoff the squash commit's body is drover's: the branch's
// subjects, each trailer the ledger can fill, and the operator's sign-off. A
// trailer naming what the ledger cannot answer is left out.
func TestSignedOffMergeCarriesTheTrailers(t *testing.T) {
	for _, c := range []struct {
		name    string
		usage   bool
		want    []string
		missing []string
	}{
		{name: "both known", usage: true,
			want: []string{"Assisted-by: claude:claude-opus-5-5\n", "Ticket: none\n"}},
		{name: "model unknown",
			want: []string{"Ticket: none\n"}, missing: []string{"Assisted-by"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _, head := certifiedRepo(t)
			log := argStub(t)
			s.cfg.Git.Trailers = []string{"Assisted-by: {runner}:{model}", "Ticket: none"}
			s.ledger.Rows = append(s.ledger.Rows, ledger.Row{ID: "1", Stage: ledger.StageImplement, Runner: "claude"})
			if c.usage {
				s.ledger.Usage = append(s.ledger.Usage, ledger.Row{ID: "1", Model: "claude-opus-5-5"})
			}
			if err := s.mergeBranch(t.Context(), "1", "b1"); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(log) // #nosec G304 -- written by the stub above
			if err != nil {
				t.Fatal(err)
			}
			got := string(raw)
			for _, w := range append(c.want,
				"--match-head-commit\n"+head+"\n--body\n",
				"* the handler returns its error\n\n",
				"Signed-off-by: Op Erator <op@example.invalid>\n\nb1\n",
			) {
				if !strings.Contains(got, w) {
					t.Errorf("gh saw no %q:\n%s", w, got)
				}
			}
			for _, m := range c.missing {
				if strings.Contains(got, m) {
					t.Errorf("gh saw %q, a trailer the ledger cannot fill:\n%s", m, got)
				}
			}
		})
	}
}

// The merge reads the certification again at the head it binds, and asks the
// forge for nothing when none covers it.
func TestUncertifiedMergeIsRefusedBeforeTheForge(t *testing.T) {
	s, dir, _ := certifiedRepo(t)
	log := argStub(t)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "after the certificate")
	err := s.mergeBranch(t.Context(), "1", "b1")
	if err == nil || !strings.Contains(err.Error(), "drover certify 1 --head") {
		t.Fatalf("merge = %v, want a refusal naming the certify to run", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Error("the forge was asked to merge an uncertified head")
	}
}

// signoffProject is mergeProject with [git] signoff = "operator".
func signoffProject(t *testing.T) (dir, ghLog string) {
	t.Helper()
	ghLog = mergeProject(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, config.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the fixture's own config
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw)+"\n[git]\nsignoff = \"operator\"\ntrailers = [\"Ticket: none\"]\n")
	write(t, filepath.Join(dir, "work", "01-first.md"),
		"---\ntitle: First\ndeps:\n---\n\n1. Do it. REVIEW-PASSES\n")
	return dir, ghLog
}

// A drive under [git] signoff parks a reviewed, green item with its pull
// request open and needs_operator naming the certify, and merges nothing.
// certify then records the head, and the operator's finish merges it with
// the sign-off.
func TestTheMergeHoldsUntilCertify(t *testing.T) {
	dir, ghLog := signoffProject(t)
	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	var parked, needs *DriveEvent
	for i, e := range evs {
		switch {
		case e.Event == evParked && e.ID == "1":
			parked = &evs[i]
		case e.Event == evNeedsOperator && e.ID == "1":
			needs = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkAwaitingCertify || parked.PR == 0 {
		t.Fatalf("item 1 parked %+v, want %s with its pull request open", parked, parkAwaitingCertify)
	}
	if needs == nil || needs.Action != "certify" || !strings.Contains(needs.Run, "drover certify 1 --head ") {
		t.Fatalf("needs_operator is %+v, want certify naming the command", needs)
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr merge") {
			t.Fatalf("the forge was asked to merge an uncertified item: %s", c)
		}
	}

	l, err := ledger.Load(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var branch, head string
	for _, r := range l.Rows {
		if r.ID == "1" && r.Branch != "" && r.Head != "" {
			branch, head = r.Branch, r.Head
		}
	}
	if head == "" {
		t.Fatal("the drive recorded no head for item 1")
	}
	open, err := json.Marshal([]map[string]any{{"number": 7, "headRefOid": head}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROVER_TEST_GH_OPEN", string(open))

	if err := execute(t, "certify", "1", "--head", "0000000"); err == nil {
		t.Error("certify took a head that is not the pull request's")
	}
	if err := execute(t, "certify", "1", "--head", head[:9]); err != nil {
		t.Fatalf("certify: %v", err)
	}
	if l, err = ledger.Load(filepath.Join(dir, "ledger.jsonl")); err != nil {
		t.Fatal(err)
	}
	c, ok := l.Certified("1")
	if !ok || c.Head != head || c.Email != "test@example.invalid" || c.DCO != dcoVersion {
		t.Fatalf("certify row is %+v, want head %s under the operator's identity", c, head)
	}

	if err := execute(t, "finish", "1"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	raw, err := os.ReadFile(ghLog) // #nosec G304 -- written by the stub
	if err != nil {
		t.Fatal(err)
	}
	merge := string(raw)
	merge = merge[strings.Index(merge, "pr merge"):]
	for _, w := range []string{"--squash --match-head-commit " + head + " --body ", "Ticket: none\nSigned-off-by: drover-test <test@example.invalid>\n", branch} {
		if !strings.Contains(merge, w) {
			t.Errorf("the merge carried no %q:\n%s", w, merge)
		}
	}
}

// certify is refused where nothing would read it, and needs the head.
func TestCertifyRefusesWithoutSignoff(t *testing.T) {
	mergeProject(t)
	err := execute(t, "certify", "1", "--head", "abc")
	var r Refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "[git] signoff") {
		t.Errorf("certify without [git] signoff = %v, want a refusal naming it", err)
	}
	if err := execute(t, "certify", "1"); err == nil || !strings.Contains(err.Error(), "--head") {
		t.Errorf("certify without --head = %v, want a usage error naming it", err)
	}
}

// A sign-off names a person, so certify refuses an identity git would only
// have guessed from the host.
func TestCertifierRefusesAGuessedIdentity(t *testing.T) {
	dir := newRepo(t)
	for _, k := range []string{"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	s := &session{cfg: &config.Config{Root: dir}}
	if c, err := s.certifier(); err == nil {
		t.Fatalf("certifier = %+v with no identity configured", c)
	}
	t.Setenv("GIT_COMMITTER_NAME", "Op Erator")
	t.Setenv("GIT_COMMITTER_EMAIL", "op@example.invalid")
	c, err := s.certifier()
	if err != nil || c.Trailer() != "Signed-off-by: Op Erator <op@example.invalid>" {
		t.Errorf("certifier = %+v, %v; want the committer ident", c, err)
	}
}

// Under [git] signoff a branch commit carrying a Signed-off-by, anywhere git's
// trailer parser finds one, is a sign-off nobody certified. A merge commit and
// a subject that only mentions the trailer are not.
func TestForgedSignoffsReadsTrailers(t *testing.T) {
	s, dir, _ := certifiedRepo(t)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "mention Signed-off-by: in prose")
	if got, err := s.forgedSignoffs("b1"); err != nil || len(got) != 0 {
		t.Fatalf("forgedSignoffs = %v, %v on a branch nobody signed off", got, err)
	}
	git(t, dir, "commit", "-q", "--allow-empty", "-s", "-m", "work")
	got, err := s.forgedSignoffs("b1")
	if err != nil || len(got) != 1 || !strings.Contains(got[0], "work (Signed-off-by: drover-test <test@example.invalid>)") {
		t.Errorf("forgedSignoffs = %q, %v; want the signed commit", got, err)
	}
	s.cfg.Git.Signoff = ""
	if got, _ := s.forgedSignoffs("b1"); len(got) != 0 {
		t.Errorf("without [git] signoff a sign-off is the project's business, got %q", got)
	}
}

// A session that signs off parks before the push, so the forge never holds a
// sign-off the operator did not make, and no pull request opens.
func TestDriveParksASessionThatSignsOff(t *testing.T) {
	_, ghLog := signoffProject(t)
	t.Setenv("DROVER_TEST_IMPL_SIGNOFF", "1")
	evs, stderr, err := driveEventsOf(t, "1")
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, stderr)
	}
	var parked *DriveEvent
	for i, e := range evs {
		if e.Event == evParked && e.ID == "1" {
			parked = &evs[i]
		}
	}
	if parked == nil || parked.Park != parkForgedSignoff || !strings.Contains(parked.Reason, "the run did something") {
		t.Fatalf("item 1 parked %+v, want %s naming the commit", parked, parkForgedSignoff)
	}
	for _, c := range ghCalls(t, ghLog) {
		if strings.HasPrefix(c, "pr create") || strings.HasPrefix(c, "pr merge") {
			t.Errorf("the forge was reached for a branch carrying a forged sign-off: %s", c)
		}
	}
}

// check names a signoff project with nobody to sign as.
func TestCheckReportsNoSigner(t *testing.T) {
	s, _, _ := certifiedRepo(t)
	var got []Finding
	add := func(f Finding) { got = append(got, f) }
	s.addSigner(add)
	if len(got) != 0 {
		t.Fatalf("a configured identity reported %+v", got)
	}
	for _, k := range []string{"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	s.addSigner(add)
	if len(got) != 1 || got[0].Kind != "NO SIGNER" {
		t.Errorf("findings = %+v, want NO SIGNER", got)
	}
}
