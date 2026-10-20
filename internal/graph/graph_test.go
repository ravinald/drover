package graph

import (
	"reflect"
	"testing"
)

// A chain: 1 <- 2 <- 3, plus 4 depending on 2, and an isolated leaf.
func chain() *Graph {
	return New(map[string][]string{
		"1":    nil,
		"2":    {"1"},
		"3":    {"2"},
		"4":    {"2"},
		"leaf": nil,
	})
}

func TestBlastRadiusIsTransitive(t *testing.T) {
	g := chain()
	cases := map[string]int{"1": 3, "2": 2, "3": 0, "4": 0, "leaf": 0}
	for id, want := range cases {
		if got := g.BlastRadius(id); got != want {
			t.Errorf("BlastRadius(%s) = %d, want %d", id, got, want)
		}
	}
}

func TestDependentsAreSorted(t *testing.T) {
	if got, want := chain().Dependents("1"), []string{"2", "3", "4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Direct stays separate from the transitive walk because inheritance is
// deliberately one hop.
func TestDirectIsOneHop(t *testing.T) {
	if got, want := chain().Direct("3"), []string{"2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A cycle must terminate the walk rather than spin, and be reportable.
func TestCycleTerminatesAndIsFound(t *testing.T) {
	g := New(map[string][]string{
		"a": {"c"},
		"b": {"a"},
		"c": {"b"},
		"d": {"a"},
	})
	if got := g.BlastRadius("a"); got != 3 {
		t.Errorf("BlastRadius through a cycle = %d, want 3", got)
	}
	cycles := g.Cycles()
	if len(cycles) == 0 {
		t.Fatal("a dependency cycle must be reported")
	}
	if len(cycles[0]) != 3 {
		t.Errorf("cycle = %v, want 3 members", cycles[0])
	}
}

func TestNoCyclesInAnAcyclicGraph(t *testing.T) {
	if c := chain().Cycles(); len(c) != 0 {
		t.Errorf("found cycles in an acyclic graph: %v", c)
	}
}

// A dep naming an item that does not exist must not invent a node.
func TestUnknownDependencyDoesNotBecomeANode(t *testing.T) {
	g := New(map[string][]string{"1": {"ghost"}})
	if got := g.BlastRadius("ghost"); got != 1 {
		t.Errorf("BlastRadius(ghost) = %d, want 1", got)
	}
	if c := g.Cycles(); len(c) != 0 {
		t.Errorf("a dangling edge must not read as a cycle: %v", c)
	}
}
