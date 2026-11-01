package cmd

import (
	"strings"
)

// diffLine is one line of a line diff: kept, removed or added, and for a
// removed line paired with the added line that rewrote it, the byte spans of
// the words that differ between the two.
type diffLine struct {
	op      byte // ' ', '-', '+'
	text    string
	changed [][2]int
}

// lineDiff is the shortest edit from a to b, line by line, with each removed
// line that an added line rewrote rather than replaced paired to it.
//
// A longest-common-subsequence table is quadratic in lines, which is the right
// trade for a prompt block of a few dozen and the wrong one for a file.
func lineDiff(a, b string) []diffLine {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			out = append(out, diffLine{op: ' ', text: x[i]})
			i, j = i+1, j+1
		// Removals first on a tie, so a rewrite reads old line then new.
		case j < len(y) && (i == len(x) || lcs[i][j+1] > lcs[i+1][j]):
			out = append(out, diffLine{op: '+', text: y[j]})
			j++
		default:
			out = append(out, diffLine{op: '-', text: x[i]})
			i++
		}
	}
	return pairRewrites(out)
}

// pairRewrites marks the changed words of each removed line that the added
// line in the same position of the same run of changes rewrote, rather than
// replaced: at least half the shorter line's words survive, in order.
func pairRewrites(lines []diffLine) []diffLine {
	for start := 0; start < len(lines); {
		if lines[start].op == ' ' {
			start++
			continue
		}
		end := start
		for end < len(lines) && lines[end].op != ' ' {
			end++
		}
		var removed, added []int
		for k := start; k < end; k++ {
			if lines[k].op == '-' {
				removed = append(removed, k)
			} else {
				added = append(added, k)
			}
		}
		for n := 0; n < len(removed) && n < len(added); n++ {
			r, a := &lines[removed[n]], &lines[added[n]]
			r.changed, a.changed = wordChanges(r.text, a.text)
		}
		start = end
	}
	return lines
}

// words splits s into runs of non-space with the space before each attached,
// so the spans they give cover the whole line and rejoin to it exactly.
func words(s string) [][2]int {
	var out [][2]int
	from := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || (s[i] == ' ' && s[i-1] != ' ') {
			out = append(out, [2]int{from, i})
			from = i
		}
	}
	return out
}

// wordChanges is the spans of a and of b outside their longest common run of
// words, or nil for both when too little survives for b to read as an edit.
func wordChanges(a, b string) (ca, cb [][2]int) {
	wa, wb := words(a), words(b)
	key := func(s string, w [2]int) string { return strings.TrimLeft(s[w[0]:w[1]], " ") }
	lcs := make([][]int, len(wa)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(wb)+1)
	}
	for i := len(wa) - 1; i >= 0; i-- {
		for j := len(wb) - 1; j >= 0; j-- {
			if key(a, wa[i]) == key(b, wb[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	if shorter := min(len(wa), len(wb)); shorter == 0 || 2*lcs[0][0] < shorter {
		return nil, nil
	}
	i, j := 0, 0
	for i < len(wa) || j < len(wb) {
		switch {
		case i < len(wa) && j < len(wb) && key(a, wa[i]) == key(b, wb[j]):
			i, j = i+1, j+1
		case j < len(wb) && (i == len(wa) || lcs[i][j+1] > lcs[i+1][j]):
			cb = grow(cb, wb[j])
			j++
		default:
			ca = grow(ca, wa[i])
			i++
		}
	}
	return ca, cb
}

// grow adds span w to spans, merging it into the last one when they touch.
func grow(spans [][2]int, w [2]int) [][2]int {
	if n := len(spans); n > 0 && spans[n-1][1] == w[0] {
		spans[n-1][1] = w[1]
		return spans
	}
	return append(spans, w)
}

// renderDiff writes lines as a unified diff body. With color, a removed line
// is red and an added one green; in a rewritten pair, what the two share is
// faint and the span that changed is bold, so the eye lands on the edit
// rather than on the line around it. Without color the -/+ markers carry it.
func renderDiff(lines []diffLine, color bool) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteByte(l.op)
		b.WriteByte(' ')
		code := map[byte]string{'-': pal.Fail, '+': pal.OK}[l.op]
		switch {
		case code == "" || !color:
			b.WriteString(l.text)
		case len(l.changed) > 0:
			at := 0
			for _, c := range l.changed {
				if c[0] > at {
					b.WriteString(sgr(l.text[at:c[0]], code+";2", true))
				}
				b.WriteString(sgr(l.text[c[0]:c[1]], code+";1", true))
				at = c[1]
			}
			if at < len(l.text) {
				b.WriteString(sgr(l.text[at:], code+";2", true))
			}
		default:
			b.WriteString(sgr(l.text, code, true))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
