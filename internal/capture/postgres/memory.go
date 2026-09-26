package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// insufficientPrivilegeCode is the SQLSTATE of the server refusing a read to
// this role.
const insufficientPrivilegeCode = "42501"

// reasonPermissionDenied: the role may not read the view. pg_shmem_allocations
// needs pg_read_all_stats from PostgreSQL 15 (which pg_monitor includes) and
// superuser before that.
const reasonPermissionDenied = "permission_denied"

// memoryColumns is the view's four columns. name is the key across samples;
// the one row with no name is the shared memory not yet allocated.
var memoryColumns = []string{
	"name",
	"off",
	"size",
	"allocated_size",
}

// memorySQL is every named allocation in the server's shared memory, largest
// first, uncapped: the view has well under a hundred rows.
const memorySQL = `SELECT name, off, size, allocated_size
FROM pg_catalog.pg_shmem_allocations
ORDER BY allocated_size DESC`

// Memory captures the server's shared memory allocations every sample. Which
// allocation is the buffer pool, and which the engine's own, is the server's
// reading.
type Memory struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration
}

func (m Memory) Artifact() Artifact {
	return Artifact{
		Name:     "pg_memory",
		FileName: "pg_memory.txt",
		Scope:    "cluster",
		Schedule: Periodic(m.Interval),

		// One statement, not DefaultSampleBudget's two: Periodic's last sample is
		// the close, and the shared tick is sized from this.
		SampleBudget: StatementTimeout,
	}
}

// Sample runs the one statement and writes one block. A refusal is a written
// sample, reason=permission_denied with no rows, since it recurs every sample
// for this role; any other failure errors and writes nothing, and the window
// writes the stub.
func (m Memory) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, err := readMemory(ctx, q)
	if err != nil && !hasSQLState(err, insufficientPrivilegeCode) {
		return err
	}

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"reason", reasonPermissionDenied})
	}

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the view read; the window's own blocks name the artifact.
	if err := writeBlockHeader(&block, "pg_shmem_allocations", m.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, memoryColumns, memoryCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// memoryRow is one allocation. name is NULL for the unallocated remainder, and
// off for the anonymous allocations, which have no one offset.
type memoryRow struct {
	name          *string
	off           *int64
	size          *int64
	allocatedSize *int64
}

func readMemory(ctx context.Context, q RowQuerier) ([]memoryRow, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, memorySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []memoryRow

	for rows.Next() {
		var row memoryRow

		if err := rows.Scan(&row.name, &row.off, &row.size, &row.allocatedSize); err != nil {
			return nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return collected, nil
}

func memoryCells(rows []memoryRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{text(row.name), int64Text(row.off), int64Text(row.size), int64Text(row.allocatedSize)}
	}

	return cells
}
