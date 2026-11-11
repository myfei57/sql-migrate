package sqlparse

import (
	"fmt"
	"io"
	"strings"
)

// IncludeFragmentCommand is the optional migration annotation that pulls the
// statements of a reusable SQL fragment into the current migration at the
// position of the annotation.
const IncludeFragmentCommand = "IncludeFragment"

// SourceRef identifies a position inside a named SQL source file.
type SourceRef struct {
	// File is the logical name of the root migration or the resolved path of
	// a fragment relative to the configured fragment root.
	File string
	// Line is the 1-based line number.
	Line int
	// Column is the 1-based column number.
	Column int
}

// IncludeFrame records one level of an IncludeFragment call chain.
type IncludeFrame struct {
	// Path is the resolved fragment path relative to the fragment root.
	Path string
	// Ref is the fragment reference as authored in the directive.
	Ref string
	// File is the file containing the IncludeFragment directive.
	File string
	// Line is the 1-based line number of the directive in File.
	Line int
	// Digest is the sha256 digest of the included fragment content.
	Digest string
}

// StatementSource records where a parsed statement originated.
type StatementSource struct {
	// Ref points at the first line of the statement in its originating file.
	Ref SourceRef
	// Chain is the root-first include chain of the originating file. It is
	// empty for statements written directly in the root migration.
	Chain []IncludeFrame
}

// Fragment is one resolved reusable SQL fragment.
type Fragment struct {
	// Path is the resolved fragment path relative to the fragment root.
	Path string
	// Content are the raw fragment bytes.
	Content []byte
	// Digest is the sha256 hex digest of Content.
	Digest string
}

// FragmentResolver loads fragment content for IncludeFragment directives.
//
// Implementations are expected to enforce the fragment namespace, inclusion
// depth, cycle and cumulative size limits, and to hand back the same fixed
// content for repeated calls within one load.
type FragmentResolver interface {
	// Resolve opens the fragment referenced by ref from the include site.
	// chain holds the frames of the inclusion that leads to the include site.
	Resolve(ref string, site SourceRef, chain []IncludeFrame) (Fragment, error)
}

// IncludeConfig configures include-aware migration parsing.
type IncludeConfig struct {
	// Resolver loads IncludeFragment targets. It must not be nil.
	Resolver FragmentResolver
	// File is the logical name of the root migration, used for source refs
	// and error messages.
	File string
}

// Error describes a parsing failure together with the originating file,
// position and IncludeFragment call chain.
type Error struct {
	// File is the file in which the failure was detected.
	File string
	// Line is the 1-based line number, zero when not tied to a line.
	Line int
	// Column is the 1-based column number, zero when not tied to a column.
	Column int
	// Chain is the root-first include chain active at the failure site.
	Chain []IncludeFrame
	// Err is the underlying error.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	position := e.File
	if e.Line > 0 {
		position = fmt.Sprintf("%s:%d", position, e.Line)
		if e.Column > 0 {
			position = fmt.Sprintf("%s:%d", position, e.Column)
		}
	}
	fmt.Fprintf(&b, "%s: %s", position, e.Err.Error())
	if len(e.Chain) > 0 {
		fmt.Fprintf(&b, " (included via %s)", formatChain(e.Chain))
	}
	return b.String()
}

func (e *Error) Unwrap() error {
	return e.Err
}

func formatChain(chain []IncludeFrame) string {
	parts := make([]string, 0, len(chain))
	for _, frame := range chain {
		parts = append(parts, fmt.Sprintf("%s:%d", frame.File, frame.Line))
	}
	return strings.Join(parts, " -> ")
}

// lexState is the lexical state carried between physical lines.
type lexState int

const (
	lexNormal lexState = iota
	lexLineComment
	lexBlockComment
	lexSingleString
	lexDoubleString
	lexDollarString
)

// lexLine is a physical line annotated with the lexical state at its start
// and end.
type lexLine struct {
	text         string
	number       int
	startsNormal bool
	endsNormal   bool
}

// lexLines splits content into physical lines while tracking SQL lexical
// state (single quoted strings, double quoted identifiers, dollar quoted
// strings and potentially nested block comments) across line boundaries.
func lexLines(content []byte) []lexLine {
	rawLines := strings.Split(string(content), "\n")
	lines := make([]lexLine, 0, len(rawLines))

	state := lexNormal
	blockDepth := 0
	dollarTags := make([]string, 0)

	for i, raw := range rawLines {
		line := strings.TrimSuffix(raw, "\r")
		lines = append(lines, lexLine{
			text:         line,
			number:       i + 1,
			startsNormal: state == lexNormal,
		})
		current := &lines[len(lines)-1]

		for pos := 0; pos < len(line); {
			switch state {
			case lexNormal:
				switch {
				case pos+1 < len(line) && line[pos] == '-' && line[pos+1] == '-':
					state = lexLineComment
					pos += 2
				case pos+1 < len(line) && line[pos] == '/' && line[pos+1] == '*':
					state = lexBlockComment
					blockDepth = 1
					pos += 2
				case line[pos] == '\'':
					state = lexSingleString
					pos++
				case line[pos] == '"':
					state = lexDoubleString
					pos++
				case line[pos] == '$':
					if tag, advance, ok := readDollarTag(line[pos:]); ok {
						dollarTags = append(dollarTags, tag)
						state = lexDollarString
						pos += advance
					} else {
						pos++
					}
				default:
					pos++
				}
			case lexLineComment:
				// A line comment ends at the end of the physical line.
				pos = len(line)
			case lexBlockComment:
				switch {
				case pos+1 < len(line) && line[pos] == '/' && line[pos+1] == '*':
					blockDepth++
					pos += 2
				case pos+1 < len(line) && line[pos] == '*' && line[pos+1] == '/':
					blockDepth--
					pos += 2
					if blockDepth == 0 {
						state = lexNormal
					}
				default:
					pos++
				}
			case lexSingleString:
				if line[pos] == '\'' {
					if pos+1 < len(line) && line[pos+1] == '\'' {
						pos += 2 // escaped doubling ''
					} else if oddBackslashes(line, pos) {
						pos++ // escaped quote in an E'...' string
					} else {
						state = lexNormal
						pos++
					}
				} else {
					pos++
				}
			case lexDoubleString:
				if line[pos] == '"' {
					if pos+1 < len(line) && line[pos+1] == '"' {
						pos += 2 // escaped doubling ""
					} else {
						state = lexNormal
						pos++
					}
				} else {
					pos++
				}
			case lexDollarString:
				if line[pos] == '$' {
					if tag, advance, ok := readDollarTag(line[pos:]); ok && tag == dollarTags[len(dollarTags)-1] {
						dollarTags = dollarTags[:len(dollarTags)-1]
						pos += advance
						if len(dollarTags) == 0 {
							state = lexNormal
						}
					} else {
						pos += advance
					}
				} else {
					pos++
				}
			}
		}

		// Line comments finish at the line boundary; the other states carry
		// over to the next physical line.
		if state == lexLineComment {
			state = lexNormal
		}
		current.endsNormal = state == lexNormal
	}

	return lines
}

// readDollarTag reads a PostgreSQL dollar quote opener ($tag$ or $$) at the
// start of s. It returns the tag (possibly empty), the consumed byte count and
// whether s actually starts with a valid dollar quote.
func readDollarTag(s string) (tag string, advance int, ok bool) {
	if s[0] != '$' {
		return "", 0, false
	}
	end := strings.IndexByte(s[1:], '$')
	if end < 0 {
		return "", 0, false
	}
	tag = s[1 : 1+end]
	if !isDollarTag(tag) {
		return "", 0, false
	}
	return tag, end + 2, true
}

func isDollarTag(tag string) bool {
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// oddBackslashes reports whether the quote at pos is preceded by an odd number
// of backslashes, which makes it an escaped quote in PostgreSQL E'...' strings.
func oddBackslashes(line string, pos int) bool {
	count := 0
	for i := pos - 1; i >= 0 && line[i] == '\\'; i-- {
		count++
	}
	return count%2 == 1
}

type bufferedLine struct {
	text string
	ref  SourceRef
}

type includeParser struct {
	out      *ParsedMigration
	resolver FragmentResolver
}

// ParseMigrationWithIncludes behaves like ParseMigration but expands
// IncludeFragment annotations through the configured resolver.
//
// Annotations are only honored on directive lines that start at the lexical
// top level of the SQL script and outside of StatementBegin/StatementEnd
// blocks: same-named text inside strings, dollar quotes, block comments or
// procedure bodies is preserved verbatim. All fragment validation happens
// before parsing returns, i.e. before any SQL is executed.
func ParseMigrationWithIncludes(r io.Reader, cfg IncludeConfig) (*ParsedMigration, error) {
	if cfg.Resolver == nil {
		return nil, fmt.Errorf("IncludeConfig.Resolver must not be nil")
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	p := &includeParser{
		out:      &ParsedMigration{},
		resolver: cfg.Resolver,
	}
	if err := p.parseFile(cfg.File, content, nil, false, directionNone); err != nil {
		return nil, err
	}
	return p.out, nil
}

func fail(ref SourceRef, chain []IncludeFrame, format string, args ...any) error {
	return &Error{
		File:   ref.File,
		Line:   ref.Line,
		Column: ref.Column,
		Chain:  cloneChain(chain),
		Err:    fmt.Errorf(format, args...),
	}
}

func cloneChain(chain []IncludeFrame) []IncludeFrame {
	if len(chain) == 0 {
		return nil
	}
	clone := make([]IncludeFrame, len(chain))
	copy(clone, chain)
	return clone
}

// parseFile parses one root migration (fragment == false) or one fragment
// (fragment == true) and appends its statements to the shared result.
func (p *includeParser) parseFile(name string, content []byte, chain []IncludeFrame, fragment bool, initialDirection migrationDirection) error {
	lines := lexLines(content)

	buf := make([]bufferedLine, 0)
	statementEnded := false
	ignoreSemicolons := false
	currentDirection := initialDirection

	flushStatement := func(ref SourceRef) {
		var b strings.Builder
		for _, line := range buf {
			b.WriteString(line.text)
		}
		source := StatementSource{
			Ref:   ref,
			Chain: cloneChain(chain),
		}
		switch currentDirection {
		case directionUp:
			p.out.UpStatements = append(p.out.UpStatements, b.String())
			p.out.UpSources = append(p.out.UpSources, source)
		case directionDown:
			p.out.DownStatements = append(p.out.DownStatements, b.String())
			p.out.DownSources = append(p.out.DownSources, source)
		default:
			panic("impossible state")
		}
		buf = make([]bufferedLine, 0)
	}

	bufTrimSpace := func() string {
		var b strings.Builder
		for _, line := range buf {
			b.WriteString(line.text)
		}
		return strings.TrimSpace(b.String())
	}

	for _, line := range lines {
		ref := SourceRef{File: name, Line: line.number, Column: 1}
		text := line.text

		// Directive lines are only recognized when the line starts at the
		// lexical top level: directive-looking text inside strings, dollar
		// quotes or block comments is ordinary SQL text.
		isDirective := line.startsNormal && strings.HasPrefix(text, sqlCmdPrefix)

		// Ordinary top-level comment lines do not contribute to statements.
		if line.startsNormal && strings.HasPrefix(text, "-- ") && !strings.HasPrefix(text, "-- +") {
			continue
		}

		if isDirective {
			cmd, err := parseCommand(text)
			if err != nil {
				return fail(ref, chain, "%s", err.Error())
			}

			switch cmd.Command {
			case "Up", "Down":
				if fragment {
					return fail(ref, chain, "fragments must not contain Up or Down directives")
				}
				if len(bufTrimSpace()) > 0 {
					return fail(ref, chain, "%s", errNoTerminator().Error())
				}
				if cmd.Command == "Up" {
					currentDirection = directionUp
				} else {
					currentDirection = directionDown
				}
				if cmd.HasOption(optionNoTransaction) {
					if currentDirection == directionUp {
						p.out.DisableTransactionUp = true
					} else {
						p.out.DisableTransactionDown = true
					}
				}
			case "StatementBegin":
				if currentDirection != directionNone {
					ignoreSemicolons = true
				}
			case "StatementEnd":
				if currentDirection != directionNone {
					statementEnded = ignoreSemicolons
					ignoreSemicolons = false
				}
			case IncludeFragmentCommand:
				// Inside a procedure body the annotation is not expanded: it
				// stays an ordinary directive comment and never reaches the
				// statement buffer, mirroring legacy directive handling.
				if ignoreSemicolons {
					continue
				}
				if currentDirection == directionNone {
					return fail(ref, chain, "%s is only allowed inside an Up or Down section", IncludeFragmentCommand)
				}
				if len(bufTrimSpace()) > 0 {
					return fail(ref, chain, "%s is only allowed at a statement boundary: %s", IncludeFragmentCommand, errNoTerminator().Error())
				}
				if len(cmd.Options) != 1 {
					return fail(ref, chain, "%s expects exactly one fragment path", IncludeFragmentCommand)
				}
				frag, err := p.resolver.Resolve(cmd.Options[0], ref, chain)
				if err != nil {
					return fail(ref, chain, "resolving %s %q: %w", IncludeFragmentCommand, cmd.Options[0], err)
				}
				frame := IncludeFrame{
					Path:   frag.Path,
					Ref:    cmd.Options[0],
					File:   name,
					Line:   line.number,
					Digest: frag.Digest,
				}
				childChain := make([]IncludeFrame, 0, len(chain)+1)
				childChain = append(childChain, chain...)
				childChain = append(childChain, frame)
				if err := p.parseFile(frag.Path, frag.Content, childChain, true, currentDirection); err != nil {
					return err
				}
			default:
				// Unknown commands are ignored for backwards compatibility.
			}
		}

		if currentDirection == directionNone {
			continue
		}

		// Line separators only act at the lexical top level: a matching line
		// inside a string or comment is ordinary SQL text.
		isLineSeparator := line.startsNormal && !ignoreSemicolons && len(LineSeparator) > 0 && text == LineSeparator

		// Directive-looking text on lines that do not start at the lexical
		// top level lives inside a string, dollar quote or block comment and
		// must be preserved verbatim.
		if !isLineSeparator && (!line.startsNormal || !strings.HasPrefix(text, "-- +")) {
			buf = append(buf, bufferedLine{
				text: text + "\n",
				ref:  SourceRef{File: name, Line: line.number, Column: firstNonSpaceColumn(text)},
			})
		}

		// Semicolons inside strings, dollar quotes or block comments never
		// terminate statements. A line that opens inside such a construct but
		// returns to the lexical top level (e.g. a closing "$tag$;") still
		// terminates normally.
		topLevelLine := line.startsNormal || line.endsNormal
		if topLevelLine && ((!ignoreSemicolons && (endsWithSemicolon(text) || isLineSeparator)) || statementEnded) {
			statementEnded = false
			firstRef := ref
			if len(buf) > 0 {
				firstRef = buf[0].ref
			}
			flushStatement(firstRef)
		}
	}

	if ignoreSemicolons {
		return fail(SourceRef{File: name, Line: lines[len(lines)-1].number, Column: 1}, chain,
			"saw '-- +migrate StatementBegin' with no matching '-- +migrate StatementEnd'")
	}

	if !fragment && currentDirection == directionNone {
		return fail(SourceRef{File: name}, chain,
			"no Up/Down annotations found, so no statements were executed.")
	}

	if len(bufTrimSpace()) > 0 && !strings.HasPrefix(bufTrimSpace(), "-- +") {
		lastRef := refOfLastBufferedLine(buf, name)
		return fail(lastRef, chain, "%s", errNoTerminator().Error())
	}

	return nil
}

func refOfLastBufferedLine(buf []bufferedLine, fallbackFile string) SourceRef {
	if len(buf) == 0 {
		return SourceRef{File: fallbackFile}
	}
	return buf[len(buf)-1].ref
}

func firstNonSpaceColumn(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return i + 1
		}
	}
	return 1
}
