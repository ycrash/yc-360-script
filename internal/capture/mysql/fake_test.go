package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// fakeResult is one statement's answer: rows of values, nil for NULL, or an error.
type fakeResult struct {
	columns []string
	rows    [][]any
	err     error
}

// fakeQuerier answers each statement with the first result whose key the statement
// contains, and records every statement sent.
type fakeQuerier struct {
	results []fakeMatch
	sent    []string
}

type fakeMatch struct {
	contains string
	result   fakeResult
}

func (q *fakeQuerier) on(contains string, result fakeResult) *fakeQuerier {
	q.results = append(q.results, fakeMatch{contains, result})

	return q
}

func (q *fakeQuerier) Query(_ context.Context, query string) (Rows, error) {
	q.sent = append(q.sent, query)

	for _, m := range q.results {
		if strings.Contains(query, m.contains) {
			if m.result.err != nil {
				return nil, m.result.err
			}

			return &fakeRows{columns: m.result.columns, rows: m.result.rows}, nil
		}
	}

	return nil, fmt.Errorf("unexpected statement: %s", query)
}

func (q *fakeQuerier) count(contains string) int {
	n := 0

	for _, sent := range q.sent {
		if strings.Contains(sent, contains) {
			n++
		}
	}

	return n
}

type fakeRows struct {
	columns []string
	rows    [][]any
	at      int
}

func (r *fakeRows) Columns() ([]string, error) { return r.columns, nil }

func (r *fakeRows) Next() bool {
	r.at++

	return r.at <= len(r.rows)
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.at-1]
	if len(row) != len(dest) {
		return errors.New("scan: column count")
	}

	for i, value := range row {
		target, ok := dest[i].(*sql.NullString)
		if !ok {
			return errors.New("scan: destination is not a NullString")
		}

		if value == nil {
			*target = sql.NullString{}

			continue
		}

		*target = sql.NullString{String: fmt.Sprint(value), Valid: true}
	}

	return nil
}

func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

// serverError is the error the server sends.
func serverError(number uint16, message string) error {
	return &gomysql.MySQLError{Number: number, Message: message}
}

var testAt = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

// testSample is sample index of total at testAt, on 8.4.3, statements timed 1ms apart.
func testSample(index, total int) SampleContext {
	reads := 0

	return SampleContext{
		At:            testAt,
		Index:         index,
		Total:         total,
		Database:      "orders_db",
		CaptureID:     "capture-1",
		TargetID:      "orders-primary",
		EngineVersion: "8.4.3",
		Engine:        Facts{Version: Version{8, 4, 3}, Variant: VariantMySQL},
		Clock: func() time.Time {
			reads++

			return testAt.Add(time.Duration(reads) * time.Millisecond)
		},
	}
}
