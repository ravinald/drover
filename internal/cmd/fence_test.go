package cmd

import (
	"strings"
	"testing"

	"github.com/ravinald/drover/internal/backlog"
	"github.com/ravinald/drover/internal/config"
)

// Each want marks a line skipped (s) or read (r), in order.
func TestFenceSkip(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"four backticks hold three", []string{"````markdown", "```", "inner", "```", "outer", "````", "prose"}, "ssssssr"},
		{"tilde fence holds a backtick line", []string{"~~~", "```", "inner", "~~~", "prose"}, "ssssr"},
		{"closed by a longer run", []string{"```", "inner", "`````", "prose"}, "sssr"},
		{"closing run followed by text", []string{"```", "``` not a close", "inner", "```", "prose"}, "ssssr"},
		{"backtick in a backtick info string", []string{"```go `x`", "prose", "```", "inner"}, "rrss"},
		{"backtick in a tilde info string", []string{"~~~ `x`", "inner", "~~~", "prose"}, "sssr"},
		{"unclosed fence", []string{"prose", "```", "inner", "# heading"}, "rsss"},
		{"three spaces of indent", []string{"   ```", "inner", "   ```", "prose"}, "sssr"},
		{"four spaces of indent is not a fence", []string{"    ```", "prose"}, "rr"},
		{"two characters are not a fence", []string{"``", "prose"}, "rr"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var f fence
			var got strings.Builder
			for _, l := range c.lines {
				if f.skip(l) {
					got.WriteByte('s')
				} else {
					got.WriteByte('r')
				}
			}
			if got.String() != c.want {
				t.Errorf("skip = %s, want %s", got.String(), c.want)
			}
		})
	}
}

// nestedQuote is a four-backtick fence quoting a Markdown snippet with its own
// three-backtick fence, which a toggle closed at the inner opener.
func nestedQuote(inner string) string {
	return "````markdown\n```\n" + inner + "\n```\n" + inner + "\n````\n"
}

func TestBodyHoldsSkipsANestedFence(t *testing.T) {
	lines := strings.Split("Usage:\n\n"+nestedQuote("drover show <id>")+"after", "\n")
	if got := bodyHolds(lines, config.DefaultHoldMarkers); len(got) != 0 {
		t.Errorf("a nested fenced example was reported: %+v", got)
	}
}

func TestMarkerLineSkipsANestedFence(t *testing.T) {
	const marker = "STOP: requirements not yet written"
	if got := markerLine("prose\n"+nestedQuote(marker), marker); got >= 0 {
		t.Errorf("markerLine found the quoted marker at line %d", got)
	}
	if got := markerLine("~~~\n```\n"+marker+"\n~~~\n", marker); got >= 0 {
		t.Errorf("a backtick line closed a tilde fence: marker found at line %d", got)
	}
}

func TestOperatorOnlyFindingsSkipsANestedFence(t *testing.T) {
	it := backlog.Item{Body: "1. [operator] a live check\n2. code\n"}
	text := "## R1 needs the live check\n\n" + nestedQuote("## R2 is broken")
	ops, _ := operatorOnlyFindings(it, text)
	if len(ops) != 1 || ops[0].Num != 1 {
		t.Errorf("operatorOnlyFindings = %+v, want R1 alone: the quoted R2 heading was read", ops)
	}
}

func TestUnfencedDropsANestedFence(t *testing.T) {
	got := unfenced("before\n" + nestedQuote("## R2 is broken") + "after")
	if got != "before\nafter" {
		t.Errorf("unfenced = %q, want %q", got, "before\nafter")
	}
}
