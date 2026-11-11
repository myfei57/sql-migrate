package migrate

import (
	"container/heap"
	"fmt"
	"sort"
	"strings"
)

// Graph and dependency-closure budgets. They protect planning from unbounded
// memory use when a misbehaving source produces huge migration sets. The
// limits can be tuned through the package variables, but must stay positive.
const (
	DefaultMaxMigrationNodes = 10000
	DefaultMaxMigrationEdges = 50000
)

// MaxMigrationNodes bounds the number of migrations accepted into a single
// dependency graph.
var MaxMigrationNodes = DefaultMaxMigrationNodes

// MaxMigrationEdges bounds the total number of dependency edges accepted into
// a single dependency graph.
var MaxMigrationEdges = DefaultMaxMigrationEdges

// DependencyErrorReason explains why a migration dependency graph or a
// dependency-aware plan could not be built.
type DependencyErrorReason string

const (
	// DependencyDuplicate is reported when two migrations share the same id.
	DependencyDuplicate DependencyErrorReason = "duplicate migration id"
	// DependencyMissing is reported when a migration depends on an id that is
	// not part of the loaded source.
	DependencyMissing DependencyErrorReason = "missing migration dependency"
	// DependencySelf is reported when a migration declares itself as a
	// dependency.
	DependencySelf DependencyErrorReason = "self referencing migration dependency"
	// DependencyCycle is reported when dependency edges form a cycle.
	DependencyCycle DependencyErrorReason = "cyclic migration dependency detected"
	// DependencyBudget is reported when the node or edge budget is exceeded.
	DependencyBudget DependencyErrorReason = "migration graph budget exceeded"
	// DependencyTarget is reported when a requested target migration id is
	// unknown to the loaded source.
	DependencyTarget DependencyErrorReason = "unknown target migration"
	// DependencyNotApplied is reported when a down target was never applied.
	DependencyNotApplied DependencyErrorReason = "migration is not applied"
	// DependencyBlocked is reported when a down target is still required by
	// another applied migration that is not part of the removal set.
	DependencyBlocked DependencyErrorReason = "migration cannot be rolled back"
)

// DependencyError carries the offending migration ids and the actual
// dependency path involved in a failed validation. Path is nil when no path
// is relevant.
type DependencyError struct {
	Reason       DependencyErrorReason
	MigrationId  string
	DependencyId string
	Path         []string
	Detail       string
}

func (e *DependencyError) Error() string {
	switch e.Reason {
	case DependencyDuplicate:
		return fmt.Sprintf("%s: %q", e.Reason, e.MigrationId)
	case DependencyMissing:
		message := fmt.Sprintf("%s: migration %q depends on unknown migration %q",
			e.Reason, e.MigrationId, e.DependencyId)
		if len(e.Path) > 0 {
			message += " (dependency path: " + joinDependencyPath(e.Path) + ")"
		}
		return message
	case DependencySelf:
		return fmt.Sprintf("%s: migration %q depends on itself (dependency path: %s)",
			e.Reason, e.MigrationId, joinDependencyPath(e.Path))
	case DependencyCycle:
		return fmt.Sprintf("%s: %s", e.Reason, joinDependencyPath(e.Path))
	case DependencyBudget:
		return fmt.Sprintf("%s: %s", e.Reason, e.Detail)
	case DependencyTarget:
		return fmt.Sprintf("%s: %q", e.Reason, e.MigrationId)
	case DependencyNotApplied:
		return fmt.Sprintf("%s: migration %q cannot be rolled back because it is not applied",
			e.Reason, e.MigrationId)
	case DependencyBlocked:
		return fmt.Sprintf("%s: migration %q is still required by applied migration %q (dependency path: %s)",
			e.Reason, e.MigrationId, e.DependencyId, joinDependencyPath(e.Path))
	default:
		return string(e.Reason)
	}
}

func joinDependencyPath(path []string) string {
	return strings.Join(path, " -> ")
}

// migrationGraph is the frozen, validated dependency metadata used by the
// planner. It is built once per planning call and never mutated afterwards.
// Planning reads this graph only; it never inspects migration SQL.
type migrationGraph struct {
	nodes      map[string]*Migration
	sorted     []*Migration
	depends    map[string][]string
	dependents map[string][]string
	topo       []*Migration
	hasEdges   bool
}

// loadMigrationGraph validates the loaded migrations and freezes them into a
// dependency graph. The input slice and the Depends slices of the migrations
// are never modified.
func loadMigrationGraph(migrations []*Migration) (*migrationGraph, error) {
	// Work on a sorted copy so the source slice and its ordering stay intact.
	sorted := make([]*Migration, len(migrations))
	copy(sorted, migrations)
	sort.Sort(byId(sorted))

	if len(sorted) > MaxMigrationNodes {
		return nil, &DependencyError{
			Reason: DependencyBudget,
			Detail: fmt.Sprintf("number of migrations %d exceeds limit %d", len(sorted), MaxMigrationNodes),
		}
	}

	graph := &migrationGraph{
		nodes:      make(map[string]*Migration, len(sorted)),
		sorted:     sorted,
		depends:    make(map[string][]string, len(sorted)),
		dependents: make(map[string][]string, len(sorted)),
	}

	for _, migration := range sorted {
		if _, exists := graph.nodes[migration.Id]; exists {
			return nil, &DependencyError{Reason: DependencyDuplicate, MigrationId: migration.Id}
		}
		graph.nodes[migration.Id] = migration
	}

	edgeCount := 0
	for _, migration := range sorted {
		seen := make(map[string]struct{}, len(migration.Depends))
		dependencies := make([]string, 0, len(migration.Depends))
		for _, dependency := range migration.Depends {
			if _, duplicated := seen[dependency]; duplicated {
				continue
			}
			seen[dependency] = struct{}{}

			if dependency == migration.Id {
				return nil, &DependencyError{
					Reason:      DependencySelf,
					MigrationId: migration.Id,
					Path:        []string{migration.Id, migration.Id},
				}
			}
			if _, exists := graph.nodes[dependency]; !exists {
				return nil, &DependencyError{
					Reason:       DependencyMissing,
					MigrationId:  migration.Id,
					DependencyId: dependency,
					Path:         []string{migration.Id, dependency},
				}
			}

			dependencies = append(dependencies, dependency)
			edgeCount++
			if edgeCount > MaxMigrationEdges {
				return nil, &DependencyError{
					Reason: DependencyBudget,
					Detail: fmt.Sprintf("number of migration dependencies %d exceeds limit %d",
						edgeCount, MaxMigrationEdges),
				}
			}
		}
		// Keep a private copy so callers can never mutate frozen metadata.
		graph.depends[migration.Id] = dependencies
		if len(dependencies) > 0 {
			graph.hasEdges = true
		}
	}

	// Iterating the legacy sorted order keeps dependent lists deterministic.
	for _, migration := range sorted {
		for _, dependency := range graph.depends[migration.Id] {
			graph.dependents[dependency] = append(graph.dependents[dependency], migration.Id)
		}
	}

	if cycle := graph.detectCycle(); cycle != nil {
		return nil, &DependencyError{Reason: DependencyCycle, Path: cycle}
	}

	graph.topo = graph.stableTopologicalOrder()
	return graph, nil
}

// detectCycle runs a deterministic depth first search and returns the actual
// cycle path, for example ["a", "b", "c", "a"]. It returns nil for acyclic
// graphs.
func (g *migrationGraph) detectCycle() []string {
	const (
		white = iota
		gray
		black
	)

	color := make(map[string]int, len(g.nodes))
	stack := make([]string, 0, len(g.nodes))

	var visit func(id string) []string
	visit = func(id string) []string {
		color[id] = gray
		stack = append(stack, id)

		for _, dependency := range g.depends[id] {
			switch color[dependency] {
			case white:
				if cycle := visit(dependency); cycle != nil {
					return cycle
				}
			case gray:
				start := 0
				for stack[start] != dependency {
					start++
				}
				cycle := make([]string, 0, len(stack)-start+1)
				cycle = append(cycle, stack[start:]...)
				cycle = append(cycle, dependency)
				return cycle
			}
		}

		stack = stack[:len(stack)-1]
		color[id] = black
		return nil
	}

	for _, migration := range g.sorted {
		if color[migration.Id] == white {
			if cycle := visit(migration.Id); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// rankHeap orders ready nodes by their position in the legacy Less ordering,
// which is the stable tie breaker for every valid topological ordering.
type rankItem struct {
	id   string
	rank int
}

type rankHeap []rankItem

func (h rankHeap) Len() int { return len(h) }

func (h rankHeap) Less(i, j int) bool { return h[i].rank < h[j].rank }

func (h rankHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *rankHeap) Push(x any) {
	*h = append(*h, x.(rankItem))
}

func (h *rankHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// stableTopologicalOrder performs Kahn's algorithm, always selecting the
// ready migration that comes first in the legacy Less ordering. Without any
// dependency edges the result is therefore identical to the legacy ordering.
func (g *migrationGraph) stableTopologicalOrder() []*Migration {
	rank := make(map[string]int, len(g.sorted))
	indegree := make(map[string]int, len(g.nodes))
	ready := &rankHeap{}
	heap.Init(ready)

	for index, migration := range g.sorted {
		rank[migration.Id] = index
		indegree[migration.Id] = len(g.depends[migration.Id])
		if indegree[migration.Id] == 0 {
			heap.Push(ready, rankItem{id: migration.Id, rank: index})
		}
	}

	ordered := make([]*Migration, 0, len(g.nodes))
	for ready.Len() > 0 {
		item := heap.Pop(ready).(rankItem)
		migration := g.nodes[item.id]
		ordered = append(ordered, migration)

		for _, dependent := range g.dependents[item.id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				heap.Push(ready, rankItem{id: dependent, rank: rank[dependent]})
			}
		}
	}

	return ordered
}

// upClosure returns the target migrations together with every transitive
// dependency they require. Target ids must exist in the graph.
func (g *migrationGraph) upClosure(targets []string) (map[string]struct{}, error) {
	closure := make(map[string]struct{})

	var visit func(id string)
	visit = func(id string) {
		if _, visited := closure[id]; visited {
			return
		}
		closure[id] = struct{}{}
		for _, dependency := range g.depends[id] {
			visit(dependency)
		}
	}

	for _, target := range targets {
		if _, exists := g.nodes[target]; !exists {
			return nil, &DependencyError{Reason: DependencyTarget, MigrationId: target}
		}
		visit(target)
	}

	return closure, nil
}

// filterTopological returns the nodes of the keep set in stable topological
// order. Filtering a topological order keeps every edge direction valid.
func (g *migrationGraph) filterTopological(keep map[string]struct{}) []*Migration {
	result := make([]*Migration, 0, len(keep))
	for _, migration := range g.topo {
		if _, ok := keep[migration.Id]; ok {
			result = append(result, migration)
		}
	}
	return result
}

// upSequence returns every unapplied migration in executable topological
// order. Missing prerequisites are always placed before the nodes requiring
// them, independently of any numeric id prefix, so applied-state holes can
// never hide an unmet dependency.
func (g *migrationGraph) upSequence(applied map[string]struct{}) []*Migration {
	pending := make(map[string]struct{}, len(g.nodes))
	for id := range g.nodes {
		if _, isApplied := applied[id]; !isApplied {
			pending[id] = struct{}{}
		}
	}
	return g.filterTopological(pending)
}

// downSequence returns every applied migration in reverse topological order,
// so dependents are always removed before their dependencies.
func (g *migrationGraph) downSequence(applied map[string]struct{}) []*Migration {
	ordered := g.filterTopological(applied)
	for left, right := 0, len(ordered)-1; left < right; left, right = left+1, right-1 {
		ordered[left], ordered[right] = ordered[right], ordered[left]
	}
	return ordered
}

// validateDownRemoval rejects a removal set while an applied migration that
// is not part of the set still depends on a removed node. Dependents are
// never rolled back implicitly.
func (g *migrationGraph) validateDownRemoval(removal, applied map[string]struct{}) error {
	for _, migration := range g.sorted {
		if _, selected := removal[migration.Id]; !selected {
			continue
		}
		for _, dependent := range g.dependents[migration.Id] {
			_, isApplied := applied[dependent]
			_, isRemoved := removal[dependent]
			if isApplied && !isRemoved {
				return &DependencyError{
					Reason:       DependencyBlocked,
					MigrationId:  migration.Id,
					DependencyId: dependent,
					Path:         []string{dependent, migration.Id},
				}
			}
		}
	}
	return nil
}

// checkUpExecutable walks a planned up sequence and proves that every
// dependency of every step is already applied before that step runs.
func checkUpExecutable(graph *migrationGraph, plan []*Migration, initial map[string]struct{}) error {
	applied := make(map[string]struct{}, len(initial)+len(plan))
	for id := range initial {
		applied[id] = struct{}{}
	}

	for _, migration := range plan {
		for _, dependency := range graph.depends[migration.Id] {
			if _, ok := applied[dependency]; !ok {
				return newPlanError(migration,
					fmt.Sprintf("migration %q cannot be applied: dependency %q is not applied",
						migration.Id, dependency))
			}
		}
		applied[migration.Id] = struct{}{}
	}
	return nil
}

// checkDownExecutable walks a planned down sequence and proves that no
// still-applied migration depends on the node being removed.
func checkDownExecutable(graph *migrationGraph, plan []*Migration, initial map[string]struct{}) error {
	applied := make(map[string]struct{}, len(initial))
	for id := range initial {
		applied[id] = struct{}{}
	}

	for _, migration := range plan {
		for _, dependent := range graph.dependents[migration.Id] {
			if _, ok := applied[dependent]; ok {
				return newPlanError(migration,
					fmt.Sprintf("migration %q cannot be rolled back: applied migration %q still depends on it",
						migration.Id, dependent))
			}
		}
		delete(applied, migration.Id)
	}
	return nil
}

// toPlannedMigrations attaches the direction specific SQL and transaction
// flags without changing either: the migration SQL itself, the
// transaction/notransaction flags or the migration record behavior.
func toPlannedMigrations(migrations []*Migration, direction MigrationDirection) []*PlannedMigration {
	planned := make([]*PlannedMigration, 0, len(migrations))
	for _, migration := range migrations {
		entry := &PlannedMigration{Migration: migration}
		switch direction {
		case Up:
			entry.Queries = migration.Up
			entry.DisableTransaction = migration.DisableTransactionUp
		case Down:
			entry.Queries = migration.Down
			entry.DisableTransaction = migration.DisableTransactionDown
		default:
			panic("Not possible")
		}
		planned = append(planned, entry)
	}
	return planned
}
