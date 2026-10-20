package backlog

import (
	"fmt"
	"strconv"
	"strings"
)

// Shape is what one frontmatter key may hold. The key set is closed by
// [frontmatter]; a shape closes one key's values, so a rank nothing can read
// and an id nothing can resolve are named where they were written instead of
// degrading into a blank cell or a dep somebody forgot to write.
//
// Nothing here runs in ParseFrontmatter. A body that will not parse leaves the
// backlog entirely — status calls it broken, run refuses it, everything
// depending on it goes MISSING DEP — and a mistyped severity letter is not worth
// that. check is where a shape is enforced.
type Shape struct {
	kind    shapeKind
	members []string
	spec    string
}

type shapeKind int

const (
	shapeInt shapeKind = iota
	shapeEnum
	shapeIDList
	shapeIntList
)

// ShapeForms names the accepted specs, for the error a typo'd one earns.
func ShapeForms() []string { return []string{"int", "enum:A,B,C", "idlist", "intlist"} }

// ParseShape reads one declared spec.
func ParseShape(spec string) (Shape, error) {
	s := Shape{spec: spec}
	switch {
	case spec == "int":
		s.kind = shapeInt
	case spec == "idlist":
		s.kind = shapeIDList
	case spec == "intlist":
		s.kind = shapeIntList
	case strings.HasPrefix(spec, "enum:"):
		s.kind = shapeEnum
		for _, m := range strings.Split(strings.TrimPrefix(spec, "enum:"), ",") {
			m = strings.TrimSpace(m)
			if m == "" {
				return Shape{}, fmt.Errorf("shape %q has an empty member", spec)
			}
			s.members = append(s.members, m)
		}
	default:
		return Shape{}, fmt.Errorf("unknown shape %q (known: %s)", spec, strings.Join(ShapeForms(), ", "))
	}
	return s, nil
}

// Spec renders the shape as it was declared, so a finding can name what the
// value failed rather than describing it in different words.
func (s Shape) Spec() string { return s.spec }

// Check reads one value as written. prefixes are the project's namespace keys
// and are read only by idlist.
//
// An empty value always passes. [frontmatter] required already separates
// presence from content, and a shape that also demanded a value would move that
// line without anyone declaring it moved.
func (s Shape) Check(value string, prefixes []string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	switch s.kind {
	case shapeInt:
		if _, err := strconv.Atoi(v); err != nil {
			return fmt.Errorf("%q is not a whole number", value)
		}
	case shapeEnum:
		for _, m := range s.members {
			if v == m {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %s", value, strings.Join(s.members, ", "))
	case shapeIDList:
		// ParseID and not existence: whether the id names a body is MISSING DEP's
		// question, and answering it twice is how two classifiers disagree.
		for _, tok := range strings.Fields(v) {
			if _, err := ParseID(tok, prefixes); err != nil {
				return err
			}
		}
	case shapeIntList:
		for _, tok := range strings.Fields(v) {
			// Zero and negatives are rejected rather than carried: the one
			// consumer is a tracker issue number, and #0 names nothing anywhere.
			if n, err := strconv.Atoi(tok); err != nil || n <= 0 {
				return fmt.Errorf("%q is not a positive whole number", tok)
			}
		}
	}
	return nil
}

// Ints reads a whitespace-separated list of numbers, skipping anything that is
// not one. Check is where a bad token is reported; a reader that refused here
// would make one typo hide every number beside it.
func Ints(value string) []int {
	var out []int
	for _, tok := range strings.Fields(value) {
		if n, err := strconv.Atoi(tok); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}
