package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-gorp/gorp/v3"

	"github.com/rubenv/sql-migrate/sqlparse"
)

// AssertionPhase identifies when an assertion check is evaluated relative to
// the statements of a migration.
type AssertionPhase string

const (
	// AssertBefore marks checks running before any migration statement.
	AssertBefore AssertionPhase = "before"
	// AssertAfter marks checks running after the migration statements, but
	// before the migration record is written (and before commit for
	// transactional migrations).
	AssertAfter AssertionPhase = "after"
)

// AssertionTimeout bounds the execution time of a single assertion check.
// It is applied on top of the context passed to the migration runner, so an
// earlier parent deadline or cancellation still wins. Adjust this variable
// before running migrations if checks legitimately need more time.
var AssertionTimeout = 30 * time.Second

// assertionMaxRows caps how many rows are consumed while evaluating an
// assertion. A valid check returns exactly one row, so observing a second one
// already proves the result has an invalid shape and no more rows are read.
const assertionMaxRows = 2

var (
	errAssertionNoRows = errors.New("assertion check returned no rows")
	errAssertionMulti  = errors.New("assertion check returned more than one row")
	errAssertionNull   = errors.New("assertion check returned NULL")
	errAssertionFalse  = errors.New("assertion check returned false")
)

// AssertionError is returned when an assertion check query fails or does not
// return exactly one row whose single column is a non-NULL boolean true. It
// always carries the migration Id and the assertion name so failures can be
// located precisely.
//
// For notransaction migrations, an after-assertion failure sets Partial: the
// migration statements have already executed outside of a transaction and
// cannot be rolled back automatically, and no success record is written.
type AssertionError struct {
	Migration *Migration
	Name      string
	Phase     AssertionPhase
	Reason    error

	// Partial is true when migration changes may already have been committed
	// and were not rolled back. This only happens for after-assertion
	// failures of notransaction migrations.
	Partial bool
}

func (e *AssertionError) Error() string {
	msg := fmt.Sprintf("assertion %q %s migration %s failed: %s",
		e.Name, e.Phase, e.Migration.Id, e.Reason)
	if e.Partial {
		msg += "; migration changes were already applied and were not rolled back, " +
			"no success record was written, inspect and repair the database state " +
			"manually instead of re-running the migration automatically"
	}
	return msg
}

// Unwrap exposes the underlying reason so errors.Is/As keep working.
func (e *AssertionError) Unwrap() error {
	return e.Reason
}

func newAssertionError(migration *PlannedMigration, phase AssertionPhase, assertion sqlparse.Assertion, reason error) *AssertionError {
	return &AssertionError{
		Migration: migration.Migration,
		Name:      assertion.Name,
		Phase:     phase,
		Reason:    reason,
	}
}

// evaluateAssertion runs one check query. The query must return exactly one
// row containing exactly one non-NULL value that scans to boolean true; any
// other outcome (no rows, several rows, several columns, NULL, false or a
// query error) is returned as an error.
//
// The check runs through the given gorp executor, which is either the
// migration transaction or the database handle, so transactional assertions
// observe uncommitted migration changes. A per-assertion deadline bounds the
// runtime and the number of rows read is capped; rows are always checked for
// errors and closed so connections are released.
func evaluateAssertion(ctx context.Context, executor gorp.SqlExecutor, assertion sqlparse.Assertion) error {
	queryCtx, cancel := context.WithTimeout(ctx, AssertionTimeout)
	defer cancel()

	// gorp routes this through QueryContext/Query when a context is present,
	// propagating cancellation and deadlines while reusing the transaction.
	rows, err := executor.WithContext(queryCtx).Query(strings.TrimSpace(assertion.Statement))
	if err != nil {
		return fmt.Errorf("assertion check query error: %w", err)
	}

	columns, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return fmt.Errorf("assertion check metadata error: %w", err)
	}
	if len(columns) != 1 {
		_ = rows.Close()
		return fmt.Errorf("assertion check must return exactly one column, got %d", len(columns))
	}

	rowCount := 0
	var result sql.NullBool
	for rows.Next() {
		rowCount++
		if rowCount >= assertionMaxRows {
			// A second row already violates the one-row contract; stop reading
			// to bound the consumed result size and let Close discard the rest.
			break
		}
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return fmt.Errorf("assertion check scan error: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("assertion check row error: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("assertion check close error: %w", err)
	}

	switch {
	case rowCount == 0:
		return errAssertionNoRows
	case rowCount >= assertionMaxRows:
		return errAssertionMulti
	case !result.Valid:
		return errAssertionNull
	case !result.Bool:
		return errAssertionFalse
	}
	return nil
}

// runAssertions evaluates every assertion of one phase in source order,
// wrapping the first failure (if any) in an AssertionError.
func runAssertions(ctx context.Context, executor SqlExecutor, migration *PlannedMigration,
	phase AssertionPhase, assertions []sqlparse.Assertion) error {
	gorpExecutor, ok := executor.(gorp.SqlExecutor)
	if !ok {
		return &AssertionError{
			Migration: migration.Migration,
			Phase:     phase,
			Reason:    errors.New("assertion executor does not support context-aware queries"),
		}
	}

	for _, assertion := range assertions {
		if err := evaluateAssertion(ctx, gorpExecutor, assertion); err != nil {
			return newAssertionError(migration, phase, assertion, err)
		}
	}
	return nil
}
