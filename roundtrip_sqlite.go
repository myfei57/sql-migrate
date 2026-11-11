package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// sqliteSchemaExtractor builds normalized schema evidence from a SQLite
// database using its catalog tables and PRAGMA introspection.
type sqliteSchemaExtractor struct{}

type sqliteMasterRow struct {
	Type string
	Name string
	DDL  sql.NullString
}

// fkClause is one REFERENCES clause recovered from a CREATE TABLE statement.
type fkClause struct {
	column     string
	target     string
	deferrable string
}

const (
	sqliteAutoindexPrefix = "sqlite_autoindex_"
	sqliteInternalPrefix  = "sqlite_"
)

func (sqliteSchemaExtractor) extract(ctx context.Context, db *sql.DB, migrationTable string) (*SchemaSnapshot, error) {
	snapshot := &SchemaSnapshot{Dialect: "sqlite3"}
	snapshot.Normalizations = []string{
		"sqlite:autoindex-name: unstable sqlite_autoindex_<n> suffix replaced by a deterministic structural name; origin, uniqueness, partiality and key columns are retained",
		"sqlite:catalog-order: tables and indexes are compared by name; table column ordinal and index key ordinal are retained",
	}

	masterRows, err := querySQLiteMaster(ctx, db)
	if err != nil {
		return nil, err
	}

	userTables := make([]sqliteMasterRow, 0)
	for _, row := range masterRows {
		switch {
		case row.Name == migrationTable:
			// Migration bookkeeping table owned by the runner; never user schema.
			continue
		case strings.HasPrefix(row.Name, sqliteInternalPrefix) &&
			!strings.HasPrefix(row.Name, sqliteAutoindexPrefix):
			snapshot.InternalObjects = append(snapshot.InternalObjects, NamedObject{
				Type: row.Type, Name: row.Name,
			})
		case row.Type == "table":
			userTables = append(userTables, row)
		case row.Type == "index":
			// Indexes are reconstructed from PRAGMA index_list below, including
			// the auto-indexes hidden behind the sqlite_autoindex_* names.
			continue
		default:
			snapshot.UnsupportedObjects = append(snapshot.UnsupportedObjects, NamedObject{
				Type: row.Type, Name: row.Name,
			})
		}
	}

	sort.Slice(userTables, func(i, j int) bool { return userTables[i].Name < userTables[j].Name })

	for _, row := range userTables {
		table, err := describeSQLiteTable(ctx, db, row)
		if err != nil {
			return nil, err
		}
		snapshot.Tables = append(snapshot.Tables, table)
		indexes, err := describeSQLiteIndexes(ctx, db, row.Name)
		if err != nil {
			return nil, err
		}
		snapshot.Indexes = append(snapshot.Indexes, indexes...)
	}

	sort.Slice(snapshot.Tables, func(i, j int) bool {
		return snapshot.Tables[i].Name < snapshot.Tables[j].Name
	})
	sort.Slice(snapshot.Indexes, func(i, j int) bool {
		left := snapshot.Indexes[i].Table + "." + snapshot.Indexes[i].Name
		right := snapshot.Indexes[j].Table + "." + snapshot.Indexes[j].Name
		return left < right
	})
	sort.Slice(snapshot.InternalObjects, func(i, j int) bool {
		return snapshot.InternalObjects[i].Name < snapshot.InternalObjects[j].Name
	})
	sort.Slice(snapshot.UnsupportedObjects, func(i, j int) bool {
		return snapshot.UnsupportedObjects[i].Name < snapshot.UnsupportedObjects[j].Name
	})
	return snapshot, nil
}

func querySQLiteMaster(ctx context.Context, db *sql.DB) ([]sqliteMasterRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT type, name, sql FROM sqlite_master WHERE name NOT IN ('sqlite_schema') ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []sqliteMasterRow
	for rows.Next() {
		var row sqliteMasterRow
		if err := rows.Scan(&row.Type, &row.Name, &row.DDL); err != nil {
			return nil, fmt.Errorf("scan sqlite_master: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite_master: %w", err)
	}
	return out, nil
}

func describeSQLiteTable(ctx context.Context, db *sql.DB, master sqliteMasterRow) (TableDescriptor, error) {
	table := TableDescriptor{Name: master.Name}

	ddl := ""
	if master.DDL.Valid {
		ddl = master.DDL.String
	}
	withoutRowID, clauses := parseSQLiteCreateTable(ddl)
	table.WithoutRowID = withoutRowID

	infoRows, err := db.QueryContext(ctx,
		"PRAGMA table_info("+quoteSQLiteIdent(master.Name)+")")
	if err != nil {
		return table, fmt.Errorf("pragma table_info(%s): %w", master.Name, err)
	}
	type infoRow struct {
		cid       int
		name      string
		typ       string
		notNull   int
		dfltValue sql.NullString
		pk        int
	}
	var infos []infoRow
	for infoRows.Next() {
		var info infoRow
		if err := infoRows.Scan(&info.cid, &info.name, &info.typ, &info.notNull, &info.dfltValue, &info.pk); err != nil {
			_ = infoRows.Close()
			return table, fmt.Errorf("scan pragma table_info(%s): %w", master.Name, err)
		}
		infos = append(infos, info)
	}
	if err := infoRows.Close(); err != nil {
		return table, fmt.Errorf("close pragma table_info(%s): %w", master.Name, err)
	}

	pkByOrdinal := map[int]string{}
	for _, info := range infos {
		column := ColumnDescriptor{
			Ordinal:    info.cid,
			Name:       info.name,
			Type:       info.typ,
			NotNull:    info.notNull != 0,
			HasDefault: info.dfltValue.Valid,
		}
		if info.dfltValue.Valid {
			column.Default = info.dfltValue.String
		}
		table.Columns = append(table.Columns, column)
		if info.pk > 0 {
			pkByOrdinal[info.pk] = info.name
		}
	}
	pkOrdinals := make([]int, 0, len(pkByOrdinal))
	for ordinal := range pkByOrdinal {
		pkOrdinals = append(pkOrdinals, ordinal)
	}
	sort.Ints(pkOrdinals)
	for _, ordinal := range pkOrdinals {
		table.PrimaryKey = append(table.PrimaryKey, pkByOrdinal[ordinal])
	}

	fks, err := describeSQLiteForeignKeys(ctx, db, master.Name, clauses)
	if err != nil {
		return table, err
	}
	table.ForeignKeys = fks
	return table, nil
}

func describeSQLiteForeignKeys(ctx context.Context, db *sql.DB, table string, clauses []fkClause) ([]ForeignKeyDescriptor, error) {
	rows, err := db.QueryContext(ctx,
		"PRAGMA foreign_key_list("+quoteSQLiteIdent(table)+")")
	if err != nil {
		return nil, fmt.Errorf("pragma foreign_key_list(%s): %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	type fkRow struct {
		id       int
		seq      int
		target   string
		from     string
		to       sql.NullString
		onUpdate string
		onDelete string
		match    string
	}
	var raw []fkRow
	for rows.Next() {
		var row fkRow
		if err := rows.Scan(&row.id, &row.seq, &row.target, &row.from, &row.to,
			&row.onUpdate, &row.onDelete, &row.match); err != nil {
			return nil, fmt.Errorf("scan pragma foreign_key_list(%s): %w", table, err)
		}
		raw = append(raw, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pragma foreign_key_list(%s): %w", table, err)
	}

	clauseByKey := make(map[string]fkClause, len(clauses))
	for _, clause := range clauses {
		clauseByKey[clause.column+"->"+clause.target] = clause
	}

	byID := map[int]*ForeignKeyDescriptor{}
	var order []int
	for _, row := range raw {
		fk, ok := byID[row.id]
		if !ok {
			fk = &ForeignKeyDescriptor{ID: row.id, References: row.target}
			byID[row.id] = fk
			order = append(order, row.id)
			if clause, ok := clauseByKey[row.from+"->"+row.target]; ok {
				fk.Deferrable = clause.deferrable
			}
		}
		to := ""
		if row.to.Valid {
			to = row.to.String
		}
		fk.Columns = append(fk.Columns, ForeignKeyColumn{
			Ordinal: row.seq, From: row.from, To: to,
		})
		if fk.OnUpdate == "" {
			fk.OnUpdate = row.onUpdate
		}
		if fk.OnDelete == "" {
			fk.OnDelete = row.onDelete
		}
		if fk.Match == "" {
			fk.Match = row.match
		}
	}
	sort.Ints(order)
	out := make([]ForeignKeyDescriptor, 0, len(order))
	for _, id := range order {
		fk := byID[id]
		sort.Slice(fk.Columns, func(i, j int) bool { return fk.Columns[i].Ordinal < fk.Columns[j].Ordinal })
		out = append(out, *fk)
	}
	return out, nil
}

func describeSQLiteIndexes(ctx context.Context, db *sql.DB, table string) ([]IndexDescriptor, error) {
	rows, err := db.QueryContext(ctx,
		"PRAGMA index_list("+quoteSQLiteIdent(table)+")")
	if err != nil {
		return nil, fmt.Errorf("pragma index_list(%s): %w", table, err)
	}
	type indexRow struct {
		seq     int
		name    string
		unique  int
		origin  string
		partial int
	}
	var listed []indexRow
	for rows.Next() {
		var row indexRow
		if err := rows.Scan(&row.seq, &row.name, &row.unique, &row.origin, &row.partial); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan pragma index_list(%s): %w", table, err)
		}
		listed = append(listed, row)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close pragma index_list(%s): %w", table, err)
	}

	out := make([]IndexDescriptor, 0, len(listed))
	for _, row := range listed {
		columns, err := describeSQLiteIndexColumns(ctx, db, row.name)
		if err != nil {
			return nil, err
		}
		index := IndexDescriptor{
			Table:    table,
			Name:     row.name,
			Origin:   row.origin,
			AutoName: strings.HasPrefix(row.name, sqliteAutoindexPrefix),
			Unique:   row.unique != 0,
			Partial:  row.partial != 0,
			Columns:  columns,
		}
		if index.AutoName {
			keys := make([]string, len(columns))
			for i, column := range columns {
				keys[i] = column.Name
			}
			index.Name = "autoindex:" + table + ":" + row.origin + ":" + strings.Join(keys, ",")
		}
		out = append(out, index)
	}
	return out, nil
}

func describeSQLiteIndexColumns(ctx context.Context, db *sql.DB, index string) ([]IndexColumn, error) {
	rows, err := db.QueryContext(ctx,
		"PRAGMA index_xinfo("+quoteSQLiteIdent(index)+")")
	if err != nil {
		return nil, fmt.Errorf("pragma index_xinfo(%s): %w", index, err)
	}
	defer func() { _ = rows.Close() }()

	type xinfoRow struct {
		seqno int
		cid   int
		name  sql.NullString
		desc  int
		coll  string
		key   int
	}
	var out []IndexColumn
	for rows.Next() {
		var row xinfoRow
		if err := rows.Scan(&row.seqno, &row.cid, &row.name, &row.desc, &row.coll, &row.key); err != nil {
			return nil, fmt.Errorf("scan pragma index_xinfo(%s): %w", index, err)
		}
		if row.key == 0 {
			// Auxiliary rowid column appended to rowid-table indexes; it is not
			// part of the index key. Partial-index WHERE expressions are covered
			// by the partial flag on the index itself.
			continue
		}
		column := IndexColumn{
			Ordinal:    row.seqno,
			Descending: row.desc != 0,
			Collation:  row.coll,
			Expression: row.cid < 0,
		}
		if row.name.Valid {
			column.Name = row.name.String
		}
		out = append(out, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pragma index_xinfo(%s): %w", index, err)
	}
	return out, nil
}

// quoteSQLiteIdent renders an identifier as a quoted SQLite name.
func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// parseSQLiteCreateTable extracts WITHOUT ROWID and every REFERENCES clause
// (including its deferrability) from a CREATE TABLE statement. The parser is
// deliberately scoped: only the information SQLite PRAGMAs do not expose is
// recovered from the DDL text.
func parseSQLiteCreateTable(ddl string) (withoutRowID bool, clauses []fkClause) {
	upper := strings.ToUpper(ddl)
	withoutRowID = strings.Contains(upper, "WITHOUT ROWID")

	open := strings.Index(ddl, "(")
	if open < 0 {
		return withoutRowID, nil
	}
	close := strings.LastIndex(ddl, ")")
	if close <= open {
		return withoutRowID, nil
	}
	body := ddl[open+1 : close]
	for _, segment := range splitTopLevelSegments(body) {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" {
			continue
		}
		upperSegment := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upperSegment, "FOREIGN KEY"):
			column := extractParenIdentifier(trimmed)
			target, deferrable := extractReference(trimmed)
			if target != "" {
				clauses = append(clauses, fkClause{column: column, target: target, deferrable: deferrable})
			}
		case strings.HasPrefix(upperSegment, "CONSTRAINT"):
			if !strings.Contains(upperSegment, "REFERENCES") {
				continue
			}
			column, target, deferrable := parseInlineReference(trimmed, true)
			if target != "" {
				clauses = append(clauses, fkClause{column: column, target: target, deferrable: deferrable})
			}
		default:
			if !strings.Contains(upperSegment, "REFERENCES") {
				continue
			}
			column, target, deferrable := parseInlineReference(trimmed, false)
			if target != "" {
				clauses = append(clauses, fkClause{column: column, target: target, deferrable: deferrable})
			}
		}
	}
	return withoutRowID, clauses
}

// splitTopLevelSegments splits a CREATE TABLE body on commas that are not
// nested inside parentheses or quoted literals/identifiers.
func splitTopLevelSegments(body string) []string {
	var segments []string
	depth := 0
	var quote rune
	start := 0
	for i, r := range body {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		case r == ',' && depth == 0:
			segments = append(segments, body[start:i])
			start = i + 1
		}
	}
	segments = append(segments, body[start:])
	return segments
}

func extractParenIdentifier(segment string) string {
	open := strings.Index(segment, "(")
	if open < 0 {
		return ""
	}
	close := strings.Index(segment[open:], ")")
	if close < 0 {
		return ""
	}
	inner := strings.TrimSpace(segment[open+1 : open+close])
	parts := strings.Split(inner, ",")
	return unquoteSQLiteIdent(strings.TrimSpace(parts[0]))
}

func parseInlineReference(segment string, constraint bool) (column, target, deferrable string) {
	if constraint {
		// CONSTRAINT name FOREIGN KEY (col) REFERENCES target ...
		column = extractParenIdentifier(segment)
	} else {
		column = firstIdentifier(segment)
	}
	target, deferrable = extractReference(segment)
	return column, target, deferrable
}

func extractReference(segment string) (target, deferrable string) {
	upper := strings.ToUpper(segment)
	idx := strings.Index(upper, "REFERENCES")
	if idx < 0 {
		return "", ""
	}
	rest := strings.TrimSpace(segment[idx+len("REFERENCES"):])
	target = firstIdentifier(rest)
	deferrable = extractDeferrability(upper)
	return target, deferrable
}

func extractDeferrability(upperSegment string) string {
	switch {
	case strings.Contains(upperSegment, "INITIALLY DEFERRED"):
		return "DEFERRABLE INITIALLY DEFERRED"
	case strings.Contains(upperSegment, "INITIALLY IMMEDIATE") && strings.Contains(upperSegment, "DEFERRABLE"):
		return "DEFERRABLE INITIALLY IMMEDIATE"
	case strings.Contains(upperSegment, "NOT DEFERRABLE"):
		return "NOT DEFERRABLE"
	case strings.Contains(upperSegment, "DEFERRABLE"):
		return "DEFERRABLE"
	default:
		return ""
	}
}

func firstIdentifier(s string) string {
	s = strings.TrimLeft(s, " \t\r\n")
	end := 0
	for end < len(s) {
		r := rune(s[end])
		if r == '(' || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			break
		}
		end++
	}
	return unquoteSQLiteIdent(s[:end])
}

func unquoteSQLiteIdent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"`[]")
	return strings.ReplaceAll(s, `""`, `"`)
}
