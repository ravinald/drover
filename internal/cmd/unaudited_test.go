package cmd

import (
	"strings"
	"testing"
)

// Critical was tallied into the total, bucketed nowhere, absent from the tag and
// worth zero in the ranking, so a Critical row sorted below a High. All four
// symptoms had one cause: four places that each hand-listed H, M and L.
func TestCriticalReachesTheTagAndTheRanking(t *testing.T) {
	crit := UnauditedRow{ID: "1", Blast: 3, FollowUps: 1, Critical: 1, FollowUpsKnown: true}
	high := UnauditedRow{ID: "2", Blast: 3, FollowUps: 1, High: 1, FollowUpsKnown: true}

	if got := sevTag(crit.counts()); got != "(1C)" {
		t.Errorf("sevTag = %q, want (1C)", got)
	}
	if !worseSev(crit.counts(), high.counts()) {
		t.Error("a Critical row does not outrank a High")
	}
	if worseSev(high.counts(), crit.counts()) {
		t.Error("a High row outranks a Critical")
	}
}

// The scheme this replaces packed counts into decimal digits, so it carried:
// ten Mediums scored exactly one High, and fifteen Lows outranked a Medium.
func TestRankingDoesNotCarryBetweenBuckets(t *testing.T) {
	for _, c := range []struct {
		name     string
		worse    []int
		thanThis []int
	}{
		{"one High beats ten Mediums", []int{0, 1, 0, 0}, []int{0, 0, 10, 0}},
		{"one Medium beats fifteen Lows", []int{0, 0, 1, 0}, []int{0, 0, 0, 15}},
		{"one Critical beats ninety-nine Highs", []int{1, 0, 0, 0}, []int{0, 99, 0, 0}},
	} {
		if !worseSev(c.worse, c.thanThis) {
			t.Errorf("%s: it does not", c.name)
		}
		if worseSev(c.thanThis, c.worse) {
			t.Errorf("%s: the comparison is not antisymmetric", c.name)
		}
	}
	// Equal counts are a tie, so the id tiebreak below it stays reachable.
	if worseSev([]int{1, 2, 3, 4}, []int{1, 2, 3, 4}) {
		t.Error("identical bucket counts compared unequal")
	}
}

// Blast radius still leads: severity only breaks a tie.
func TestBlastRadiusOutranksSeverity(t *testing.T) {
	rows := []UnauditedRow{
		{ID: "low-dependents-critical", Blast: 1, Critical: 9, FollowUpsKnown: true},
		{ID: "high-dependents-clean", Blast: 9, FollowUpsKnown: true},
	}
	sortUnaudited(rows)
	if rows[0].ID != "high-dependents-clean" {
		t.Errorf("first row is %q, want high-dependents-clean", rows[0].ID)
	}
}

func TestSevTagSaysNothingRatherThanZero(t *testing.T) {
	if got := sevTag([]int{0, 0, 0, 0}); got != "-" {
		t.Errorf("sevTag of an empty row = %q, want -", got)
	}
	got := sevTag([]int{2, 1, 0, 3})
	for _, want := range []string{"2C", "1H", "3L"} {
		if !strings.Contains(got, want) {
			t.Errorf("sevTag = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "M") {
		t.Errorf("sevTag = %q names a bucket that is empty", got)
	}
	// The cell is padded to 13, which is what "(9C 9H 9M 9L)" costs.
	if n := len(sevTag([]int{9, 9, 9, 9})); n > 13 {
		t.Errorf("widest tag is %d columns, wider than the cell that holds it", n)
	}
}

// The label vocabulary is the item vocabulary, so a tracker spelling a rank out
// buckets the same as one using the letter.
func TestBucketReadsTheLabelVocabulary(t *testing.T) {
	var r UnauditedRow
	for _, label := range []string{"C", "critical", "H", "High", "M", "m", "L", "low", "P0", ""} {
		r.bucket(label)
	}
	if r.FollowUps != 10 {
		t.Errorf("FollowUps = %d, want 10: an ungraded row is still open", r.FollowUps)
	}
	for _, c := range []struct {
		name string
		got  int
	}{{"Critical", r.Critical}, {"High", r.High}, {"Medium", r.Medium}, {"Low", r.Low}} {
		if c.got != 2 {
			t.Errorf("%s = %d, want 2", c.name, c.got)
		}
	}
	// P0 and "" are counted and ranked nowhere, which is the sum of the buckets
	// falling short of the total rather than either number being wrong.
	if sum := r.Critical + r.High + r.Medium + r.Low; sum != 8 {
		t.Errorf("ranked %d of %d rows, want 8", sum, r.FollowUps)
	}
}

// A project with one open row is the ordinary shape, not an edge case.
func TestUnauditedLineAgreesWithItsCounts(t *testing.T) {
	for _, c := range []struct {
		unaudited, risks int
		known            bool
		want             string
	}{
		{1, 1, true, "1 unaudited · 1 open follow-up attributed"},
		{4, 2, true, "4 unaudited · 2 open follow-ups attributed"},
		{3, 0, true, "3 unaudited · 0 open follow-ups attributed"},
		{2, 0, false, "2 unaudited · follow-ups unavailable, ranked on blast radius alone"},
	} {
		if got := unauditedLine(c.unaudited, c.risks, c.known); got != c.want {
			t.Errorf("unauditedLine(%d, %d, %v) = %q, want %q", c.unaudited, c.risks, c.known, got, c.want)
		}
	}
}
