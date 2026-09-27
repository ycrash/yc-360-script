package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// xidAgeColumns is two columns. datname is the key across samples: it is unique
// in pg_database.
var xidAgeColumns = []string{
	"datname",
	"xid_age",
}

// xidAgeSQL is how far each database is from transaction-ID wraparound. Every
// database, uncapped: pg_database holds one row per database and any role can
// read it.
const xidAgeSQL = `SELECT datname::text, age(datfrozenxid) AS xid_age
FROM pg_catalog.pg_database
ORDER BY xid_age DESC`

// XIDAge captures each database's transaction-ID age every sample. How close is
// too close is the server's call.
type XIDAge struct {
	// Interval is the cadence, the run's normal speed (its frequency). Zero is the bookend alone.
	Interval time.Duration
}

func (x XIDAge) Artifact() Artifact {
	return Artifact{
		Name:     "pg_xid_age",
		FileName: "pg_xid_age.txt",
		Scope:    "cluster",
		Schedule: Periodic(x.Interval),

		// One statement, not DefaultSampleBudget's two: Periodic's last sample is
		// the close, and the shared tick is sized from this.
		SampleBudget: StatementTimeout,
	}
}

// Sample runs the one statement and writes one block. A statement that fails
// errors and writes nothing, and the window writes the stub.
func (x XIDAge) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, err := readXIDAges(ctx, q)
	if err != nil {
		return err
	}

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the catalog read; the window's own blocks name the artifact.
	if err := writeBlockHeader(&block, "pg_database", x.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, xidAgeColumns, xidAgeCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

type xidAgeRow struct {
	datName string
	xidAge  *int64
}

func readXIDAges(ctx context.Context, q RowQuerier) ([]xidAgeRow, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, xidAgeSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []xidAgeRow

	for rows.Next() {
		var row xidAgeRow

		if err := rows.Scan(&row.datName, &row.xidAge); err != nil {
			return nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return collected, nil
}

func xidAgeCells(rows []xidAgeRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{row.datName, int64Text(row.xidAge)}
	}

	return cells
}
