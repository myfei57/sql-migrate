package sqlparse

import (
	"strings"

	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"
)

type AssertParseSuite struct{}

var _ = Suite(&AssertParseSuite{})

const basicAssertionMigration = `-- +migrate Up
-- +migrate AssertBeforeBegin precheck
SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table');
-- +migrate AssertBeforeEnd

CREATE TABLE people (id int);

-- +migrate AssertAfterBegin postcheck
SELECT COUNT(*) = 0 FROM people;
-- +migrate AssertAfterEnd

-- +migrate Down
-- +migrate AssertBeforeBegin down_pre
SELECT 1;
-- +migrate AssertBeforeEnd
DROP TABLE people;
-- +migrate AssertAfterBegin down_post
SELECT 1;
-- +migrate AssertAfterEnd
`

func (*AssertParseSuite) TestAssertionParsing(c *C) {
	parsed, err := ParseMigration(strings.NewReader(basicAssertionMigration))
	c.Assert(err, IsNil)

	c.Assert(parsed.UpStatements, HasLen, 1)
	c.Assert(strings.Contains(parsed.UpStatements[0], "CREATE TABLE people"), Equals, true)
	c.Assert(parsed.DownStatements, HasLen, 1)

	c.Assert(parsed.AssertBeforeUp, HasLen, 1)
	c.Assert(parsed.AssertBeforeUp[0].Name, Equals, "precheck")
	c.Assert(strings.Contains(parsed.AssertBeforeUp[0].Statement, "sqlite_master"), Equals, true)
	c.Assert(parsed.AssertAfterUp, HasLen, 1)
	c.Assert(parsed.AssertAfterUp[0].Name, Equals, "postcheck")
	c.Assert(parsed.AssertAfterUp[0].Statement, Equals, "SELECT COUNT(*) = 0 FROM people;")

	c.Assert(parsed.AssertBeforeDown, HasLen, 1)
	c.Assert(parsed.AssertBeforeDown[0].Name, Equals, "down_pre")
	c.Assert(parsed.AssertAfterDown, HasLen, 1)
	c.Assert(parsed.AssertAfterDown[0].Name, Equals, "down_post")
}

func (*AssertParseSuite) TestAssertionBlockNotSplitOnSemicolons(c *C) {
	// The check SQL is captured verbatim: embedded semicolons, even inside
	// strings, must not split the assertion into several statements.
	sql := `-- +migrate Up
-- +migrate AssertBeforeBegin weird_semicolons
SELECT 'a;b' = 'a;b';
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.AssertBeforeUp, HasLen, 1)
	c.Assert(parsed.AssertBeforeUp[0].Statement, Equals, "SELECT 'a;b' = 'a;b';")
	c.Assert(parsed.UpStatements, HasLen, 1)
}

func (*AssertParseSuite) TestAssertionAccumulatesAcrossSections(c *C) {
	sql := `-- +migrate Up
-- +migrate AssertBeforeBegin first
SELECT 1;
-- +migrate AssertBeforeEnd
CREATE TABLE a (id int);

-- +migrate Down
DROP TABLE a;

-- +migrate Up
-- +migrate AssertAfterBegin second
SELECT 1;
-- +migrate AssertAfterEnd
CREATE TABLE b (id int);

-- +migrate Down
DROP TABLE b;
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.AssertBeforeUp, HasLen, 1)
	c.Assert(parsed.AssertBeforeUp[0].Name, Equals, "first")
	c.Assert(parsed.AssertAfterUp, HasLen, 1)
	c.Assert(parsed.AssertAfterUp[0].Name, Equals, "second")
	c.Assert(parsed.UpStatements, HasLen, 2)
}

func (*AssertParseSuite) TestAssertionNameCanRepeatAcrossDirections(c *C) {
	sql := `-- +migrate Up
-- +migrate AssertBeforeBegin shared_name
SELECT 1;
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);

-- +migrate Down
-- +migrate AssertBeforeBegin shared_name
SELECT 1;
-- +migrate AssertBeforeEnd
DROP TABLE t;
`
	_, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
}

func (*AssertParseSuite) TestAssertionRejectedMigrations(c *C) {
	bad := map[string]string{
		"duplicate assertion name": `-- +migrate Up
-- +migrate AssertBeforeBegin dup
SELECT 1;
-- +migrate AssertBeforeEnd
-- +migrate AssertAfterBegin dup
SELECT 1;
-- +migrate AssertAfterEnd
CREATE TABLE t (id int);
`,
		"unclosed before block": `-- +migrate Up
-- +migrate AssertBeforeBegin open
SELECT 1;
CREATE TABLE t (id int);
`,
		"unclosed after block": `-- +migrate Up
-- +migrate AssertAfterBegin open
SELECT 1;
CREATE TABLE t (id int);
`,
		"end without begin": `-- +migrate Up
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"mismatched end marker": `-- +migrate Up
-- +migrate AssertAfterBegin check
SELECT 1;
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"assert nested in statement block": `-- +migrate Up
-- +migrate StatementBegin
-- +migrate AssertBeforeBegin illegal
SELECT 1;
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
-- +migrate StatementEnd
`,
		"statement nested in assertion block": `-- +migrate Up
-- +migrate AssertBeforeBegin check
SELECT 1;
-- +migrate StatementBegin
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"nested assertion blocks": `-- +migrate Up
-- +migrate AssertBeforeBegin outer
SELECT 1;
-- +migrate AssertAfterBegin inner
SELECT 1;
-- +migrate AssertAfterEnd
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"assertion outside direction": `-- +migrate AssertBeforeBegin orphan
SELECT 1;
-- +migrate AssertBeforeEnd
-- +migrate Up
CREATE TABLE t (id int);
`,
		"missing assertion name": `-- +migrate Up
-- +migrate AssertBeforeBegin
SELECT 1;
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"too many assertion names": `-- +migrate Up
-- +migrate AssertBeforeBegin one two
SELECT 1;
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"empty assertion block": `-- +migrate Up
-- +migrate AssertBeforeBegin empty
-- +migrate AssertBeforeEnd
CREATE TABLE t (id int);
`,
		"direction switch inside assertion": `-- +migrate Up
-- +migrate AssertBeforeBegin check
SELECT 1;
-- +migrate Down
-- +migrate AssertBeforeEnd
DROP TABLE t;
`,
		"statement end without begin": `-- +migrate Up
-- +migrate AssertBeforeBegin check
SELECT 1;
-- +migrate AssertBeforeEnd
-- +migrate StatementEnd
CREATE TABLE t (id int);
`,
	}

	for label, sql := range bad {
		_, err := ParseMigration(strings.NewReader(sql))
		c.Assert(err, NotNil, Commentf("expected rejection for %s", label))
	}
}

func (*AssertParseSuite) TestPseudoAnnotationsInsideStringsAreIgnored(c *C) {
	// Directive-looking lines inside a multi-line string literal are SQL
	// content, not migration commands.
	sql := `-- +migrate Up
CREATE TABLE notes (id int, body text);
INSERT INTO notes (id, body) VALUES (1, '
-- +migrate AssertBeforeBegin fake
-- +migrate AssertAfterBegin fake_too
');
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.AssertBeforeUp, HasLen, 0)
	c.Assert(parsed.AssertAfterUp, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 2)
	c.Assert(strings.Contains(parsed.UpStatements[1], "AssertBeforeBegin fake"), Equals, true)

	// Doubled single quotes escape a quote without ending the string.
	sqlEscaped := `-- +migrate Up
INSERT INTO notes (id, body) VALUES (2, 'it''s
-- +migrate AssertBeforeBegin escaped
text');
`
	parsed, err = ParseMigration(strings.NewReader(sqlEscaped))
	c.Assert(err, IsNil)
	c.Assert(parsed.AssertBeforeUp, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 1)
}

func (*AssertParseSuite) TestPseudoAnnotationsInsideBlockCommentsAreIgnored(c *C) {
	sql := `-- +migrate Up
/*
-- +migrate AssertBeforeBegin fake
-- +migrate AssertBeforeEnd
*/
CREATE TABLE t (id int);
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.AssertBeforeUp, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 1)
}

func (*AssertParseSuite) TestPseudoAnnotationsInLineCommentsAreIgnored(c *C) {
	sql := `-- +migrate Up
-- -- +migrate AssertBeforeBegin fake
CREATE TABLE t (id int);
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.AssertBeforeUp, HasLen, 0)
	c.Assert(parsed.UpStatements, HasLen, 1)
}

func (*AssertParseSuite) TestStatementsAndAssertionsCoexist(c *C) {
	// StatementBegin/End blocks and assertion blocks may follow each other,
	// just not nest.
	sql := `-- +migrate Up
-- +migrate StatementBegin
CREATE TRIGGER trg AFTER INSERT ON t BEGIN
  SELECT 1;
END;
-- +migrate StatementEnd
-- +migrate AssertAfterBegin trigger_exists
SELECT 1;
-- +migrate AssertAfterEnd

-- +migrate Down
DROP TRIGGER trg;
`
	parsed, err := ParseMigration(strings.NewReader(sql))
	c.Assert(err, IsNil)
	c.Assert(parsed.UpStatements, HasLen, 1)
	c.Assert(parsed.AssertAfterUp, HasLen, 1)
	c.Assert(parsed.AssertAfterUp[0].Name, Equals, "trigger_exists")
}
