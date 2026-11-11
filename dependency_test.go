package migrate

import (
	"errors"
	"strings"

	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"
)

type DependencySuite struct{}

var _ = Suite(&DependencySuite{})

func dependencyErrorFrom(c *C, err error) *DependencyError {
	c.Assert(err, NotNil)
	var dependencyError *DependencyError
	c.Assert(errors.As(err, &dependencyError), Equals, true)
	return dependencyError
}

func topologicalIds(graph *migrationGraph) []string {
	ids := make([]string, len(graph.topo))
	for index, migration := range graph.topo {
		ids[index] = migration.Id
	}
	return ids
}

func migrationIds(migrations []*Migration) []string {
	ids := make([]string, len(migrations))
	for index, migration := range migrations {
		ids[index] = migration.Id
	}
	return ids
}

func (*DependencySuite) TestLoadGraphRejectsDuplicateId(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "1_a"},
		{Id: "2_b"},
	}

	_, err := loadMigrationGraph(migrations)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyDuplicate)
	c.Assert(dependencyError.MigrationId, Equals, "1_a")
	c.Assert(err.Error(), Equals, `duplicate migration id: "1_a"`)
}

func (*DependencySuite) TestLoadGraphRejectsMissingDependency(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"9_missing"}},
	}

	_, err := loadMigrationGraph(migrations)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyMissing)
	c.Assert(dependencyError.MigrationId, Equals, "2_b")
	c.Assert(dependencyError.DependencyId, Equals, "9_missing")
	c.Assert(dependencyError.Path, DeepEquals, []string{"2_b", "9_missing"})
	c.Assert(strings.Contains(err.Error(), "2_b"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "9_missing"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "2_b -> 9_missing"), Equals, true)
}

func (*DependencySuite) TestLoadGraphRejectsSelfDependency(c *C) {
	migrations := []*Migration{
		{Id: "1_a", Depends: []string{"1_a"}},
	}

	_, err := loadMigrationGraph(migrations)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencySelf)
	c.Assert(dependencyError.MigrationId, Equals, "1_a")
	c.Assert(dependencyError.Path, DeepEquals, []string{"1_a", "1_a"})
	c.Assert(strings.Contains(err.Error(), "1_a -> 1_a"), Equals, true)
}

func (*DependencySuite) TestLoadGraphRejectsMultiNodeCycle(c *C) {
	migrations := []*Migration{
		{Id: "1_a", Depends: []string{"3_c"}},
		{Id: "2_b", Depends: []string{"1_a"}},
		{Id: "3_c", Depends: []string{"2_b"}},
	}

	_, err := loadMigrationGraph(migrations)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyCycle)
	c.Assert(dependencyError.Path, DeepEquals, []string{"1_a", "3_c", "2_b", "1_a"})
	c.Assert(err.Error(), Equals, "cyclic migration dependency detected: 1_a -> 3_c -> 2_b -> 1_a")
}

func (*DependencySuite) TestTopologicalOrderWithoutEdgesEqualsLegacyOrder(c *C) {
	migrations := []*Migration{
		{Id: "10_x"},
		{Id: "1_a"},
		{Id: "2_b"},
		{Id: "efg"},
	}

	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)
	c.Assert(graph.hasEdges, Equals, false)
	c.Assert(topologicalIds(graph), DeepEquals, []string{"1_a", "2_b", "10_x", "efg"})
}

func (*DependencySuite) TestTopologicalOrderDoesNotRelyOnNumericPrefix(c *C) {
	// 2_b numerically precedes 10_x but depends on it: the edge, not the
	// numeric prefix, decides the order.
	migrations := []*Migration{
		{Id: "10_x"},
		{Id: "2_b", Depends: []string{"10_x"}},
	}

	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)
	c.Assert(topologicalIds(graph), DeepEquals, []string{"10_x", "2_b"})
}

func (*DependencySuite) TestStableTopologicalOrderTieBreak(c *C) {
	// Diamond: 2_b and 3_c are both ready after 1_a; the legacy Less ordering
	// breaks the tie deterministically.
	migrations := []*Migration{
		{Id: "4_d", Depends: []string{"2_b", "3_c"}},
		{Id: "2_b", Depends: []string{"1_a"}},
		{Id: "3_c", Depends: []string{"1_a"}},
		{Id: "1_a"},
	}

	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)
	c.Assert(topologicalIds(graph), DeepEquals, []string{"1_a", "2_b", "3_c", "4_d"})
}

func (*DependencySuite) TestUpClosure(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
		{Id: "3_c", Depends: []string{"1_a"}},
		{Id: "4_d", Depends: []string{"2_b", "3_c"}},
	}

	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)

	closure, err := graph.upClosure([]string{"4_d"})
	c.Assert(err, IsNil)
	c.Assert(closure, HasLen, 4)
	for _, id := range []string{"1_a", "2_b", "3_c", "4_d"} {
		_, ok := closure[id]
		c.Assert(ok, Equals, true, Commentf("closure must contain %s", id))
	}

	closure, err = graph.upClosure([]string{"3_c"})
	c.Assert(err, IsNil)
	c.Assert(closure, DeepEquals, map[string]struct{}{"1_a": {}, "3_c": {}})

	_, err = graph.upClosure([]string{"9_missing"})
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyTarget)
	c.Assert(dependencyError.MigrationId, Equals, "9_missing")
}

func (*DependencySuite) TestDuplicateDependencyDeclarationsDeduplicated(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a", "1_a"}},
	}

	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)
	c.Assert(graph.depends["2_b"], DeepEquals, []string{"1_a"})
	c.Assert(graph.dependents["1_a"], DeepEquals, []string{"2_b"})
}

func (*DependencySuite) TestGraphNodeAndEdgeBudgets(c *C) {
	originalNodes, originalEdges := MaxMigrationNodes, MaxMigrationEdges
	defer func() {
		MaxMigrationNodes, MaxMigrationEdges = originalNodes, originalEdges
	}()

	MaxMigrationNodes = 2
	_, err := loadMigrationGraph([]*Migration{{Id: "1_a"}, {Id: "2_b"}, {Id: "3_c"}})
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyBudget)
	c.Assert(strings.Contains(err.Error(), "exceeds limit 2"), Equals, true)

	MaxMigrationNodes = DefaultMaxMigrationNodes
	MaxMigrationEdges = 1
	_, err = loadMigrationGraph([]*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
		{Id: "3_c", Depends: []string{"1_a", "2_b"}},
	})
	dependencyError = dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyBudget)
	c.Assert(strings.Contains(err.Error(), "dependencies 2 exceeds limit 1"), Equals, true)
}

func (*DependencySuite) TestLoadDoesNotMutateSourceSlices(c *C) {
	original := []*Migration{
		{Id: "3_c", Depends: []string{"1_a"}},
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
	}
	source := &MemoryMigrationSource{Migrations: original}

	returned, err := source.FindMigrations()
	c.Assert(err, IsNil)
	c.Assert(migrationIds(original), DeepEquals, []string{"3_c", "1_a", "2_b"})
	c.Assert(migrationIds(returned), DeepEquals, []string{"1_a", "2_b", "3_c"})

	graph, err := loadMigrationGraph(returned)
	c.Assert(err, IsNil)

	// Mutate the caller's metadata after loading; the frozen graph is isolated.
	original[0].Depends[0] = "mutated"
	c.Assert(graph.depends["3_c"], DeepEquals, []string{"1_a"})

	// Feeding an unsorted slice directly must leave that slice untouched.
	unsorted := []*Migration{
		{Id: "3_c", Depends: []string{"1_a"}},
		{Id: "1_a"},
	}
	_, err = loadMigrationGraph(unsorted)
	c.Assert(err, IsNil)
	c.Assert(migrationIds(unsorted), DeepEquals, []string{"3_c", "1_a"})
}

func (*DependencySuite) TestUpSequenceFillsHolesDependencyFirst(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
		{Id: "3_c", Depends: []string{"2_b"}},
		{Id: "4_d", Depends: []string{"3_c"}},
	}

	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)

	// The highest numeric id 3_c is applied while its prerequisites are not.
	applied := map[string]struct{}{"3_c": {}}
	sequence := graph.upSequence(applied)
	c.Assert(migrationIds(sequence), DeepEquals, []string{"1_a", "2_b", "4_d"})

	c.Assert(checkUpExecutable(graph, sequence, applied), IsNil)
}

func (*DependencySuite) TestUpExecutableGuardRejectsUnmetDependency(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
	}
	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)

	// A deliberately illegal ordering must be rejected.
	illegal := []*Migration{graph.nodes["2_b"], graph.nodes["1_a"]}
	err = checkUpExecutable(graph, illegal, map[string]struct{}{})
	c.Assert(err, FitsTypeOf, &PlanError{})
	c.Assert(strings.Contains(err.Error(), "1_a"), Equals, true)
}

func (*DependencySuite) TestDownRemovalValidation(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
		{Id: "3_c", Depends: []string{"2_b"}},
	}
	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)
	applied := map[string]struct{}{"1_a": {}, "2_b": {}, "3_c": {}}

	// Removing 2_b while 3_c stays applied must be rejected, and 3_c must not
	// be rolled back implicitly.
	err = graph.validateDownRemoval(map[string]struct{}{"2_b": {}}, applied)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyBlocked)
	c.Assert(dependencyError.MigrationId, Equals, "2_b")
	c.Assert(dependencyError.DependencyId, Equals, "3_c")
	c.Assert(dependencyError.Path, DeepEquals, []string{"3_c", "2_b"})

	// Selecting the dependent as well makes the removal legal and orders it
	// dependents-first.
	removal := map[string]struct{}{"2_b": {}, "3_c": {}}
	c.Assert(graph.validateDownRemoval(removal, applied), IsNil)
	c.Assert(migrationIds(graph.downSequence(removal)), DeepEquals, []string{"3_c", "2_b"})

	// A leaf can always be removed on its own.
	c.Assert(graph.validateDownRemoval(map[string]struct{}{"3_c": {}}, applied), IsNil)
}

func (*DependencySuite) TestDownExecutableGuardRejectsRemainingDependent(c *C) {
	migrations := []*Migration{
		{Id: "1_a"},
		{Id: "2_b", Depends: []string{"1_a"}},
	}
	graph, err := loadMigrationGraph(migrations)
	c.Assert(err, IsNil)
	applied := map[string]struct{}{"1_a": {}, "2_b": {}}

	// Trying to remove 1_a while 2_b is still applied is illegal.
	illegal := []*Migration{graph.nodes["1_a"]}
	err = checkDownExecutable(graph, illegal, applied)
	c.Assert(err, FitsTypeOf, &PlanError{})
	c.Assert(strings.Contains(err.Error(), "2_b"), Equals, true)

	// Dependents-first order is legal.
	legal := []*Migration{graph.nodes["2_b"], graph.nodes["1_a"]}
	c.Assert(checkDownExecutable(graph, legal, applied), IsNil)
}
