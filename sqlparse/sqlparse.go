package sqlparse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

const (
	sqlCmdPrefix        = "-- +migrate "
	optionNoTransaction = "notransaction"

	statementBeginCmd = "StatementBegin"
	statementEndCmd   = "StatementEnd"

	assertBeforeBeginCmd = "AssertBeforeBegin"
	assertBeforeEndCmd   = "AssertBeforeEnd"
	assertAfterBeginCmd  = "AssertAfterBegin"
	assertAfterEndCmd    = "AssertAfterEnd"
)

// Assertion is a single named check parsed from an AssertBefore/AssertAfter
// block. Statement holds the verbatim check SQL found inside the block.
type Assertion struct {
	Name      string
	Statement string
}

type ParsedMigration struct {
	UpStatements   []string
	DownStatements []string

	// AssertBefore and AssertAfter checks, per direction, kept in source order.
	AssertBeforeUp   []Assertion
	AssertAfterUp    []Assertion
	AssertBeforeDown []Assertion
	AssertAfterDown  []Assertion

	DisableTransactionUp   bool
	DisableTransactionDown bool
}

// LineSeparator can be used to split migrations by an exact line match. This line
// will be removed from the output. If left blank, it is not considered. It is defaulted
// to blank so you will have to set it manually.
// Use case: in MSSQL, it is convenient to separate commands by GO statements like in
// SQL Query Analyzer.
var LineSeparator = ""

func errNoTerminator() error {
	if len(LineSeparator) == 0 {
		return fmt.Errorf(`ERROR: The last statement must be ended by a semicolon or '-- +migrate StatementEnd' marker.
			See https://github.com/rubenv/sql-migrate for details.`)
	}

	return fmt.Errorf(`ERROR: The last statement must be ended by a semicolon, a line whose contents are %q, or '-- +migrate StatementEnd' marker.
			See https://github.com/rubenv/sql-migrate for details.`, LineSeparator)
}

// Checks the line to see if the line has a statement-ending semicolon
// or if the line contains a double-dash comment.
func endsWithSemicolon(line string) bool {
	prev := ""
	scanner := bufio.NewScanner(strings.NewReader(line))
	scanner.Split(bufio.ScanWords)

	for scanner.Scan() {
		word := scanner.Text()
		if strings.HasPrefix(word, "--") {
			break
		}
		prev = word
	}

	return strings.HasSuffix(prev, ";")
}

type migrationDirection int

const (
	directionNone migrationDirection = iota
	directionUp
	directionDown
)

func (d migrationDirection) label() string {
	switch d {
	case directionUp:
		return "Up"
	case directionDown:
		return "Down"
	default:
		return "none"
	}
}

// blockMode tracks the explicit block a migration line currently belongs to.
type blockMode int

const (
	blockNone blockMode = iota
	blockStatement
	blockAssertBefore
	blockAssertAfter
)

func (b blockMode) label() string {
	switch b {
	case blockStatement:
		return "StatementBegin/StatementEnd"
	case blockAssertBefore:
		return "AssertBeforeBegin/AssertBeforeEnd"
	case blockAssertAfter:
		return "AssertAfterBegin/AssertAfterEnd"
	default:
		return "none"
	}
}

type migrateCommand struct {
	Command string
	Options []string
}

func (c *migrateCommand) HasOption(opt string) bool {
	for _, specifiedOption := range c.Options {
		if specifiedOption == opt {
			return true
		}
	}

	return false
}

func parseCommand(line string) (*migrateCommand, error) {
	cmd := &migrateCommand{}

	if !strings.HasPrefix(line, sqlCmdPrefix) {
		return nil, fmt.Errorf("ERROR: not a sql-migrate command")
	}

	fields := strings.Fields(line[len(sqlCmdPrefix):])
	if len(fields) == 0 {
		return nil, fmt.Errorf(`ERROR: incomplete migration command`)
	}

	cmd.Command = fields[0]

	cmd.Options = fields[1:]

	return cmd, nil
}

func singleAssertionName(cmd *migrateCommand) (string, error) {
	if len(cmd.Options) != 1 {
		return "", fmt.Errorf("ERROR: '-- +migrate %s' requires exactly one assertion name, e.g. '-- +migrate %s my_check'",
			cmd.Command, cmd.Command)
	}

	name := cmd.Options[0]
	if name == "" {
		return "", fmt.Errorf("ERROR: '-- +migrate %s' requires an assertion name", cmd.Command)
	}

	return name, nil
}

func errNestedBlock(cmd string, outer blockMode) error {
	return fmt.Errorf("ERROR: '-- +migrate %s' cannot be nested inside a '%s' block", cmd, outer.label())
}

// scanSQLLine updates the single-quote string and block-comment state while
// walking one line of SQL. Both constructs may span several lines, so the
// returned state has to be fed back in for the next line. Line comments
// ("-- ...") terminate at the end of the line and do not change the state.
//
// Tracking this state is what prevents directive-looking lines that happen to
// appear inside string literals or block comments from being mistaken for
// migration commands.
func scanSQLLine(line string, inString, inBlockComment bool) (bool, bool) {
	i := 0
	for i < len(line) {
		switch {
		case inBlockComment:
			if i+1 < len(line) && line[i] == '*' && line[i+1] == '/' {
				inBlockComment = false
				i += 2
				continue
			}
			i++
		case inString:
			if line[i] == '\'' {
				// A doubled single quote is an escaped quote inside a string.
				if i+1 < len(line) && line[i+1] == '\'' {
					i += 2
					continue
				}
				inString = false
			}
			i++
		default:
			if i+1 < len(line) && line[i] == '-' && line[i+1] == '-' {
				// The remainder of the line is a line comment.
				return inString, inBlockComment
			}
			if i+1 < len(line) && line[i] == '/' && line[i+1] == '*' {
				inBlockComment = true
				i += 2
				continue
			}
			if line[i] == '\'' {
				inString = true
			}
			i++
		}
	}

	return inString, inBlockComment
}

func (p *ParsedMigration) addAssertion(dir migrationDirection, mode blockMode, assertion Assertion) {
	switch dir {
	case directionUp:
		if mode == blockAssertBefore {
			p.AssertBeforeUp = append(p.AssertBeforeUp, assertion)
		} else {
			p.AssertAfterUp = append(p.AssertAfterUp, assertion)
		}
	case directionDown:
		if mode == blockAssertBefore {
			p.AssertBeforeDown = append(p.AssertBeforeDown, assertion)
		} else {
			p.AssertAfterDown = append(p.AssertAfterDown, assertion)
		}
	}
}

// Split the given sql script into individual statements.
//
// The base case is to simply split on semicolons, as these
// naturally terminate a statement.
//
// However, more complex cases like pl/pgsql can have semicolons
// within a statement. For these cases, we provide the explicit annotations
// 'StatementBegin' and 'StatementEnd' to allow the script to
// tell us to ignore semicolons.
//
// Assertion checks are delimited by the independent
// 'AssertBeforeBegin'/'AssertBeforeEnd' and
// 'AssertAfterBegin'/'AssertAfterEnd' markers. Their content is captured
// verbatim and is never split on semicolons.
func ParseMigration(r io.ReadSeeker) (*ParsedMigration, error) {
	p := &ParsedMigration{}

	_, err := r.Seek(0, 0)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	var assertBuf bytes.Buffer
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	statementEnded := false
	ignoreSemicolons := false
	currentDirection := directionNone
	mode := blockNone
	assertName := ""
	inString := false
	inBlockComment := false
	assertNames := map[migrationDirection]map[string]struct{}{
		directionUp:   {},
		directionDown: {},
	}

	for scanner.Scan() {
		line := scanner.Text()
		lineStartsInString := inString
		lineStartsInBlockComment := inBlockComment
		isCommand := !inString && !inBlockComment && strings.HasPrefix(line, sqlCmdPrefix)

		if !isCommand {
			inString, inBlockComment = scanSQLLine(line, inString, inBlockComment)
		}

		// handle any migrate-specific commands
		if isCommand {
			cmd, err := parseCommand(line)
			if err != nil {
				return nil, err
			}

			switch cmd.Command {
			case "Up":
				if mode != blockNone {
					return nil, fmt.Errorf("ERROR: '-- +migrate Up' found before '%s' block was closed", mode.label())
				}
				if len(strings.TrimSpace(buf.String())) > 0 {
					return nil, errNoTerminator()
				}
				currentDirection = directionUp
				if cmd.HasOption(optionNoTransaction) {
					p.DisableTransactionUp = true
				}

			case "Down":
				if mode != blockNone {
					return nil, fmt.Errorf("ERROR: '-- +migrate Down' found before '%s' block was closed", mode.label())
				}
				if len(strings.TrimSpace(buf.String())) > 0 {
					return nil, errNoTerminator()
				}
				currentDirection = directionDown
				if cmd.HasOption(optionNoTransaction) {
					p.DisableTransactionDown = true
				}

			case statementBeginCmd:
				if currentDirection != directionNone {
					if mode != blockNone {
						return nil, errNestedBlock(statementBeginCmd, mode)
					}
					ignoreSemicolons = true
					mode = blockStatement
				}

			case statementEndCmd:
				if currentDirection != directionNone {
					if mode != blockStatement {
						return nil, fmt.Errorf("ERROR: '-- +migrate StatementEnd' without matching '-- +migrate StatementBegin'")
					}
					statementEnded = true
					ignoreSemicolons = false
					mode = blockNone
				}

			case assertBeforeBeginCmd, assertAfterBeginCmd:
				if currentDirection == directionNone {
					return nil, fmt.Errorf("ERROR: '-- +migrate %s' must be used inside an Up or Down section", cmd.Command)
				}
				if mode != blockNone {
					return nil, errNestedBlock(cmd.Command, mode)
				}

				name, err := singleAssertionName(cmd)
				if err != nil {
					return nil, err
				}
				if _, exists := assertNames[currentDirection][name]; exists {
					return nil, fmt.Errorf("ERROR: duplicate assertion name %q in %s section",
						name, currentDirection.label())
				}
				assertNames[currentDirection][name] = struct{}{}

				assertName = name
				assertBuf.Reset()
				if cmd.Command == assertBeforeBeginCmd {
					mode = blockAssertBefore
				} else {
					mode = blockAssertAfter
				}

			case assertBeforeEndCmd, assertAfterEndCmd:
				expected := blockAssertBefore
				beginMarker := assertBeforeBeginCmd
				if cmd.Command == assertAfterEndCmd {
					expected = blockAssertAfter
					beginMarker = assertAfterBeginCmd
				}
				if mode != expected {
					return nil, fmt.Errorf("ERROR: '-- +migrate %s' without matching '-- +migrate %s'",
						cmd.Command, beginMarker)
				}

				statement := strings.TrimSpace(assertBuf.String())
				if statement == "" {
					return nil, fmt.Errorf("ERROR: assertion %q must contain a check SQL statement", assertName)
				}
				p.addAssertion(currentDirection, mode, Assertion{
					Name:      assertName,
					Statement: statement,
				})
				mode = blockNone
				assertName = ""
				assertBuf.Reset()
			}
		}

		if currentDirection == directionNone {
			continue
		}

		// Drop ordinary comment lines outside of strings and block comments,
		// preserving historical behavior. Inside a string or block comment
		// such lines are SQL content and must be buffered.
		if !isCommand && !lineStartsInString && !lineStartsInBlockComment &&
			strings.HasPrefix(line, "-- ") && !strings.HasPrefix(line, "-- +") {
			continue
		}

		if mode == blockAssertBefore || mode == blockAssertAfter {
			// Directive lines delimit blocks and are never block content.
			// Directive-looking lines inside strings or block comments are
			// not commands and get captured verbatim instead.
			if !isCommand {
				if _, err := assertBuf.WriteString(line + "\n"); err != nil {
					return nil, err
				}
			}
			continue
		}

		isLineSeparator := !ignoreSemicolons && len(LineSeparator) > 0 && line == LineSeparator

		if !isLineSeparator && !(strings.HasPrefix(line, "-- +") && !lineStartsInString && !lineStartsInBlockComment) {
			if _, err := buf.WriteString(line + "\n"); err != nil {
				return nil, err
			}
		}

		// Wrap up the two supported cases: 1) basic with semicolon; 2) psql statement
		// Lines that end with semicolon that are in a statement block
		// do not conclude statement.
		if (!ignoreSemicolons && (endsWithSemicolon(line) || isLineSeparator)) || statementEnded {
			statementEnded = false
			switch currentDirection {
			case directionUp:
				p.UpStatements = append(p.UpStatements, buf.String())

			case directionDown:
				p.DownStatements = append(p.DownStatements, buf.String())

			default:
				panic("impossible state")
			}

			buf.Reset()
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// diagnose likely migration script errors
	if ignoreSemicolons {
		return nil, fmt.Errorf("ERROR: saw '-- +migrate StatementBegin' with no matching '-- +migrate StatementEnd'")
	}

	switch mode {
	case blockStatement:
		return nil, fmt.Errorf("ERROR: saw '-- +migrate StatementBegin' with no matching '-- +migrate StatementEnd'")
	case blockAssertBefore, blockAssertAfter:
		return nil, fmt.Errorf("ERROR: saw '-- +migrate %s %s' with no matching end marker",
			assertBeginMarker(mode), assertName)
	}

	if currentDirection == directionNone {
		return nil, fmt.Errorf(`ERROR: no Up/Down annotations found, so no statements were executed.
			See https://github.com/rubenv/sql-migrate for details.`)
	}

	// allow comment without sql instruction. Example:
	// -- +migrate Down
	// -- nothing to downgrade!
	if len(strings.TrimSpace(buf.String())) > 0 && !strings.HasPrefix(buf.String(), "-- +") {
		return nil, errNoTerminator()
	}

	return p, nil
}

func assertBeginMarker(mode blockMode) string {
	if mode == blockAssertAfter {
		return assertAfterBeginCmd
	}
	return assertBeforeBeginCmd
}
