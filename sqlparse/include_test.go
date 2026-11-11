package sqlparse

import (
	"strings"

	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"
)

type IncludeParseSuite struct{}

var _ = Suite(&IncludeParseSuite{})

type fakeFragment struct {
	content string
	err     error
}

type fakeResolver struct {
	fragments map[string]fakeFragment
	calls     []string
}

func (r *fakeResolver) Resolve(ref string, _ SourceRef, _ []IncludeFrame) (Fragment, error) {
	r.calls = append(r.calls, ref)
	fragment, ok := r.fragments[ref]
	if !ok {
		return Fragment{}, &simpleError{message: "fragment " + ref + " not found"}
	}
	if fragment.err != nil {
		return Fragment{}, fragment.err
	}
	return Fragment{
		Path:    "frags/" + ref,
		Content: []byte(fragment.content),
		Digest:  "digest-of-" + ref,
	}, nil
}

type simpleError struct {
	message string
}

func (e *simpleError) Error() string { return e.message }

func parseWithIncludes(c *C, sql string, resolver FragmentResolver) *ParsedMigration {
	parsed, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, IsNil)
	return parsed
}

func (*IncludeParseSuite) TestNilResolver(c *C) {
	_, err := ParseMigrationWithIncludes(strings.NewReader("-- +migrate Up\nSELECT 1;\n"), IncludeConfig{})
	c.Assert(err, NotNil)
}

func (*IncludeParseSuite) TestBasicExpansionAndSources(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"create_people.sqlinc": {content: "CREATE TABLE people (id int);\nINSERT INTO people VALUES (1);\n"},
		"drop_people.sqlinc":   {content: "DROP TABLE people;\n"},
	}}
	sql := `-- +migrate Up
-- +migrate IncludeFragment create_people.sqlinc
CREATE TABLE extra (id int);

-- +migrate Down
-- +migrate IncludeFragment drop_people.sqlinc
`
	parsed := parseWithIncludes(c, sql, resolver)

	c.Assert(parsed.UpStatements, HasLen, 3)
	c.Assert(parsed.DownStatements, HasLen, 1)
	c.Assert(parsed.UpSources, HasLen, 3)
	c.Assert(parsed.DownSources, HasLen, 1)

	c.Assert(strings.TrimSpace(parsed.UpStatements[0]), Equals, "CREATE TABLE people (id int);")
	c.Assert(strings.TrimSpace(parsed.UpStatements[1]), Equals, "INSERT INTO people VALUES (1);")
	c.Assert(strings.TrimSpace(parsed.UpStatements[2]), Equals, "CREATE TABLE extra (id int);")

	first := parsed.UpSources[0]
	c.Assert(first.Ref.File, Equals, "frags/create_people.sqlinc")
	c.Assert(first.Ref.Line, Equals, 1)
	c.Assert(first.Chain, HasLen, 1)
	c.Assert(first.Chain[0].File, Equals, "1_root.sql")
	c.Assert(first.Chain[0].Line, Equals, 2)
	c.Assert(first.Chain[0].Ref, Equals, "create_people.sqlinc")
	c.Assert(first.Chain[0].Path, Equals, "frags/create_people.sqlinc")
	c.Assert(first.Chain[0].Digest, Equals, "digest-of-create_people.sqlinc")

	// Statements written directly in the root have an empty chain.
	c.Assert(parsed.UpSources[2].Chain, HasLen, 0)
	c.Assert(parsed.UpSources[2].Ref.File, Equals, "1_root.sql")
	c.Assert(parsed.UpSources[2].Ref.Line, Equals, 3)

	down := parsed.DownSources[0]
	c.Assert(down.Ref.File, Equals, "frags/drop_people.sqlinc")
	c.Assert(down.Chain, HasLen, 1)
	c.Assert(down.Chain[0].Line, Equals, 6)
}

func (*IncludeParseSuite) TestRepeatedIncludeProducesIndependentOccurrences(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"marker.sqlinc": {content: "INSERT INTO log VALUES ('x');\n"},
	}}
	sql := `-- +migrate Up
-- +migrate IncludeFragment marker.sqlinc
CREATE TABLE log (msg text);
-- +migrate IncludeFragment marker.sqlinc
-- +migrate IncludeFragment marker.sqlinc
`
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(parsed.UpStatements, HasLen, 4)
	c.Assert(strings.TrimSpace(parsed.UpStatements[0]), Equals, "INSERT INTO log VALUES ('x');")
	c.Assert(strings.TrimSpace(parsed.UpStatements[1]), Equals, "CREATE TABLE log (msg text);")
	c.Assert(strings.TrimSpace(parsed.UpStatements[2]), Equals, "INSERT INTO log VALUES ('x');")
	c.Assert(strings.TrimSpace(parsed.UpStatements[3]), Equals, "INSERT INTO log VALUES ('x');")

	// Every occurrence carries its own chain slice even though the content is
	// identical, and the include sites point at different root lines.
	c.Assert(parsed.UpSources[0].Chain[0].Line, Equals, 2)
	c.Assert(parsed.UpSources[2].Chain[0].Line, Equals, 4)
	c.Assert(parsed.UpSources[3].Chain[0].Line, Equals, 5)
	c.Assert(resolver.calls, HasLen, 3)
}

func (*IncludeParseSuite) TestDiamondInclude(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"a.sqlinc": {content: "-- +migrate IncludeFragment d.sqlinc\nINSERT INTO log VALUES ('a');\n"},
		"b.sqlinc": {content: "-- +migrate IncludeFragment d.sqlinc\nINSERT INTO log VALUES ('b');\n"},
		"d.sqlinc": {content: "INSERT INTO log VALUES ('d');\n"},
	}}
	sql := `-- +migrate Up
-- +migrate IncludeFragment a.sqlinc
-- +migrate IncludeFragment b.sqlinc
`
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(parsed.UpStatements, HasLen, 4)
	expected := []string{"d", "a", "d", "b"}
	for i, marker := range expected {
		c.Assert(strings.TrimSpace(parsed.UpStatements[i]), Equals, "INSERT INTO log VALUES ('"+marker+"');")
	}
	// d is reachable through two branches and is expanded twice.
	c.Assert(parsed.UpSources[0].Chain, HasLen, 2)
	c.Assert(parsed.UpSources[0].Chain[0].Path, Equals, "frags/a.sqlinc")
	c.Assert(parsed.UpSources[0].Chain[1].Path, Equals, "frags/d.sqlinc")
	c.Assert(parsed.UpSources[2].Chain[1].Path, Equals, "frags/d.sqlinc")
}

func (*IncludeParseSuite) TestHiddenDirectivesAreNotExpanded(c *C) {
	// The referenced fragment deliberately does not exist: any expansion
	// would fail, so successful parsing proves the annotations are ignored.
	sql := `-- +migrate Up
SELECT 'single
-- +migrate IncludeFragment missing.sqlinc
quote';
SELECT $body$
-- +migrate IncludeFragment missing.sqlinc
$body$;
/* outer /* nested
-- +migrate IncludeFragment missing.sqlinc
*/ still nested */
SELECT 1;
-- +migrate StatementBegin
CREATE TRIGGER trg AFTER INSERT ON t
BEGIN
  -- +migrate IncludeFragment missing.sqlinc
  INSERT INTO log VALUES ('trigger');
END;
-- +migrate StatementEnd
`
	resolver := &fakeResolver{fragments: map[string]fakeFragment{}}
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(resolver.calls, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 4)

	// Directive-looking text inside the string and block comment is preserved.
	c.Assert(strings.Contains(parsed.UpStatements[0], "IncludeFragment missing.sqlinc"), Equals, true)
	c.Assert(strings.Contains(parsed.UpStatements[1], "IncludeFragment missing.sqlinc"), Equals, true)
	c.Assert(strings.Contains(parsed.UpStatements[2], "IncludeFragment missing.sqlinc"), Equals, true)
	// Inside StatementBegin/End the directive is not expanded: the indented
	// directive line is kept as ordinary procedure body text, exactly like
	// the legacy line-based parser.
	c.Assert(strings.Contains(parsed.UpStatements[3], "IncludeFragment missing.sqlinc"), Equals, true)
	c.Assert(strings.Contains(parsed.UpStatements[3], "INSERT INTO log"), Equals, true)
}

func (*IncludeParseSuite) TestSemicolonInsideStringDoesNotSplit(c *C) {
	sql := `-- +migrate Up
INSERT INTO t VALUES ('a;b;c');
INSERT INTO u VALUES ('d');
`
	resolver := &fakeResolver{fragments: map[string]fakeFragment{}}
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(parsed.UpStatements, HasLen, 2)
}

func (*IncludeParseSuite) TestFragmentMustNotContainDirection(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"bad.sqlinc": {content: "-- +migrate Down\nDROP TABLE x;\n"},
	}}
	sql := "-- +migrate Up\n-- +migrate IncludeFragment bad.sqlinc\n"
	_, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "fragments must not contain Up or Down"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "frags/bad.sqlinc:1"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "1_root.sql:2"), Equals, true)
}

func (*IncludeParseSuite) TestIncludeRequiresStatementBoundary(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"x.sqlinc": {content: "SELECT 1;\n"},
	}}
	sql := `-- +migrate Up
CREATE TABLE t (
  id int
-- +migrate IncludeFragment x.sqlinc
);
`
	_, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "statement boundary"), Equals, true)
	c.Assert(resolver.calls, HasLen, 0)
}

func (*IncludeParseSuite) TestIncludeBeforeDirectionRejected(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"x.sqlinc": {content: "SELECT 1;\n"},
	}}
	sql := "-- +migrate IncludeFragment x.sqlinc\n-- +migrate Up\nSELECT 1;\n"
	_, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "Up or Down section"), Equals, true)
	c.Assert(resolver.calls, HasLen, 0)
}

func (*IncludeParseSuite) TestUnbalancedStatementBeginInFragment(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"proc.sqlinc": {content: "-- +migrate StatementBegin\nCREATE FUNCTION f() RETURNS void AS $$ BEGIN END; $$ LANGUAGE plpgsql;\n"},
	}}
	sql := "-- +migrate Up\n-- +migrate IncludeFragment proc.sqlinc\n"
	_, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "StatementEnd"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "frags/proc.sqlinc"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "1_root.sql:2"), Equals, true)
}

func (*IncludeParseSuite) TestMissingFragmentReportsSiteAndChain(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"outer.sqlinc": {content: "-- +migrate IncludeFragment inner.sqlinc\nSELECT 1;\n"},
	}}
	sql := "-- +migrate Up\n-- +migrate IncludeFragment outer.sqlinc\n"
	_, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "frags/outer.sqlinc:1"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "1_root.sql:2"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "inner.sqlinc"), Equals, true)
}

func (*IncludeParseSuite) TestBadFragmentArgumentCount(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{}}
	sql := "-- +migrate Up\n-- +migrate IncludeFragment a.sqlinc b.sqlinc\nSELECT 1;\n"
	_, err := ParseMigrationWithIncludes(strings.NewReader(sql), IncludeConfig{
		Resolver: resolver,
		File:     "1_root.sql",
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "exactly one fragment path"), Equals, true)
}

func (*IncludeParseSuite) TestLegacyParserIgnoresIncludeAnnotation(c *C) {
	// The legacy parser must not touch the resolver and simply skip unknown
	// directive lines, keeping historical behavior byte-compatible.
	sql := `-- +migrate Up
-- +migrate IncludeFragment whatever.sqlinc
CREATE TABLE t (id int);
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.UpStatements, HasLen, 1)
	c.Assert(parsed.UpSources, IsNil)
	c.Assert(strings.TrimSpace(parsed.UpStatements[0]), Equals, "CREATE TABLE t (id int);")
}

func (*IncludeParseSuite) TestNoTransactionFlagStillOwnedByRoot(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"x.sqlinc": {content: "SELECT 1;\n"},
	}}
	sql := "-- +migrate Up notransaction\n-- +migrate IncludeFragment x.sqlinc\n"
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(parsed.DisableTransactionUp, Equals, true)
	c.Assert(parsed.UpStatements, HasLen, 1)
}

func (*IncludeParseSuite) TestEmptyFragment(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"empty.sqlinc": {content: "-- only a comment\n"},
	}}
	sql := "-- +migrate Up\n-- +migrate IncludeFragment empty.sqlinc\nSELECT 1;\n"
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(parsed.UpStatements, HasLen, 1)
	c.Assert(strings.TrimSpace(parsed.UpStatements[0]), Equals, "SELECT 1;")
}

func (*IncludeParseSuite) TestColumnOneDirectiveInProcedureBodyDropped(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{}}
	sql := `-- +migrate Up
-- +migrate StatementBegin
CREATE TRIGGER trg AFTER INSERT ON t
BEGIN
-- +migrate IncludeFragment missing.sqlinc
  INSERT INTO log VALUES ('trigger');
END;
-- +migrate StatementEnd
`
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(resolver.calls, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 1)
	c.Assert(strings.Contains(parsed.UpStatements[0], "IncludeFragment"), Equals, false)
}

func (*IncludeParseSuite) TestFragmentWithStatementBlock(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{
		"proc.sqlinc": {content: `-- +migrate StatementBegin
CREATE TRIGGER trg AFTER INSERT ON t
BEGIN
  INSERT INTO log VALUES ('a');
  INSERT INTO log VALUES ('b');
END;
-- +migrate StatementEnd
`},
	}}
	sql := `-- +migrate Up
CREATE TABLE t (id int);
-- +migrate IncludeFragment proc.sqlinc
CREATE TABLE u (id int);
`
	parsed := parseWithIncludes(c, sql, resolver)
	// Semicolons inside the fragment procedure body must not split it:
	// root statement, one procedure statement, root statement.
	c.Assert(parsed.UpStatements, HasLen, 3)
	c.Assert(strings.Contains(parsed.UpStatements[1], "CREATE TRIGGER"), Equals, true)
	c.Assert(parsed.UpSources[1].Ref.File, Equals, "frags/proc.sqlinc")
	c.Assert(parsed.UpSources[1].Chain, HasLen, 1)
	// The fragment cannot change the outer direction: statements keep
	// landing in Up and no transaction flags are touched.
	c.Assert(parsed.DisableTransactionUp, Equals, false)
	c.Assert(parsed.DownStatements, HasLen, 0)
}

func (*IncludeParseSuite) TestNestedDollarQuoteTags(c *C) {
	resolver := &fakeResolver{fragments: map[string]fakeFragment{}}
	sql := `-- +migrate Up
-- +migrate StatementBegin
CREATE FUNCTION f() RETURNS text AS $outer$
BEGIN
  RETURN $inner$
-- +migrate IncludeFragment missing.sqlinc
$inner$;
END;
$outer$ LANGUAGE plpgsql;
-- +migrate StatementEnd
`
	parsed := parseWithIncludes(c, sql, resolver)
	c.Assert(resolver.calls, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 1)
	c.Assert(strings.Contains(parsed.UpStatements[0], "IncludeFragment missing.sqlinc"), Equals, true)
}
