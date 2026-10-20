// Package graph answers how much rests on a given work item.
//
// The question it exists for: a completion record says an item merged, not that
// anyone inspected it. Ordering that backlog by blast radius is what separates
// a defect costing one PR from one costing twenty.
package graph

import "sort"

// Graph is the dependency relation. Edges point from an item to what it needs.
type Graph struct {
	deps       map[string][]string
	dependents map[string][]string
}

// New builds a graph from an id-to-dependencies mapping.
func New(deps map[string][]string) *Graph {
	g := &Graph{
		deps:       make(map[string][]string, len(deps)),
		dependents: make(map[string][]string, len(deps)),
	}
	for id, ds := range deps {
		g.deps[id] = append([]string(nil), ds...)
		for _, d := range ds {
			g.dependents[d] = append(g.dependents[d], id)
		}
	}
	return g
}

// BlastRadius counts the items that transitively depend on id.
//
// A breadth-first walk of the reverse edges. The root seeds the visited set so
// a dependency cycle terminates rather than spinning, and is subtracted back out.
func (g *Graph) BlastRadius(id string) int {
	return len(g.Dependents(id))
}

// Dependents returns every item that transitively depends on id, sorted.
func (g *Graph) Dependents(id string) []string {
	seen := map[string]bool{id: true}
	frontier := []string{id}
	var out []string
	for len(frontier) > 0 {
		var next []string
		for _, node := range frontier {
			for _, up := range g.dependents[node] {
				if seen[up] {
					continue
				}
				seen[up] = true
				out = append(out, up)
				next = append(next, up)
			}
		}
		frontier = next
	}
	sort.Strings(out)
	return out
}

// Direct returns the immediate dependencies of id.
//
// Kept distinct from the transitive walk because inheritance is deliberately one
// hop: a prompt carrying every ancestor's open gaps would bury the ones its
// author can actually act on.
func (g *Graph) Direct(id string) []string { return g.deps[id] }

// Cycles returns each dependency cycle found, as the ids involved.
//
// A cycle is a backlog error, not a runtime one: every item in it is permanently
// blocked on itself, and the status table would show them all blocked with no
// hint why.
func (g *Graph) Cycles() [][]string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var found [][]string

	ids := make([]string, 0, len(g.deps))
	for id := range g.deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var visit func(string)
	visit = func(id string) {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range g.deps[id] {
			switch color[d] {
			case white:
				if _, known := g.deps[d]; known {
					visit(d)
				}
			case grey:
				for i, s := range stack {
					if s == d {
						found = append(found, append([]string(nil), stack[i:]...))
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
	return found
}
